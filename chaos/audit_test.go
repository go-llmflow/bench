package chaos

import (
	"errors"
	"strings"
	"testing"
)

func TestSetupRetriesOnlyAPortRace(t *testing.T) {
	calls := 0
	_, err := retryOnPortRace(func() (*World, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("gateway exited at start: listen tcp 127.0.0.1:1: bind: address already in use")
		}
		return nil, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v: two port races then success", calls, err)
	}
	calls = 0
	_, err = retryOnPortRace(func() (*World, error) { calls++; return nil, errors.New("check-config: bad") })
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v: any other error is final, at once", calls, err)
	}
	calls = 0
	_, err = retryOnPortRace(func() (*World, error) { calls++; return nil, errors.New("address already in use") })
	if err == nil || calls != 3 {
		t.Fatalf("calls=%d err=%v: a persistent port clash gives up after 3", calls, err)
	}
}

// A fault with a duration lifts itself; the end of the load must not lift it a
// second time (a replica started twice leaked the first process).
func TestALiftedFaultIsNotLiftedAgainAtTheEnd(t *testing.T) {
	r := newReverts()
	n := 0
	undo := r.add(func() { n++ })
	undo() // the fault lifts itself
	r.all()
	undo()
	if n != 1 {
		t.Fatalf("the revert ran %d times, want once", n)
	}
	m := 0
	r.add(func() { m++ }) // a fault with no duration: lifted by all()
	r.all()
	if m != 1 {
		t.Fatalf("an open-ended fault was lifted %d times by all(), want once", m)
	}
}

func TestCompareListsOnlyStatusChanges(t *testing.T) {
	mk := func(st Status, detail string) []Result {
		return []Result{{Scenario: "s", Seed: 1, Requests: 1, Verdicts: []Verdict{{N: 1, Status: Pass, Detail: detail}, {N: 2, Status: st, Detail: detail}}}}
	}
	if d := Compare(mk(Pass, "x 1"), mk(Pass, "x 2")); len(d) != 0 {
		t.Fatalf("a different detail is not a difference: %v", d)
	}
	d := Compare(mk(Pass, "x"), mk(Fail, "y"))
	if len(d) != 1 || !strings.Contains(d[0], "s/1 invariant 2: PASS -> FAIL") {
		t.Fatalf("diff = %v", d)
	}
	if d := Compare(mk(Pass, "x"), nil); len(d) != 1 || !strings.Contains(d[0], "only in the first") {
		t.Fatalf("a run missing from the second campaign: %v", d)
	}
}

func fullVerdicts(sc *Scenario) []Verdict {
	var vs []Verdict
	for n := 1; n < len(InvariantNames); n++ {
		s := Pass
		if expectedNA(sc, n) {
			s = NA
		}
		vs = append(vs, Verdict{N: n, Status: s})
	}
	return vs
}

func cleanCampaign(seeds []int) []Result {
	var rs []Result
	for _, sc := range Catalogue() {
		for _, s := range seeds {
			rs = append(rs, Result{Scenario: sc.Name, Seed: uint64(s), Requests: 10, Verdicts: fullVerdicts(sc)})
		}
	}
	return rs
}

func TestAuditAcceptsACleanCampaign(t *testing.T) {
	if p := Audit(cleanCampaign([]int{1, 2}), []int{1, 2}); len(p) != 0 {
		t.Fatalf("problems on a clean campaign: %v", p)
	}
}

func TestAuditCatchesEveryPlantedProblem(t *testing.T) {
	seeds := []int{1}
	plant := func(f func(rs []Result) []Result) string {
		return strings.Join(Audit(f(cleanCampaign(seeds)), seeds), "\n")
	}
	for name, c := range map[string]struct {
		f    func(rs []Result) []Result
		want string
	}{
		"fail":    {func(rs []Result) []Result { rs[0].Verdicts[1].Status = Fail; return rs }, "invariant 2 FAIL"},
		"err":     {func(rs []Result) []Result { rs[0].Err = "boom"; return rs }, "ERROR boom"},
		"skip":    {func(rs []Result) []Result { rs[0].Skipped = "no pg"; return rs }, "skipped"},
		"zero":    {func(rs []Result) []Result { rs[0].Requests = 0; return rs }, "sent no request"},
		"missing": {func(rs []Result) []Result { return rs[1:] }, "missing from the campaign"},
		"dup":     {func(rs []Result) []Result { return append(rs, rs[0]) }, "two results"},
		"unexpected N/A": {func(rs []Result) []Result {
			rs[0].Verdicts[2].Status = NA // invariant 3 of baseline has no excuse
			return rs
		}, "invariant 3 unexpected N/A"},
		"no verdict": {func(rs []Result) []Result { rs[0].Verdicts = rs[0].Verdicts[:8]; return rs }, "invariant 9 has no verdict"},
	} {
		if got := plant(c.f); !strings.Contains(got, c.want) {
			t.Errorf("%s: audit says %q, want %q", name, got, c.want)
		}
	}
}
