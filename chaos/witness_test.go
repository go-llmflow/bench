package chaos

import (
	"bytes"
	"context"
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

func TestARequestCountedOnlyBecauseItsConnectionIsGoneIsNotAnAdmission(t *testing.T) {
	t0 := time.Now()
	build := func(zombie bool) *World {
		w := &World{Sc: &Scenario{Name: "t"}, Up: NewUpstream(1)}
		put := func(k string, s Seen) { w.Up.seen[k] = &s; w.Up.order = append(w.Up.order, k) }
		put("a#1", Seen{Marker: "MKtiers-s1-1", Org: Borrower, Started: t0, Concurrent: PoolCapacity + 1, Ended: t0.Add(time.Second)})
		if zombie { // a killed replica's request: connection gone before, handler still running after
			put("b#2", Seen{Marker: "MKetabA-s1-2", Org: "etabA", Started: t0.Add(-time.Second), Gone: t0.Add(-time.Millisecond), Ended: t0.Add(time.Second)})
		}
		return w
	}
	for _, c := range []struct {
		zombie bool
		want   Status
	}{{true, Pass}, {false, Fail}} {
		w := build(c.zombie)
		v := inv3(w, newJoin(w))
		w.Up.Close()
		if v.Status != c.want {
			t.Fatalf("zombie=%v: %s %s", c.zombie, v.Status, v.Detail)
		}
	}
}
