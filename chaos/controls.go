package chaos

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
	for _, f := range []func() ([]string, []string){w.schacSpoofProbe, w.nSlotsProbe} {
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
