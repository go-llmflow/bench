package chaos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Positive controls: scenarios that must FAIL on the gateway releases before a
// fix and PASS from it on (Scenario.ControlSince). Each is a probe run when the
// invariants are judged, and reports through invariant 6 (see authProblems).

func (s *Scenario) expects(name string) bool {
	for _, e := range s.Expect {
		if e == name {
			return true
		}
	}
	return false
}

// controlProbes runs the probes of the scenario's controls.
func (w *World) controlProbes() (problems, notes []string) {
	for _, f := range []func() ([]string, []string){w.schacSpoofProbe, w.nSlotsProbe, w.adminUsersProbe, w.spareGuardProbe, w.fallbackSpareProbe} {
		p, n := f()
		problems, notes = append(problems, p...), append(notes, n...)
	}
	return
}

// ---- control n-slots: gateway v0.38.0, "fair share counts every choice" ----

type inflightSample struct {
	At       time.Time
	InFlight float64
	Tokens   float64
}

// gaugeOf returns the value of a gateway gauge sample of the organisation.
func gaugeOf(text, name, org string) (float64, bool) {
	prefix := name + "{"
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, prefix) || !strings.Contains(l, `org="`+org+`"`) {
			continue
		}
		f := strings.Fields(l)
		if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// sampleInflight reads the gateway's in-flight gauges of etabA twice a second.
func (w *World) sampleInflight(ctx context.Context, until time.Time) {
	for time.Now().Before(until) && ctx.Err() == nil {
		if text, err := w.gatewayMetrics(); err == nil {
			n, ok1 := gaugeOf(text, "llmflow_inflight", "etabA")
			t, ok2 := gaugeOf(text, "llmflow_inflight_tokens", "etabA")
			if ok1 && ok2 {
				w.mu.Lock()
				w.samples = append(w.samples, inflightSample{At: time.Now(), InFlight: n, Tokens: t})
				w.mu.Unlock()
			}
		}
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
		}
	}
}

// nSlotsProbe: every request of etabA asks n = best_of = 4 with max_tokens 64.
// The fair-share estimate of a request in flight (prompt + max_tokens x
// max(n, best_of)) is then at least 4 x 64 = 256 tokens; v0.37.1 counted one
// choice (prompt + 64, about 75). The gauge is tokens / requests in flight.
func (w *World) nSlotsProbe() (problems, notes []string) {
	if !w.Sc.expects("n-slots") {
		return nil, nil
	}
	w.mu.Lock()
	ss := append([]inflightSample(nil), w.samples...)
	w.mu.Unlock()
	const want = 4 * 64
	var seen, low int
	var worst float64
	for _, s := range ss {
		if s.InFlight < 1 {
			continue
		}
		seen++
		if per := s.Tokens / s.InFlight; per < want {
			low++
			if worst == 0 || per < worst {
				worst = per
			}
		}
	}
	switch {
	case seen < 3:
		problems = append(problems, fmt.Sprintf("n-slots: only %d samples with a request in flight: the probe proves nothing", seen))
	case low > 0:
		problems = append(problems, fmt.Sprintf("n-slots: in %d of %d samples the estimate of an n=4 request in flight was %.0f tokens, below 4 x max_tokens = %d: the choices are not counted in the borrower's share", low, seen, worst, want))
	default:
		notes = append(notes, fmt.Sprintf("n-slots: %d samples, every n=4 request counted at least %d tokens in flight", seen, want))
	}
	return
}

// ---- control admin-users: gateway v0.38.0, "POST /admin/users is bound to the institutions" ----

// adminUsersProbe: the org administrator of etabB (whose provider idpB has the
// domain b.example) creates users through POST /admin/users.
//
//   - a bare name ("mallory") must be refused 400: once a domain is configured a
//     name is local@domain, and a bare stored user stops the next Start (v0.37.1
//     created it, 201);
//   - a name in another institution's domain ("carol@a.example", idpA's) must be
//     refused 400: v0.37.1 let any org admin pre-create another institution's
//     person, who its SCIM never managed and its restricted SSO refused;
//   - the control: a name in its own domain ("dora@b.example") is created, 201,
//     so the probe cannot pass by refusing everything;
//   - the gateway must restart on that store.
func (w *World) adminUsersProbe() (problems, notes []string) {
	if !w.Sc.expects("admin-users") {
		return nil, nil
	}
	g := w.Primary()
	const admin = "adminb@b.example"
	if code, b, err := g.Admin("POST", "/admin/users", `{"name":"`+admin+`"}`); err != nil || (code != 201 && code != 409) {
		return []string{fmt.Sprintf("admin-users: cannot create %s: %d %v %s", admin, code, err, b)}, nil
	}
	if code, b, err := g.Admin("PUT", "/admin/users/"+admin+"/roles", `{"role":"org_admin","org":"etabB"}`); err != nil || code != 200 {
		return []string{fmt.Sprintf("admin-users: cannot grant org_admin: %d %v %s", code, err, b)}, nil
	}
	code, b, err := g.Admin("POST", "/admin/users/"+admin+"/tokens", "")
	if err != nil || code != 201 {
		return []string{fmt.Sprintf("admin-users: no admin token: %d %v %s", code, err, b)}, nil
	}
	i := strings.Index(string(b), "llma-")
	if i < 0 {
		return []string{"admin-users: no llma- token in " + string(b)}, nil
	}
	tok := string(b)[i:]
	if j := strings.IndexAny(tok, `"`); j > 0 {
		tok = tok[:j]
	}
	create := func(name string) int {
		c, _, _ := g.AdminAs(tok, "POST", "/admin/users", `{"name":"`+name+`"}`)
		return c
	}
	if c := create("dora@b.example"); c != 201 {
		return []string{fmt.Sprintf("admin-users control: the org admin of etabB creating dora@b.example (its own domain) got %d (want 201): the probe proves nothing", c)}, nil
	}
	if c := create("mallory"); c != 400 {
		problems = append(problems, fmt.Sprintf("admin-users: an org admin creating the bare name mallory got %d (want 400: a name is local@domain once a domain is configured)", c))
	}
	if c := create("carol@a.example"); c != 400 {
		problems = append(problems, fmt.Sprintf("admin-users: the org admin of etabB creating carol@a.example, in idpA's domain, got %d (want 400: only the domains of its own institutions)", c))
	}
	if err := g.Stop(); err != nil {
		problems = append(problems, fmt.Sprintf("admin-users: stop: %v", err))
	} else if err := g.Start(40 * time.Second); err != nil {
		problems = append(problems, fmt.Sprintf("admin-users: the gateway does not restart on the stored users: %.300v", err))
	}
	if len(problems) == 0 {
		notes = append(notes, "admin-users: bare and foreign-domain creations refused 400, own domain 201, the gateway restarted")
	}
	return
}

// ---- the fakes the controls need ----

type guardCall struct {
	At         time.Time
	HeaderPrio string
	BodyPrio   int // noPriority when absent
}

// fakeGuard is a llama-guard model that counts the calls it receives and the
// vLLM priority each carried, and always answers "safe".
type fakeGuard struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []guardCall
}

func newFakeGuard() *fakeGuard {
	g := &fakeGuard{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"meta-llama/Llama-Guard-3-8B","object":"model","max_model_len":8192}]}`)
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			var req struct {
				Priority *int `json:"priority"`
			}
			_ = json.Unmarshal(b, &req)
			c := guardCall{At: time.Now(), HeaderPrio: r.Header.Get("X-Vllm-Priority"), BodyPrio: noPriority}
			if req.Priority != nil {
				c.BodyPrio = *req.Priority
			}
			g.mu.Lock()
			g.calls = append(g.calls, c)
			g.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"g","object":"chat.completion","model":"meta-llama/Llama-Guard-3-8B","choices":[{"index":0,"message":{"role":"assistant","content":"safe"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	return g
}

func (g *fakeGuard) Calls() []guardCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]guardCall(nil), g.calls...)
}

// setupControlFakes starts the fakes a scenario's features ask for and adds
// their configuration to the topology.
func (w *World) setupControlFakes(topo *Topology) error {
	if w.Sc.F.Guard {
		w.Guard = newFakeGuard()
		w.closers = append(w.closers, w.Guard.srv.Close)
		topo.Extra += fmt.Sprintf(`pool "guard" {
  capacity = 4
}
model "llama-guard" {
  upstream_model = "meta-llama/Llama-Guard-3-8B"
  pool = "guard"
  class = "guard"
  key_file = "@DIR@/upstream.key"
  upstream "g0" {
    base_url = %q
  }
}
guardrail "lg" {
  type = "llama_guard"
  model = "llama-guard"
  orgs = [%q]
  action = "block"
}
`, w.Guard.srv.URL, Borrower)
	}
	if w.Sc.F.Fallback {
		w.Small = NewUpstream(24)
		w.closers = append(w.closers, w.Small.Close)
		if w.Sc.UpstreamLatency > 0 {
			lat := w.Sc.UpstreamLatency
			w.Small.Update(func(f *UpstreamFaults) { f.Latency = lat })
		}
		// The borrower holds a share of the main pool too small to reserve a slot
		// (0.05 x 8 = 0.4, below the largest remainders), so it is not spare-only
		// there, every request of it borrows and waits; in the fallback pool it
		// holds none: spare-only.
		topo.PoolExtra = fmt.Sprintf("  shares = { etabA = 0.45, etabB = 0.3, etabC = 0.2, %s = 0.05 }\n", Borrower)
		topo.ModelExtra = "  fallbacks = [\"fake-small\"]\n  fallback_on = [\"unavailable\", \"saturated\"]\n  saturation_waiting = 2\n"
		topo.Extra += fmt.Sprintf(`pool "small" {
  capacity = 64
  shares = { etabA = 1 }
  borrow = "spare"
}
model "fake-small" {
  upstream_model = "fake"
  pool = "small"
  class = "interactive"
  key_file = "@DIR@/upstream.key"
  max_output_tokens = 256
  max_model_len = %d
  fallbacks = [%q]
  fallback_on = ["unavailable", "saturated"]
  saturation_waiting = 2
  upstream "n2" {
    base_url = %q
  }
}
`, MaxModelLen, ModelName, w.Small.URL())
	}
	return nil
}

func init() {
	// the fallback model's vLLM queue reports Arg waiting requests from now on
	faultTable["saturate-small"] = func(w *World, f FaultAt) (func(), error) {
		if w.Small == nil {
			return nil, fmt.Errorf("no fallback model in this scenario")
		}
		w.mu.Lock()
		w.satAt = time.Now()
		w.mu.Unlock()
		w.Small.PreemptionBurst(0, int64(f.Arg))
		return w.Small.Calm, nil
	}
}

// ---- control spare-guard: gateway v0.38.0, "a refused request costs no guard call" ----

// spareGuardProbe: the borrower (tiers, share 0, borrow = spare) has a
// llama_guard guardrail; the owners keep the pool full, so most borrower
// requests are refused 429 for lack of spare capacity. v0.37.1 ran the guard
// model for every one of them, at priority 0 (ahead of the owners' work);
// v0.38.0 takes the slot first and runs the guard, at the request's own band,
// only for a request admitted. So: guard calls <= the borrower's requests the
// upstream received (a small margin for a request abandoned in between), and no
// call at priority 0.
func (w *World) spareGuardProbe() (problems, notes []string) {
	if !w.Sc.expects("spare-guard") || w.Guard == nil {
		return nil, nil
	}
	calls := w.Guard.Calls()
	var admitted, refused int
	for _, s := range w.Up.Seen() {
		if s.Org == Borrower {
			admitted++
		}
	}
	for _, o := range w.Ledger.All() {
		if o.Org == Borrower && o.Status == 429 {
			refused++
		}
	}
	zero := 0
	for _, c := range calls {
		if c.BodyPrio == 0 || c.HeaderPrio == "0" {
			zero++
		}
	}
	switch {
	case refused < 10 || admitted < 3:
		problems = append(problems, fmt.Sprintf("spare-guard: %d borrower requests refused and %d admitted: the pool was not contended, the probe proves nothing", refused, admitted))
	case len(calls) > 2*admitted+3:
		// measured on v0.38.0: exactly two guard calls per admitted request (the
		// prompt, then the answer); on v0.37.1 one per request refused as well.
		problems = append(problems, fmt.Sprintf("spare-guard: %d guard-model calls for %d borrower requests that reached the upstream (at most 2 each: the prompt and the answer), while %d were refused 429: a refused request must cost no guard call", len(calls), admitted, refused))
	}
	if zero > 0 {
		problems = append(problems, fmt.Sprintf("spare-guard: %d guard-model calls ran at vLLM priority 0, ahead of the owners' work (want the priority of the request judged)", zero))
	}
	if len(problems) == 0 {
		notes = append(notes, fmt.Sprintf("spare-guard: %d guard calls, %d admitted, %d refused 429, none at priority 0", len(calls), admitted, refused))
	}
	return
}

// ---- control fallback-spare: gateway v0.38.0, "the saturation fallback takes its slot through the same test" ----

// fallbackSpareProbe: both vLLM queues report 5 waiting requests (saturation
// threshold 2) from satAt on; only the borrower sends traffic. Its request is
// refused on the main pool (saturated, spare-only), falls back on "saturated" to
// the small pool, whose queue is saturated too: v0.38.0 refuses it there (429),
// v0.37.1 skipped the test and admitted it: the small upstream receives it.
func (w *World) fallbackSpareProbe() (problems, notes []string) {
	if !w.Sc.expects("fallback-spare") || w.Small == nil {
		return nil, nil
	}
	w.mu.Lock()
	sat := w.satAt
	w.mu.Unlock()
	if sat.IsZero() {
		return []string{"fallback-spare: the queues were never saturated"}, nil
	}
	from := sat.Add(3 * time.Second) // the health loop scrapes every second
	var hits, admitted int
	all := w.Small.Seen()
	byOrg := map[string]int{}
	for _, s := range all {
		byOrg[s.Org]++
	}
	notes = append(notes, fmt.Sprintf("fallback-spare: the fallback pool's upstream received %d requests in all, by organisation %v; the main upstream %d", len(all), byOrg, len(w.Up.Seen())))
	for _, s := range all {
		if s.Org == Borrower && s.Started.After(from) {
			hits++
		}
	}
	for _, s := range w.Up.Seen() {
		if s.Org == Borrower && s.Started.After(from) {
			admitted++
		}
	}
	switch {
	case hits == 0 && admitted < 5:
		problems = append(problems, fmt.Sprintf("fallback-spare: only %d borrower requests were admitted in the main pool after the saturation: the probe proves nothing", admitted))
	case hits > 0:
		problems = append(problems, fmt.Sprintf("fallback-spare: %d borrower requests took a slot in the fallback pool whose vLLM queue is saturated, and %d stayed in the main pool (want 0 in the fallback pool: a spare-only organisation is refused there)", hits, admitted))
	default:
		notes = append(notes, fmt.Sprintf("fallback-spare: %d borrower requests served by the main pool, none moved to the saturated fallback pool", admitted))
	}
	return
}

func init() {
	// the store HANGS: connections are accepted and nothing is answered (a
	// restart refuses them, slow queries answer): gateway#165's trigger.
	faultTable["pg-hang"] = func(w *World, f FaultAt) (func(), error) {
		if w.PGProxy == nil {
			return nil, fmt.Errorf("no PostgreSQL relay in this scenario")
		}
		w.PGProxy.Set(ProxyBlackhole, 0)
		return func() { w.PGProxy.Set(ProxyPass, 0) }, nil
	}
}
