package chaos

import (
	"strings"
	"testing"
	"time"
)

// The share judgement fails when the owners are served equally (the planted
// breakage: what the gateway would do with no fair share) and passes when they
// are served 50/30/20 within the 15-point tolerance.
func TestInvariant3JudgesTheOwnersShares(t *testing.T) {
	mk := func(a, b, c int) *World {
		w := adminFake(t, nil, nil)
		w.Sc = &Scenario{Name: "t", PrioritySlots: 8, Expect: []string{"owner-shares"},
			Load: map[string]OrgLoad{"etabA": {Start: time.Second}}}
		w.started = time.Now().Add(-time.Minute)
		id := 0
		for org, n := range map[string]int{"etabA": a, "etabB": b, "etabC": c} {
			for i := 0; i < n; i++ {
				id++
				cleanReq(w, "key:k"+org, org, id, 6, 10)
				// place it after the ramp
				all := w.Ledger.All()
				all[len(all)-1].At = w.started.Add(30 * time.Second)
				w.Ledger = Ledger{}
				for _, o := range all {
					w.Ledger.Add(o)
				}
			}
		}
		return w
	}
	good := mk(50, 30, 20)
	if v := inv3(good, newJoin(good)); v.Status != Pass {
		t.Fatalf("50/30/20 must pass: %+v", v)
	}
	bad := mk(34, 33, 33)
	v := inv3(bad, newJoin(bad))
	if v.Status != Fail || !strings.Contains(v.Detail, "etabA got") {
		t.Fatalf("an equal split must fail: %+v", v)
	}
}
