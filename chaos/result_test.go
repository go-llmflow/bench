package chaos

import (
	"strings"
	"testing"
)

func TestARunThatSentNothingFails(t *testing.T) {
	if !(Result{Scenario: "x"}).Failed() {
		t.Fatal("0 requests, not skipped: nothing ran, that is a failure")
	}
	if (Result{Scenario: "x", Skipped: "dry run"}).Failed() {
		t.Fatal("a declared skip is not a failure by itself (--strict makes it one)")
	}
	if !(Result{Scenario: "x", Requests: 5, Verdicts: []Verdict{{N: 1, Status: Fail}}}).Failed() {
		t.Fatal("a failed invariant fails the run")
	}
	if (Result{Scenario: "x", Requests: 5, Verdicts: []Verdict{{N: 1, Status: Pass}, {N: 5, Status: NA}}}).Failed() {
		t.Fatal("passes and N/A do not fail")
	}
}

func TestTableCountsEveryVerdict(t *testing.T) {
	out := Table([]Result{
		{Scenario: "a", Seed: 1, Requests: 3, Verdicts: []Verdict{{N: 1, Status: Pass}, {N: 2, Status: Fail}, {N: 3, Status: NA}}},
		{Scenario: "b", Seed: 1, Skipped: "x"},
	})
	for _, want := range []string{"| a | 1 | 3 | P | **F** | - |", "| b | 1 | 0 | skip", "| 1 | 1 | 0 | 0 |", "| 2 | 0 | 1 | 0 |", "| 3 | 0 | 0 | 1 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}
