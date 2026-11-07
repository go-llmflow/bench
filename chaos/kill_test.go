package chaos

import (
	"testing"
	"time"
)

// Only answers served by the killed replica in the last seconds before the kill
// may go unbilled; the same loss anywhere else is judged.
func TestKillWindowCoversOnlyTheKilledReplicasLastSeconds(t *testing.T) {
	w := adminFake(t, nil, nil)
	w.GW = append(w.GW, &Gateway{})
	kill := time.Now()
	w.spans = []faultSpan{{FaultAt: FaultAt{Kind: "replica-kill", Target: "r1"}, From: kill}}
	at := func(id int, before time.Duration) Outcome {
		return Outcome{Req: Req{ID: id}, At: kill.Add(-before), Dur: 10 * time.Millisecond}
	}
	if !w.inKillWindow(at(1, 500*time.Millisecond)) { // ID 1 -> r1, half a second before
		t.Fatal("a request served by the killed replica just before the kill is covered")
	}
	if w.inKillWindow(at(2, 500*time.Millisecond)) { // ID 2 -> r0
		t.Fatal("the other replica's answers are not covered: it was not killed")
	}
	if w.inKillWindow(at(1, 30*time.Second)) {
		t.Fatal("an answer long before the kill was flushed: not covered")
	}
	// inv8 judges the planted loss outside the window: 1 clean answer, nothing billed.
	w2 := adminFake(t, nil, nil)
	w2.GW = append(w2.GW, &Gateway{})
	w2.spans = w.spans
	cleanReq(w2, "key:kA", "etabA", 4, 6, 10) // ID 4 -> r0, never killed
	all := w2.Ledger.All()
	all[0].At, all[0].Dur = kill.Add(-time.Minute), time.Millisecond
	w2.Ledger = Ledger{}
	w2.Ledger.Add(all[0])
	if v := inv8(w2, newJoin(w2)); v.Status != Fail {
		t.Fatalf("an unbilled answer outside the kill window passed: %+v", v)
	}
}
