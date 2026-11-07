package chaos

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Phase is where a client leaves a stream.
type Phase string

// The phases at which a client may disconnect.
const (
	PhaseNever      Phase = ""
	PhaseConnect    Phase = "connect"     // before the gateway has answered anything
	PhaseHeaders    Phase = "headers"     // right after the response headers
	PhaseFirstEvent Phase = "first-event" // after the first SSE event
	PhaseMid        Phase = "mid-stream"  // after half of the events
	PhaseBeforeDone Phase = "before-done" // after the last data event, before [DONE]
	PhaseAfterDone  Phase = "after-done"  // reads everything, then drops without draining
)

// Phases lists them in order.
var Phases = []Phase{PhaseConnect, PhaseHeaders, PhaseFirstEvent, PhaseMid, PhaseBeforeDone, PhaseAfterDone}

// Cred is how a client authenticates.
type Cred struct {
	Bearer string
	Who    string // the person or key the ground truth attributes the request to
}

// Req is one planned request. Everything in it comes from the seed.
type Req struct {
	ID         int
	Org        string // the caller's organisation (ground truth)
	Team       string
	Who        string
	Cred       Cred
	Prefix     string // "" or the federation prefix
	Stream     bool
	MaxTokens  int
	N          int
	BestOf     int
	Words      int // prompt words, marker included
	Leave      Phase
	At         time.Duration // offset from the start of the run
	Retries    int           // extra attempts after a refusal (retry storm)
	Marker     string
	Attempt    int
	Group      string // a burst the request belongs to
	SkipUpstrm bool
	IdP        string // the identity provider that signed the token, "" for a key
}

// Outcome is what the client saw.
type Outcome struct {
	Req
	Status     int
	RetryAfter string
	Header     http.Header
	BodyLen    int
	Body       string // first 512 bytes
	Events     int
	GotDone    bool
	GotUsage   bool
	Err        string
	Left       bool
	Dur        time.Duration
	TTFB       time.Duration
	At         time.Time // when the request was sent
	IdP        string    // the provider that signed its token, "" for a key
}

// Do runs a request against base and returns what it saw.
func Do(ctx context.Context, hc *http.Client, base string, r Req) Outcome {
	o := Outcome{Req: r, At: time.Now(), IdP: r.IdP}
	prompt := r.Marker
	for i := 1; i < r.Words; i++ {
		prompt += " w"
	}
	body := map[string]any{"model": ModelName, "max_tokens": r.MaxTokens, "stream": r.Stream,
		"messages": []map[string]string{{"role": "user", "content": prompt}}}
	if r.Stream {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	if r.N > 1 {
		body["n"] = r.N
	}
	if r.BestOf > 1 {
		body["best_of"] = r.BestOf
	}
	bb, _ := json.Marshal(body)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodPost, base+r.Prefix+"/v1/chat/completions", strings.NewReader(string(bb)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.Cred.Bearer)
	t0 := time.Now()
	if r.Leave == PhaseConnect {
		// the cancel races the answer: whichever wins, this client has left.
		o.Left = true
		time.AfterFunc(time.Duration(r.ID%3)*time.Millisecond, cancel)
	}
	resp, err := hc.Do(req)
	if err != nil {
		o.Err = err.Error()
		o.Dur = time.Since(t0)
		return o
	}
	defer resp.Body.Close()
	o.Status, o.Header, o.TTFB = resp.StatusCode, resp.Header, time.Since(t0)
	o.RetryAfter = resp.Header.Get("Retry-After")
	if r.Leave == PhaseHeaders {
		o.Left = true
		cancel()
		o.Dur = time.Since(t0)
		return o
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		o.BodyLen = len(b)
		o.Body = string(b[:min(len(b), 512)])
		if err != nil {
			o.Err = err.Error()
		}
		o.GotUsage = strings.Contains(string(b), `"usage"`)
		o.Dur = time.Since(t0)
		return o
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var datas []string
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		o.BodyLen += len(line) + 2
		if data == "[DONE]" {
			o.GotDone = true
			break
		}
		datas = append(datas, data)
		o.Events++
		if strings.Contains(data, `"usage"`) && strings.Contains(data, `"completion_tokens"`) {
			o.GotUsage = true
		}
		switch r.Leave {
		case PhaseFirstEvent:
			if o.Events == 1 {
				o.Left = true
				cancel()
				o.Dur = time.Since(t0)
				return o
			}
		case PhaseMid:
			if o.Events >= 4 {
				o.Left = true
				cancel()
				o.Dur = time.Since(t0)
				return o
			}
		}
	}
	if err := sc.Err(); err != nil {
		o.Err = err.Error()
	}
	if r.Leave == PhaseBeforeDone || r.Leave == PhaseAfterDone {
		o.Left = true
	}
	o.Dur = time.Since(t0)
	return o
}

// NewClient is an HTTP client with no connection reuse, so that every request
// opens (and closes) its own connection and a leak shows as a file descriptor.
func NewClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, MaxIdleConns: -1}, Timeout: 60 * time.Second}
}

// Ledger collects the outcomes of a run.
type Ledger struct {
	mu  sync.Mutex
	out []Outcome
}

// Add records an outcome.
func (l *Ledger) Add(o Outcome) { l.mu.Lock(); l.out = append(l.out, o); l.mu.Unlock() }

// All returns a copy.
func (l *Ledger) All() []Outcome {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Outcome(nil), l.out...)
}

func (o Outcome) String() string {
	return fmt.Sprintf("#%d %s/%s status=%d ra=%q leave=%q stream=%v events=%d done=%v err=%q", o.ID, o.Org, o.Who, o.Status, o.RetryAfter, o.Leave, o.Stream, o.Events, o.GotDone, o.Err)
}
