package chaos

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// bench#31: a random identifier the gateway adds to the request held
// "MK6E2TlQVR4b1eSDBo-KYoLP1p-6", the leftmost lookalike of a marker, and every
// request of one key was witnessed under that "organisation".
const lookalike = `{"model":"fake","max_tokens":16,"messages":[{"role":"system","content":"xMK6E2TlQVR4b1eSDBo-KYoLP1p-6x"},{"role":"user","content":"MKetabB-s5-201000000 a b c"}]}`

func TestTheMarkerIsTheHarnessOneNotALookalike(t *testing.T) {
	post := func(u *Upstream) {
		t.Helper()
		resp, err := http.Post(u.URL()+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(lookalike)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	// without the tag: the old reading, kept as the fallback
	u := NewUpstream(4)
	defer u.Close()
	post(u)
	if s := u.Seen(); len(s) != 1 || s[0].Org != "6E2TlQVR4b1eSDBo" {
		t.Fatalf("the untagged fallback no longer shows the lookalike this test plants: %+v", s)
	}
	// with the run's tag: the marker of the run
	v := NewUpstream(4)
	defer v.Close()
	v.SetRunTag("s5")
	post(v)
	if s := v.Seen(); len(s) != 1 || s[0].Marker != "MKetabB-s5-201000000" || s[0].Org != "etabB" {
		t.Fatalf("the tagged upstream read the wrong marker: %+v", s)
	}
}

func TestAnUnplacedWitnessMakesTheJoinsInvariantsIncomplete(t *testing.T) {
	w := &World{Sc: &Scenario{Name: "t"}, Up: NewUpstream(1)}
	defer w.Up.Close()
	// the lookalike is not tagged here: the planted fault
	resp, err := http.Post(w.Up.URL()+"/v1/chat/completions", "application/json", strings.NewReader(lookalike))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	j := newJoin(w)
	if o := j.orphans(); len(o) != 1 || o[0].Org != "6E2TlQVR4b1eSDBo" {
		t.Fatalf("orphans: %+v", o)
	}
	// a probe is not an orphan
	w.Up.SetRunTag("")
	resp, err = http.Post(w.Up.URL()+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fake","messages":[{"role":"user","content":"MKprobe-s0-1 a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if o := newJoin(w).orphans(); len(o) != 1 {
		t.Fatalf("a probe counted as an orphan: %+v", o)
	}
	_ = context.Background()
}

// gateway#180, the admission-time invariant. The pool's guarantee for a spare-only
// borrower is that it takes a slot only if the pool has room when it is ADMITTED
// (the instant the gateway tells the upstream, X-Llmflow-Admitted-At); owners are
// never refused, so the upstream may hold more than the pool because of them, and
// that is not judged.
func TestABorrowerIsJudgedAtItsAdmissionNotAtItsArrival(t *testing.T) {
	adm := time.Now()
	for _, c := range []struct {
		name  string
		build func(put func(string, Seen))
		want  Status
	}{
		{"owners admitted after the borrower may arrive before it, and exceed the pool", func(put func(string, Seen)) {
			for i := 0; i < 11; i++ { // started after the admission: not in the count then
				put(fmt.Sprintf("o%d", i), Seen{Marker: fmt.Sprintf("MKetabA-s1-%d", i), Org: "etabA", Started: adm.Add(time.Millisecond), Ended: adm.Add(time.Second)})
			}
		}, Pass},
		{"the pool was full of owners when the borrower was admitted", func(put func(string, Seen)) {
			for i := 0; i < PoolCapacity; i++ {
				put(fmt.Sprintf("o%d", i), Seen{Marker: fmt.Sprintf("MKetabA-s1-%d", i), Org: "etabA", Started: adm.Add(-time.Second), Ended: adm.Add(time.Second)})
			}
		}, Fail},
		{"a request that had left the count before the admission does not hold a slot", func(put func(string, Seen)) {
			for i := 0; i < PoolCapacity; i++ {
				put(fmt.Sprintf("o%d", i), Seen{Marker: fmt.Sprintf("MKetabA-s1-%d", i), Org: "etabA", Started: adm.Add(-time.Second), Freed: adm.Add(-time.Millisecond), Ended: adm.Add(time.Second)})
			}
		}, Pass},
		{"a killed replica's leftover (its connection gone) does not hold a slot", func(put func(string, Seen)) {
			for i := 0; i < PoolCapacity; i++ {
				put(fmt.Sprintf("o%d", i), Seen{Marker: fmt.Sprintf("MKetabA-s1-%d", i), Org: "etabA", Started: adm.Add(-time.Second), Gone: adm.Add(-time.Millisecond), Ended: adm.Add(time.Second)})
			}
		}, Pass},
		{"borrowers among themselves over the pool", func(put func(string, Seen)) {
			for i := 0; i < PoolCapacity; i++ {
				put(fmt.Sprintf("b%d", i), Seen{Marker: fmt.Sprintf("MKtiers-s1-%d", i), Org: Borrower, Started: adm.Add(-time.Second), Ended: adm.Add(time.Second)})
			}
		}, Fail},
	} {
		w := &World{Sc: &Scenario{Name: "t"}, Up: NewUpstream(1)}
		put := func(k string, s Seen) { w.Up.seen[k] = &s; w.Up.order = append(w.Up.order, k) }
		c.build(put)
		put("me", Seen{Marker: "MKtiers-s1-99", Org: Borrower, N: 1, Started: adm.Add(2 * time.Millisecond), AdmittedAt: adm, Ended: adm.Add(time.Second)})
		v := inv3(w, newJoin(w))
		w.Up.Close()
		if v.Status != c.want {
			t.Errorf("%s: %s %s", c.name, v.Status, v.Detail)
		}
	}
}

// A release that does not tell the upstream when it admitted is not judged on
// the concurrency, and the verdict says so.
func TestWithoutTheAdmissionInstantTheConcurrencyIsNotJudgedAndSaysSo(t *testing.T) {
	w := &World{Sc: &Scenario{Name: "t"}, Up: NewUpstream(1)}
	defer w.Up.Close()
	for i := 0; i < PoolCapacity+3; i++ {
		s := Seen{Marker: fmt.Sprintf("MKetabA-s1-%d", i), Org: "etabA", Started: time.Now(), Ended: time.Now().Add(time.Second)}
		w.Up.seen[s.Marker] = &s
		w.Up.order = append(w.Up.order, s.Marker)
	}
	s := Seen{Marker: "MKtiers-s1-1", Org: Borrower, N: 1, Started: time.Now(), Concurrent: PoolCapacity + 4}
	w.Up.seen[s.Marker] = &s
	w.Up.order = append(w.Up.order, s.Marker)
	v := inv3(w, newJoin(w))
	if v.Status != Pass || !strings.Contains(v.Detail, "NOT judged") {
		t.Fatalf("%s %s", v.Status, v.Detail)
	}
}
