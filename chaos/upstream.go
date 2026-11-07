package chaos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
)

// UpstreamFaults is what the upstream does wrong, changed while serving.
type UpstreamFaults struct {
	Status      int           // answer every POST with this status (5xx)
	StatusEvery int           // only every Nth POST (0: all, when Status is set)
	Latency     time.Duration // every answer takes this long to start (a busy model)
	Hang        time.Duration // block this long before any byte (or until the client leaves)
	TokenDelay  time.Duration // between two SSE events
	CutAfter    int           // abort an SSE answer after this many events (0: never)
	UsageAdd    int           // added to the reported completion tokens (wrong usage)
	UsageMul    float64       // the reported completion tokens times this (0: x1)
	NoUsage     bool          // the usage object is dropped
	ModelsDown  bool          // GET /v1/models answers 503
}

// Seen is what the upstream witnessed about one request, keyed by its marker.
type Seen struct {
	Marker     string
	Org        string
	Stream     bool
	Prompt     int // prompt words, as fakeupstream counts them
	MaxTokens  int
	Started    time.Time
	Reported   bool // a usage object left the upstream
	PromptTok  int  // the usage it reported
	ComplTok   int
	Completed  bool // the handler returned without abort
	Cut        bool
	Failed     int // status of a fault answer
	ClientGone bool
	// Concurrent is the number of requests in flight when it started.
	Concurrent int
	// Priority is the vLLM priority the gateway sent (noPriority: none).
	Priority int
	N        int
}

const noPriority = -1 << 30

// Upstream is the recording fake vLLM (bench/fakeupstream) behind a fault
// injector. It also serves vLLM's /metrics: the preemption counter and the
// waiting queue the gateway reads.
type Upstream struct {
	srv    *httptest.Server
	inner  *fakeupstream.Server
	mu     sync.Mutex
	f      UpstreamFaults
	seen   map[string]*Seen
	order  []string
	posts  atomic.Int64
	inFl   atomic.Int64
	maxFl  atomic.Int64
	pre    atomic.Int64
	wait   atomic.Int64
	models atomic.Int64 // GET /v1/models answered
	gets   atomic.Int64 // GET /v1/models received
}

var markerRE = regexp.MustCompile(`MK([A-Za-z0-9]+)-([A-Za-z0-9]+)-([0-9]+)`)

// NewUpstream starts the fake. generate is the completion tokens a request
// wants (vLLM stops at max_tokens).
func NewUpstream(generate int) *Upstream {
	u := &Upstream{seen: map[string]*Seen{}}
	u.inner = fakeupstream.NewServer(fakeupstream.Options{Model: "fake", Reply: "alpha beta gamma delta epsilon zeta eta theta", Generate: generate}, nil)
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	return u
}

// URL of the fake.
func (u *Upstream) URL() string { return u.srv.URL }

// Close stops it.
func (u *Upstream) Close() { u.srv.CloseClientConnections(); u.srv.Close() }

// Set replaces the faults.
func (u *Upstream) Set(f UpstreamFaults) { u.mu.Lock(); u.f = f; u.mu.Unlock() }

// Faults returns the current faults.
func (u *Upstream) Faults() UpstreamFaults { u.mu.Lock(); defer u.mu.Unlock(); return u.f }

// Update changes the faults through fn.
func (u *Upstream) Update(fn func(*UpstreamFaults)) { u.mu.Lock(); fn(&u.f); u.mu.Unlock() }

// PreemptionBurst bumps vLLM's preemption counter by n and reports a waiting
// queue of w until Calm.
func (u *Upstream) PreemptionBurst(n, w int64) { u.pre.Add(n); u.wait.Store(w) }

// Calm empties the waiting queue.
func (u *Upstream) Calm() { u.wait.Store(0) }

// MaxInFlight is the highest number of requests in flight at once.
func (u *Upstream) MaxInFlight() int { return int(u.maxFl.Load()) }

// Posts is the number of POSTs received.
func (u *Upstream) Posts() int { return int(u.posts.Load()) }

// ModelsRequests is the number of GET /v1/models received.
func (u *Upstream) ModelsRequests() int { return int(u.gets.Load()) }

// Seen returns the witnesses in arrival order.
func (u *Upstream) Seen() []Seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]Seen, 0, len(u.order))
	for _, k := range u.order {
		out = append(out, *u.seen[k])
	}
	return out
}

func (u *Upstream) note(s *Seen, fn func(*Seen)) {
	u.mu.Lock()
	fn(s)
	u.mu.Unlock()
}

func (u *Upstream) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		fmt.Fprintf(w, "# TYPE vllm:num_preemptions_total counter\nvllm:num_preemptions_total{engine=\"0\",model_name=\"fake\"} %d.0\n# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{engine=\"0\",model_name=\"fake\"} %d.0\n", u.pre.Load(), u.wait.Load())
		return
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		u.gets.Add(1)
		if u.Faults().ModelsDown {
			http.Error(w, "models unavailable", http.StatusServiceUnavailable)
			return
		}
		u.models.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "fake", "object": "model", "owned_by": "chaos", "max_model_len": MaxModelLen}}})
		return
	case r.Method != http.MethodPost:
		u.inner.ServeHTTP(w, r)
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	r.Body = io.NopCloser(bytes.NewReader(body))
	n := u.posts.Add(1)
	f := u.Faults()
	var req struct {
		Stream    bool            `json:"stream"`
		Messages  json.RawMessage `json:"messages"`
		MaxTokens *int            `json:"max_tokens"`
		MaxCompl  *int            `json:"max_completion_tokens"`
		Priority  *int            `json:"priority"`
		N         *int            `json:"n"`
	}
	_ = json.Unmarshal(body, &req)
	s := &Seen{Stream: req.Stream, Started: time.Now(), Priority: noPriority, N: 1}
	if req.Priority != nil {
		s.Priority = *req.Priority
	}
	if req.N != nil {
		s.N = *req.N
	}
	if m := markerRE.Find(body); m != nil {
		s.Marker = string(m)
		if sm := markerRE.FindSubmatch(m); sm != nil {
			s.Org = string(sm[1])
		}
	}
	for _, p := range []*int{req.MaxTokens, req.MaxCompl} {
		if p != nil {
			s.MaxTokens = *p
		}
	}
	cur := u.inFl.Add(1)
	defer u.inFl.Add(-1)
	for {
		old := u.maxFl.Load()
		if cur <= old || u.maxFl.CompareAndSwap(old, cur) {
			break
		}
	}
	s.Concurrent = int(cur)
	// One witness per hit: the gateway retries a failed attempt, and each hit
	// is told apart by its sequence number.
	key := s.Marker + "#" + strconv.FormatInt(n, 10)
	u.mu.Lock()
	u.seen[key] = s
	u.order = append(u.order, key)
	u.mu.Unlock()
	if f.Latency > 0 { // a model that takes time: slots stay busy
		select {
		case <-time.After(f.Latency):
		case <-r.Context().Done():
			u.note(s, func(s *Seen) { s.ClientGone = true })
			return
		}
	}
	if f.Hang > 0 {
		select {
		case <-time.After(f.Hang):
		case <-r.Context().Done():
			u.note(s, func(s *Seen) { s.ClientGone = true })
			return
		}
	}
	if f.Status != 0 && (f.StatusEvery <= 1 || int(n)%f.StatusEvery == 0) {
		u.note(s, func(s *Seen) { s.Failed = f.Status })
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.Status)
		_, _ = io.WriteString(w, `{"error":{"message":"chaos: upstream failure","type":"server_error"}}`)
		return
	}
	fw := &faultWriter{ResponseWriter: w, u: u, s: s, f: f, ctx: r.Context()}
	defer func() {
		if rec := recover(); rec != nil {
			u.note(s, func(s *Seen) { s.Cut = true })
			panic(http.ErrAbortHandler)
		}
		fw.finish()
	}()
	u.inner.ServeHTTP(fw, r)
}

// faultWriter applies the stream faults and records the usage that leaves.
type faultWriter struct {
	http.ResponseWriter
	u      *Upstream
	s      *Seen
	f      UpstreamFaults
	ctx    interface{ Done() <-chan struct{} }
	buf    bytes.Buffer // JSON answers are held, to rewrite their usage
	sse    bool
	events int
	hdr    bool
	line   bytes.Buffer
}

func (w *faultWriter) WriteHeader(code int) {
	w.hdr = true
	w.sse = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
	if w.sse {
		w.ResponseWriter.WriteHeader(code)
	} else {
		w.Header().Del("Content-Length")
	}
}

func (w *faultWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok && w.sse {
		f.Flush()
	}
}

func (w *faultWriter) Write(p []byte) (int, error) {
	if !w.hdr {
		w.WriteHeader(http.StatusOK)
	}
	if !w.sse {
		return w.buf.Write(p)
	}
	w.line.Write(p)
	for {
		b := w.line.Bytes()
		i := bytes.Index(b, []byte("\n\n"))
		if i < 0 {
			return len(p), nil
		}
		ev := append([]byte(nil), b[:i+2]...)
		w.line.Next(i + 2)
		if err := w.event(ev); err != nil {
			return len(p), err
		}
	}
}

func (w *faultWriter) event(ev []byte) error {
	if w.f.TokenDelay > 0 && w.events > 0 {
		select {
		case <-time.After(w.f.TokenDelay):
		case <-w.ctx.Done():
			w.u.note(w.s, func(s *Seen) { s.ClientGone = true })
			return io.ErrClosedPipe
		}
	}
	if data, ok := bytes.CutPrefix(bytes.TrimSpace(ev), []byte("data: ")); ok && bytes.Contains(data, []byte(`"usage"`)) {
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			if u, ok := m["usage"].(map[string]any); ok {
				w.rewrite(u)
				nb, _ := json.Marshal(m)
				ev = append(append([]byte("data: "), nb...), '\n', '\n')
			}
		}
	}
	if w.f.NoUsage {
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(ev), []byte("data: ")); ok && bytes.Contains(data, []byte(`"usage"`)) {
			return nil
		}
	}
	if _, err := w.ResponseWriter.Write(ev); err != nil {
		w.u.note(w.s, func(s *Seen) { s.ClientGone = true })
		return err
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	w.events++
	if w.f.CutAfter > 0 && w.events >= w.f.CutAfter {
		w.u.note(w.s, func(s *Seen) { s.Cut = true })
		panic(http.ErrAbortHandler)
	}
	return nil
}

// rewrite applies the wrong-usage faults to a usage object and records what
// the upstream reported.
func (w *faultWriter) rewrite(u map[string]any) {
	ct, _ := u["completion_tokens"].(float64)
	pt, _ := u["prompt_tokens"].(float64)
	c := int(ct)
	if w.f.UsageMul > 0 {
		c = int(float64(c) * w.f.UsageMul)
	}
	c += w.f.UsageAdd
	if c < 0 {
		c = 0
	}
	u["completion_tokens"] = c
	u["total_tokens"] = int(pt) + c
	w.u.note(w.s, func(s *Seen) { s.Reported, s.PromptTok, s.ComplTok = true, int(pt), c })
}

// finish writes a held JSON answer, with its usage rewritten.
func (w *faultWriter) finish() {
	if w.sse {
		w.u.note(w.s, func(s *Seen) { s.Completed = true })
		return
	}
	if w.buf.Len() == 0 {
		return
	}
	out := w.buf.Bytes()
	var m map[string]any
	if json.Unmarshal(out, &m) == nil {
		if u, ok := m["usage"].(map[string]any); ok {
			w.rewrite(u)
			if w.f.NoUsage {
				delete(m, "usage")
				w.u.note(w.s, func(s *Seen) { s.Reported = false })
			}
			out, _ = json.Marshal(m)
		}
	}
	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	if _, err := w.ResponseWriter.Write(out); err != nil {
		w.u.note(w.s, func(s *Seen) { s.ClientGone = true })
		return
	}
	w.u.note(w.s, func(s *Seen) { s.Completed = true })
}
