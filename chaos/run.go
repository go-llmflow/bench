package chaos

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// Result is one scenario run.
type Result struct {
	Scenario string
	Seed     uint64
	Requests int
	Verdicts []Verdict
	Notes    []string
	Err      string
	Skipped  string
	Elapsed  time.Duration
}

// Failed reports whether any invariant failed (or the run itself broke).
func (r Result) Failed() bool {
	if r.Err != "" {
		return true
	}
	// A run that sent nothing judged nothing: it is a failure, never a pass.
	if r.Skipped == "" && r.Requests == 0 {
		return true
	}
	for _, v := range r.Verdicts {
		if v.Status == Fail {
			return true
		}
	}
	return false
}

func rngFor(seed uint64, parts ...string) *rand.Rand {
	h := fnv.New64a()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return rand.New(rand.NewPCG(seed, h.Sum64()))
}

func pick[T any](r *rand.Rand, v []T, def T) T {
	if len(v) == 0 {
		return def
	}
	return v[r.IntN(len(v))]
}

// Run executes a scenario with a seed and judges the invariants.
func Run(ctx context.Context, opt Options, sc *Scenario) (res Result) {
	res = Result{Scenario: sc.Name, Seed: opt.Seed}
	t0 := time.Now()
	defer func() { res.Elapsed = time.Since(t0) }()
	if opt.DryRun {
		w, err := Setup(ctx, opt, sc)
		if err != nil {
			res.Err = "dry run: " + err.Error()
			return res
		}
		w.Close()
		res.Skipped = "dry run: configuration passes check-config"
		return res
	}
	if sc.needs("pg") && opt.PG == "" {
		res.Skipped = "needs PostgreSQL (--pg-dsn-file); run in CI"
		return res
	}
	w, err := Setup(ctx, opt, sc)
	if err != nil {
		res.Err = "setup: " + err.Error()
		return res
	}
	var closed bool
	closeWorld := func() {
		if !closed {
			closed = true
			w.Close()
		}
	}
	defer closeWorld()
	w.Logf("%s seed=%d: world up (%d gateway(s))", sc.Name, opt.Seed, len(w.GW))

	runCtx, stop := context.WithTimeout(ctx, sc.Duration+2*time.Minute)
	defer stop()
	loadEnd := time.Now().Add(sc.Duration)
	var wg sync.WaitGroup
	// background energy: ground truth for the fluids invariant.
	if sc.F.Fluids {
		wg.Add(1)
		go func() { defer wg.Done(); w.driveEnergy(runCtx, loadEnd) }()
	}
	// the timeline of faults
	rev := newReverts()
	for i, f := range sc.Faults {
		wg.Add(1)
		go func(i int, f FaultAt) {
			defer wg.Done()
			select {
			case <-time.After(f.At):
			case <-runCtx.Done():
				return
			}
			span := faultSpan{FaultAt: f, From: time.Now()}
			undo, err := w.applyFault(f)
			if err != nil {
				w.Note("fault %s: %v", f.Kind, err)
				return
			}
			w.Logf("fault %s applied at +%s", f.Kind, f.At)
			span.To = loadEnd
			defer func() { w.addSpan(span) }()
			if undo == nil {
				return
			}
			rev.add(undo)
			if f.For > 0 {
				select {
				case <-time.After(f.For):
				case <-runCtx.Done():
				}
				undo()
				span.To = time.Now()
				w.Logf("fault %s lifted", f.Kind)
			}
		}(i, f)
	}
	// the load
	hc := NewClient()
	var seq int
	orgs := append([]string(nil), AllOrgs...)
	for oi, org := range orgs {
		ld, ok := sc.Load[org]
		if !ok || ld.Workers == 0 {
			continue
		}
		clients := w.ClientsOf(org)
		for k := 0; k < ld.Workers; k++ {
			wg.Add(1)
			go func(oi, k int, org string, ld OrgLoad) {
				defer wg.Done()
				r := rngFor(opt.Seed, sc.Name, org, fmt.Sprint(k))
				if ld.Start > 0 {
					select {
					case <-time.After(ld.Start):
					case <-runCtx.Done():
						return
					}
				}
				for n := 0; time.Now().Before(loadEnd) && runCtx.Err() == nil; n++ {
					c := clients[(k+n)%len(clients)]
					req := w.newReq(r, ld, c, (oi+1)*1_000_000+k*10_000+n*10)
					w.runWithRetries(runCtx, hc, ld, req, r)
					if ld.Think > 0 {
						select {
						case <-time.After(time.Duration(r.Int64N(int64(ld.Think)))):
						case <-runCtx.Done():
						}
					}
				}
			}(oi, k, org, ld)
		}
	}
	for bi, b := range sc.Bursts {
		wg.Add(1)
		go func(bi int, b Burst) {
			defer wg.Done()
			select {
			case <-time.After(b.At):
			case <-runCtx.Done():
				return
			}
			w.burst(runCtx, hc, bi, b, &seq)
		}(bi, b)
	}
	wg.Wait()
	rev.all()
	w.Up.Set(UpstreamFaults{})
	w.Up.Calm()
	w.Logf("%s: load done, %d requests", sc.Name, len(w.Ledger.All()))
	w.settle(runCtx)
	res.Requests = len(w.Ledger.All())
	res.Verdicts = Judge(runCtx, w)
	w.dumpEvidence()
	// invariant 7 needs the world closed first.
	closeWorld()
	w.dumpLogs()
	hc.CloseIdleConnections()
	res.Verdicts = append(res.Verdicts, w.leakVerdict())
	res.Notes = w.Notes
	return res
}

type reverts struct {
	mu sync.Mutex
	f  []func()
}

func newReverts() *reverts { return &reverts{} }
func (r *reverts) add(f func()) {
	r.mu.Lock()
	r.f = append(r.f, f)
	r.mu.Unlock()
}
func (r *reverts) all() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.f {
		f()
	}
	r.f = nil
}

func (w *World) newReq(r *rand.Rand, ld OrgLoad, c Client, id int) Req {
	req := Req{ID: id, Org: c.Org, Team: c.Team, Who: c.Who, Prefix: c.Prefix, IdP: c.IdP}
	tok, err := w.Token(c)
	if err != nil {
		tok = "invalid"
	}
	req.Cred = Cred{Bearer: tok, Who: c.Who}
	req.Stream = r.IntN(100) < ld.StreamPct
	req.MaxTokens = pick(r, ld.Tokens, 32)
	req.Words = pick(r, ld.Words, 8)
	if r.IntN(100) < ld.NPct {
		req.N, req.BestOf = 2, 2
	}
	if req.Stream && r.IntN(100) < ld.LeavePct {
		req.Leave = Phases[r.IntN(len(Phases))]
	} else if r.IntN(100) < ld.LeavePct/3 {
		req.Leave = PhaseConnect
	}
	req.Retries = ld.RetryMax
	req.Marker = fmt.Sprintf("MK%s-%s-%d", c.Org, runTag(w.Opt.Seed), id)
	return req
}

func runTag(seed uint64) string { return fmt.Sprintf("s%d", seed) }

// runWithRetries sends a request; a refusal is retried at once (a storm) up to
// the load's limit. Every attempt is an outcome of its own.
func (w *World) runWithRetries(ctx context.Context, hc *http.Client, ld OrgLoad, req Req, r *rand.Rand) {
	for attempt := 0; ; attempt++ {
		q := req
		q.Attempt = attempt
		q.ID = req.ID + attempt
		q.Marker = fmt.Sprintf("MK%s-%s-%d", req.Org, runTag(w.Opt.Seed), q.ID)
		o := w.doBalanced(ctx, hc, q)
		w.Ledger.Add(o)
		if attempt >= req.Retries || ctx.Err() != nil || (o.Status != 429 && o.Status != 503 && o.Status != 502 && o.Status != 504) {
			return
		}
		// a client that honours Retry-After waits; a storm does not.
		if o.RetryAfter == "" || r.IntN(2) == 0 {
			continue
		}
		select {
		case <-time.After(time.Duration(r.IntN(300)) * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

// doBalanced is the load balancer in front of the replicas: round robin, and a
// replica that refuses the connection (it is down) is skipped. A single
// gateway is the only target of a one-replica scenario.
func (w *World) doBalanced(ctx context.Context, hc *http.Client, r Req) Outcome {
	n := len(w.GW)
	var o Outcome
	for i := 0; i < n; i++ {
		o = Do(ctx, hc, w.GW[(r.ID+i)%n].URL, r)
		if n == 1 || o.Status != 0 || o.Left || ctx.Err() != nil {
			return o
		}
	}
	return o
}

// burst releases N requests at the same instant.
func (w *World) burst(ctx context.Context, hc *http.Client, bi int, b Burst, _ *int) {
	clients := w.ClientsOf(b.Org)
	if b.Edge {
		clients = []Client{w.EdgeClient()}
	}
	if len(clients) == 0 {
		return
	}
	r := rngFor(w.Opt.Seed, w.Sc.Name, "burst", fmt.Sprint(bi))
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < b.N; i++ {
		c := clients[i%len(clients)]
		req := w.newReq(r, OrgLoad{Tokens: []int{b.Tokens}, Words: []int{8}}, c, 9_000_000+bi*1000+i)
		req.Stream = false
		req.Leave = PhaseNever
		req.Group = fmt.Sprintf("burst%d", bi)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			w.Ledger.Add(w.doBalanced(ctx, hc, req))
		}()
	}
	close(gate)
	wg.Wait()
}

// settle waits until the accounting has caught up: the gateway's spend stops
// moving and nothing is in flight upstream.
func (w *World) settle(ctx context.Context) {
	deadline := time.Now().Add(20 * time.Second)
	var last float64 = -1
	stable := 0
	for time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(700 * time.Millisecond)
		tot, err := w.spendTotal()
		if err == nil && tot == last && w.Up.inFl.Load() == 0 {
			stable++
			if stable >= 3 {
				break
			}
		} else {
			stable = 0
		}
		last = tot
	}
	if w.Sc.F.Fluids {
		time.Sleep(3 * time.Second) // two fluids intervals, so the last energy is booked
	}
}

// driveEnergy moves the DCGM counters (and the BMC's) while the load runs: the
// ground truth of the fluids invariant.
func (w *World) driveEnergy(ctx context.Context, until time.Time) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for time.Now().Before(until) {
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
		// 150 W and 180 W for 0.5 s on two GPUs, in millijoules.
		w.DCGM.Add("GPU-aaa", 75_000)
		w.DCGM.Add("GPU-bbb", 90_000)
		if w.BMC != nil {
			// the node draws 12 % more than its GPUs.
			w.BMC.Add(float64(75_000+90_000) * 1.12 / 3_600_000_000)
		}
	}
}
