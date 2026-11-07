package chaos

import (
	"strings"
	"testing"
)

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
