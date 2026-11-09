package chaos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Round 2: network faults, hostile tenants, an IdP that changes its schac, a
// long run. Faults here are in faultTable; judges are pure functions (tested
// with planted breakage) called from controlProbes.

// floodStats counts the answers of a hostile client of the gateway.
type floodStats struct {
	ok, bad, slow atomic.Int64
}

func (f *floodStats) add(code int, d time.Duration) {
	switch {
	case code == 200 || code == 429:
		f.ok.Add(1)
	default:
		f.bad.Add(1)
	}
	if d > 5*time.Second {
		f.slow.Add(1)
	}
}

// floodProblems judges a flood: the gateway answered every hostile request
// with 200 (or 429, a refusal), none with a 5xx or a transport failure, none
// after more than 5 s; and it answered something at all.
func floodProblems(what string, ok, bad, slow int64) []string {
	switch {
	case ok+bad == 0:
		return []string{what + ": the flood sent nothing, the probe proves nothing"}
	case bad > 0:
		return []string{fmt.Sprintf("%s: %d of %d hostile requests were not answered 200 or 429", what, bad, ok+bad)}
	case slow > 0:
		return []string{fmt.Sprintf("%s: %d hostile requests took more than 5 s", what, slow)}
	}
	return nil
}

// schacChangeProblems judges an IdP that changes the schac value it states at
// `at`: before it, no federation token is refused; from 1 s after it, every one
// is (the value is no longer one the issuer accepts).
func schacChangeProblems(reqs []Outcome, at time.Time) (problems, notes []string) {
	var before, after, badBefore, badAfter int
	for _, r := range reqs {
		if r.IdP != "fed" || r.Left || r.Status == 0 {
			continue
		}
		switch {
		case r.At.Before(at.Add(-time.Second)):
			before++
			if r.Status == 401 {
				badBefore++
			}
		case r.At.After(at.Add(time.Second)):
			after++
			if r.Status != 401 {
				badAfter++
			}
		}
	}
	switch {
	case before == 0 || after == 0:
		problems = append(problems, fmt.Sprintf("schac-change: %d federation requests before and %d after the change: the probe proves nothing", before, after))
	case badBefore > 0:
		problems = append(problems, fmt.Sprintf("schac-change: %d of %d federation tokens were refused before the IdP changed its schac", badBefore, before))
	case badAfter > 0:
		problems = append(problems, fmt.Sprintf("schac-change: %d of %d federation tokens stating the new schac value were not refused (401)", badAfter, after))
	default:
		notes = append(notes, fmt.Sprintf("schac-change: %d tokens accepted before, %d refused after", before, after))
	}
	return
}

// ---- the long run: trends ----

type trendSample struct {
	At       time.Time
	RSSKB    int64 // the gateway's resident set; -1 when unknown
	FDs      int
	SpoolKB  int64
	Requests int
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return c[len(c)/2]
}

// trendProblems compares the first and the last fifth of the samples: the
// gateway's memory, descriptors and spool must not grow with time at a steady
// load. Bounds: RSS x1.6 + 32 MiB, descriptors +20, spool 8 MiB.
func trendProblems(ss []trendSample) (problems, notes []string) {
	if len(ss) < 20 {
		return []string{fmt.Sprintf("long-run: only %d samples: the run was too short to show a trend", len(ss))}, nil
	}
	k := len(ss) / 5
	pick := func(part []trendSample, f func(trendSample) float64) float64 {
		v := make([]float64, 0, len(part))
		for _, s := range part {
			if x := f(s); x >= 0 {
				v = append(v, x)
			}
		}
		return median(v)
	}
	rss := func(s trendSample) float64 { return float64(s.RSSKB) }
	fds := func(s trendSample) float64 { return float64(s.FDs) }
	spool := func(s trendSample) float64 { return float64(s.SpoolKB) }
	first, last := ss[:k], ss[len(ss)-k:]
	r0, r1 := pick(first, rss), pick(last, rss)
	f0, f1 := pick(first, fds), pick(last, fds)
	var sp float64
	for _, s := range ss {
		sp = max(sp, spool(s))
	}
	if r0 > 0 && r1 > r0*1.6+32*1024 {
		problems = append(problems, fmt.Sprintf("long-run: the gateway's resident memory grew from %.0f to %.0f MiB", r0/1024, r1/1024))
	}
	if f0 >= 0 && f1 > f0+20 {
		problems = append(problems, fmt.Sprintf("long-run: the gateway's descriptors grew from %.0f to %.0f", f0, f1))
	}
	if sp > 8*1024 {
		problems = append(problems, fmt.Sprintf("long-run: the spool reached %.1f MiB (bound 8 MiB)", sp/1024))
	}
	if len(problems) == 0 {
		notes = append(notes, fmt.Sprintf("long-run: %d samples; memory %.0f -> %.0f MiB, descriptors %.0f -> %.0f, spool at most %.1f MiB", len(ss), r0/1024, r1/1024, f0, f1, sp/1024))
	}
	return
}

// driftProblems cuts the run into `windows` equal windows and compares each
// organisation's share of the answered requests of the owners: it must not
// drift by more than 10 points between windows (fair share over time).
func driftProblems(outs []Outcome, start time.Time, total time.Duration, windows int) (problems, notes []string) {
	owners := []string{"etabA", "etabB", "etabC"}
	cnt := make([]map[string]int, windows)
	sum := make([]int, windows)
	for i := range cnt {
		cnt[i] = map[string]int{}
	}
	for _, o := range outs {
		if o.Status != 200 || o.At.Before(start) {
			continue
		}
		i := int(o.At.Sub(start) * time.Duration(windows) / total)
		if i < 0 || i >= windows {
			continue
		}
		for _, ow := range owners {
			if o.Org == ow {
				cnt[i][ow]++
				sum[i]++
			}
		}
	}
	for _, ow := range owners {
		lo, hi := 2.0, -1.0
		for i := range cnt {
			if sum[i] < 50 {
				return []string{fmt.Sprintf("drift: window %d has only %d owner answers: the probe proves nothing", i, sum[i])}, nil
			}
			f := float64(cnt[i][ow]) / float64(sum[i])
			lo, hi = min(lo, f), max(hi, f)
		}
		if hi-lo > 0.10 {
			problems = append(problems, fmt.Sprintf("drift: %s's share of the owners' answers moved from %.0f%% to %.0f%% between windows", ow, lo*100, hi*100))
		}
	}
	if len(problems) == 0 {
		notes = append(notes, fmt.Sprintf("drift: the owners' shares held within 10 points over %d windows", windows))
	}
	return
}

// sampleTrends reads the gateway's resident set, descriptors and spool size.
func (w *World) sampleTrends(ctx context.Context, until time.Time) {
	for time.Now().Before(until) && ctx.Err() == nil {
		g := w.Primary()
		s := trendSample{At: time.Now(), RSSKB: -1, FDs: fdCount(g.PID()), Requests: len(w.Ledger.All())}
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", g.PID())); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "VmRSS:") {
					f := strings.Fields(l)
					if len(f) >= 2 {
						s.RSSKB, _ = strconv.ParseInt(f[1], 10, 64)
					}
				}
			}
		}
		var sz int64
		_ = filepath.Walk(filepath.Join(g.Dir, "spool"), func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				sz += fi.Size()
			}
			return nil
		})
		s.SpoolKB = sz / 1024
		w.mu.Lock()
		w.trends = append(w.trends, s)
		w.mu.Unlock()
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
		}
	}
}

// ---- probes ----

func (w *World) floodProbe() (problems, notes []string) {
	if !w.Sc.expects("flood") {
		return nil, nil
	}
	for _, f := range w.Sc.Faults {
		switch f.Kind {
		case "admin-flood":
			problems = append(problems, floodProblems("admin-flood", w.adminFlood.ok.Load(), w.adminFlood.bad.Load(), w.adminFlood.slow.Load())...)
		case "fed-hammer":
			problems = append(problems, floodProblems("fed-hammer", w.fedFlood.ok.Load(), w.fedFlood.bad.Load(), w.fedFlood.slow.Load())...)
		}
	}
	if len(problems) == 0 {
		notes = append(notes, fmt.Sprintf("flood: admin %d answers, federation metrics %d answers, none a failure", w.adminFlood.ok.Load(), w.fedFlood.ok.Load()))
	}
	return
}

func (w *World) schacChangeProbe() (problems, notes []string) {
	if !w.Sc.expects("schac-change") {
		return nil, nil
	}
	w.mu.Lock()
	at := w.schacAt
	w.mu.Unlock()
	if at.IsZero() {
		return []string{"schac-change: the IdP never changed its schac"}, nil
	}
	return schacChangeProblems(w.Ledger.All(), at)
}

func (w *World) longRunProbe() (problems, notes []string) {
	if !w.Sc.expects("long-run") {
		return nil, nil
	}
	w.mu.Lock()
	ss := append([]trendSample(nil), w.trends...)
	w.mu.Unlock()
	rows := make([]any, 0, len(ss))
	for _, s := range ss { // the series is evidence: a trend is a shape, not a verdict
		rows = append(rows, s)
	}
	w.writeJSONL("trends.jsonl", rows)
	p, n := trendProblems(ss)
	problems, notes = append(problems, p...), append(notes, n...)
	p, n = driftProblems(w.Ledger.All(), w.started, w.Sc.Duration, 6)
	return append(problems, p...), append(notes, n...)
}

// ---- faults ----

// goFlood runs n workers calling do until stop is closed.
func goFlood(n int, stop <-chan struct{}, do func()) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					do()
				}
			}
		}()
	}
	return &wg
}

func init() {
	// latency between the gateway and its upstream
	faultTable["net-slow"] = func(w *World, f FaultAt) (func(), error) {
		if w.UpProxy == nil {
			return nil, fmt.Errorf("no upstream relay in this scenario")
		}
		w.UpProxy.Set(ProxySlow, time.Duration(f.Arg)*time.Millisecond)
		return func() { w.UpProxy.Set(ProxyPass, 0) }, nil
	}
	// loss between the gateway and its upstream: connections are reset and
	// refused in turns of 300 ms
	faultTable["net-loss"] = func(w *World, f FaultAt) (func(), error) {
		if w.UpProxy == nil {
			return nil, fmt.Errorf("no upstream relay in this scenario")
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			bad := false
			for {
				bad = !bad
				if bad {
					w.UpProxy.Set(ProxyRefuse, 0)
				} else {
					w.UpProxy.Set(ProxyPass, 0)
				}
				select {
				case <-stop:
					return
				case <-time.After(300 * time.Millisecond):
				}
			}
		}()
		var once sync.Once
		return func() { once.Do(func() { close(stop); <-done; w.UpProxy.Set(ProxyPass, 0) }) }, nil
	}
	// latency between the replicas
	faultTable["ha-link-slow"] = func(w *World, f FaultAt) (func(), error) {
		if len(w.HAProxy) == 0 {
			return nil, fmt.Errorf("no HA group in this scenario")
		}
		for _, p := range w.HAProxy {
			p.Set(ProxySlow, time.Duration(f.Arg)*time.Millisecond)
		}
		return func() {
			for _, p := range w.HAProxy {
				p.Set(ProxyPass, 0)
			}
		}, nil
	}
	// an organisation floods the admin reports (spend, budgets)
	faultTable["admin-flood"] = func(w *World, f FaultAt) (func(), error) {
		stop := make(chan struct{})
		wg := goFlood(int(f.Arg), stop, func() {
			from, to := window()
			for _, p := range []string{"/admin/spend?group_by=org,team,key&from=" + from + "&to=" + to, "/admin/budgets"} {
				t0 := time.Now()
				code, _, err := w.Primary().Admin("GET", p, "")
				if err != nil {
					code = 0
				}
				w.adminFlood.add(code, time.Since(t0))
			}
		})
		var once sync.Once
		return func() { once.Do(func() { close(stop); wg.Wait() }) }, nil
	}
	// a federation peer hammers <prefix>/metrics
	faultTable["fed-hammer"] = func(w *World, f FaultAt) (func(), error) {
		stop := make(chan struct{})
		wg := goFlood(int(f.Arg), stop, func() {
			t0 := time.Now()
			_, err := httpGet(w.Primary().URL + FedPrefix + "/metrics")
			code := 200
			if err != nil {
				code = 0
			}
			w.fedFlood.add(code, time.Since(t0))
		})
		var once sync.Once
		return func() { once.Do(func() { close(stop); wg.Wait() }) }, nil
	}
	// the federation IdP states another schac value from now on
	faultTable["idp-schac-change"] = func(w *World, f FaultAt) (func(), error) {
		w.mu.Lock()
		w.schacAt = time.Now()
		w.schacOverride = "univ-e.example"
		w.mu.Unlock()
		return nil, nil
	}
}
