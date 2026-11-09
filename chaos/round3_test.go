package chaos

import (
	"strings"
	"testing"
	"time"
)

func TestStartClockOffsets(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	got := now.Add(startClockOffset("prev-month-20th", now))
	if want := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("prev-month-20th = %v, want %v", got, want)
	}
	// January: the previous month is December of the year before
	jan := time.Date(2027, 1, 5, 8, 0, 0, 0, time.UTC)
	if got := jan.Add(startClockOffset("prev-month-20th", jan)); !got.Equal(time.Date(2026, 12, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("january: %v", got)
	}
	e := now.Add(startClockOffset("tariff-edge", now))
	loc, _ := time.LoadLocation("Europe/Paris")
	if p := e.In(loc); p.Hour() != 22 || p.Minute() != 59 || p.Second() != 45 || !e.After(now) {
		t.Fatalf("tariff-edge = %v (Paris %v)", e, p)
	}
	if startClockOffset("", now) != 0 {
		t.Fatal("no clock, no offset")
	}
}

func TestClosedContentJudge(t *testing.T) {
	if p := closedContentProblem(true, 0); !strings.Contains(p, "empty month") {
		t.Fatalf("an empty month passed as a month with content: %q", p)
	}
	if closedContentProblem(true, 12.5) != "" || closedContentProblem(false, 0) != "" {
		t.Fatal("content, or no expectation of it, is fine")
	}
}

func TestSlowFsyncJudge(t *testing.T) {
	from := time.Now()
	mk := func(d time.Duration, n int) []Outcome {
		var o []Outcome
		for i := 0; i < n; i++ {
			o = append(o, Outcome{Status: 200, Dur: d, At: from.Add(time.Second)})
		}
		return o
	}
	p, n := slowFsyncProblems(mk(200*time.Millisecond, 100), from, from.Add(5*time.Second))
	if len(p) != 0 || !has(n, "20 requests/s") {
		t.Fatalf("a throughput is reported, never judged: %v %v", p, n)
	}
	if p, _ := slowFsyncProblems(nil, from, from.Add(5*time.Second)); !has(p, "proves nothing") {
		t.Fatalf("nothing answered: %v", p)
	}
}

func TestSpoolFullJudge(t *testing.T) {
	ok := func(n int) []Outcome {
		o := make([]Outcome, n)
		for i := range o {
			o[i] = Outcome{Status: 200}
		}
		return o
	}
	if p, _ := spoolFullProblems(ok(100), true); len(p) != 0 {
		t.Fatalf("a full spool: %v", p)
	}
	if p, _ := spoolFullProblems(ok(100), false); !has(p, "never reported the spool out of room") {
		t.Fatalf("a spool that never filled passed: %v", p)
	}
	if p, _ := spoolFullProblems(ok(3), true); !has(p, "proves nothing") {
		t.Fatalf("nothing answered: %v", p)
	}
}

func TestEachGroupIsAuditedOnItsOwnScenarios(t *testing.T) {
	for _, c := range []struct{ name, group string }{{"baseline", ""}, {"clock-jump-tariff-band", "clock"}, {"spool-disk-full", "fs"}, {"spool-slow-fsync", "fs"}, {"pg-readonly-failover", ""}} {
		if g := GroupOf(Find(c.name)); g != c.group {
			t.Errorf("GroupOf(%s) = %q, want %q", c.name, g, c.group)
		}
	}
	// the sharded campaign does not miss a scenario that needs a clock build, and the clock group misses only its own
	seeds := []int{1}
	var rs []Result
	for _, sc := range Catalogue() {
		if GroupOf(sc) == "" {
			rs = append(rs, Result{Scenario: sc.Name, Seed: 1, Requests: 5, Verdicts: fullVerdicts(sc)})
		}
	}
	if p := AuditGroup(rs, seeds, ""); len(p) != 0 {
		t.Fatalf("the sharded campaign: %v", p)
	}
	if p := strings.Join(AuditGroup(rs, seeds, "clock"), "\n"); !strings.Contains(p, "clock-jump-tariff-band/1: missing") {
		t.Fatalf("the clock group must miss its scenarios: %q", p)
	}
}

func TestAKnownDefectIsJudgedAndNeverHidden(t *testing.T) {
	sc := Find("clock-jump-tariff-band")
	if sc.Known[4] == "" {
		t.Fatal("the scenario lists its filed defect")
	}
	mk := func(st Status) []Result {
		vs := fullVerdicts(sc)
		vs[3].Status = st
		return []Result{{Scenario: sc.Name, Seed: 1, Requests: 5, Verdicts: vs}}
	}
	if p := AuditGroup(mk(Fail), []int{1}, "clock"); has(p, "invariant 4 FAIL") {
		t.Fatalf("a known defect is not a problem: %v", p)
	}
	if p := AuditGroup(mk(Pass), []int{1}, "clock"); !has(p, "looks fixed") {
		t.Fatalf("a known defect that passes must be reported: %v", p)
	}
	if mk(Fail)[0].FailsOutside(sc.Known) || !mk(Pass)[0].FailsOutside(sc.Known) {
		t.Fatal("FailsOutside")
	}
	other := mk(Pass)
	other[0].Verdicts[1].Status = Fail
	if !other[0].FailsOutside(sc.Known) {
		t.Fatal("another invariant failing is still a failure")
	}
}
