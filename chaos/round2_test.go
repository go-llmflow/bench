package chaos

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func has(ps []string, want string) bool {
	return strings.Contains(strings.Join(ps, "\n"), want)
}

func TestFloodJudgeFlagsEveryPlantedBreakage(t *testing.T) {
	if p := floodProblems("x", 100, 0, 0); len(p) != 0 {
		t.Fatalf("a clean flood: %v", p)
	}
	if p := floodProblems("x", 0, 0, 0); !has(p, "proves nothing") {
		t.Fatalf("a flood that sent nothing: %v", p)
	}
	if p := floodProblems("x", 100, 3, 0); !has(p, "3 of 103") {
		t.Fatalf("hostile requests that failed: %v", p)
	}
	if p := floodProblems("x", 100, 0, 2); !has(p, "more than 5 s") {
		t.Fatalf("hostile requests that took long: %v", p)
	}
	var f floodStats
	f.add(200, time.Second)
	f.add(429, time.Second)
	f.add(500, time.Second)
	f.add(200, 6*time.Second)
	if f.ok.Load() != 3 || f.bad.Load() != 1 || f.slow.Load() != 1 {
		t.Fatalf("floodStats %d %d %d", f.ok.Load(), f.bad.Load(), f.slow.Load())
	}
}

func TestSchacChangeJudge(t *testing.T) {
	at := time.Now()
	mk := func(off time.Duration, status int) Outcome {
		return Outcome{IdP: "fed", Status: status, At: at.Add(off)}
	}
	good := []Outcome{mk(-5*time.Second, 200), mk(-3*time.Second, 200), mk(3*time.Second, 401), mk(5*time.Second, 401)}
	if p, _ := schacChangeProblems(good, at); len(p) != 0 {
		t.Fatalf("a correct gateway: %v", p)
	}
	// planted: the gateway keeps accepting the new value
	bad := append([]Outcome(nil), good...)
	bad[3] = mk(5*time.Second, 200)
	if p, _ := schacChangeProblems(bad, at); !has(p, "not refused") {
		t.Fatalf("an accepted new schac passed: %v", p)
	}
	// planted: the gateway refuses before the change
	early := append([]Outcome(nil), good...)
	early[0] = mk(-5*time.Second, 401)
	if p, _ := schacChangeProblems(early, at); !has(p, "refused before") {
		t.Fatalf("a refusal before the change passed: %v", p)
	}
	if p, _ := schacChangeProblems(good[:2], at); !has(p, "proves nothing") {
		t.Fatalf("no request after the change: %v", p)
	}
}

func TestTrendJudge(t *testing.T) {
	t0 := time.Now()
	mk := func(n int, rss func(i int) int64, fds func(i int) int, spool func(i int) int64) []trendSample {
		var ss []trendSample
		for i := 0; i < n; i++ {
			ss = append(ss, trendSample{At: t0.Add(time.Duration(i) * 10 * time.Second), RSSKB: rss(i), FDs: fds(i), SpoolKB: spool(i)})
		}
		return ss
	}
	flat := mk(60, func(int) int64 { return 100 << 10 }, func(int) int { return 30 }, func(int) int64 { return 100 })
	if p, _ := trendProblems(flat); len(p) != 0 {
		t.Fatalf("a flat run: %v", p)
	}
	leakMem := mk(60, func(i int) int64 { return int64(100<<10 + i*4<<10) }, func(int) int { return 30 }, func(int) int64 { return 100 })
	if p, _ := trendProblems(leakMem); !has(p, "resident memory grew") {
		t.Fatalf("a memory leak passed: %v", p)
	}
	leakFD := mk(60, func(int) int64 { return 100 << 10 }, func(i int) int { return 30 + i }, func(int) int64 { return 100 })
	if p, _ := trendProblems(leakFD); !has(p, "descriptors grew") {
		t.Fatalf("a descriptor leak passed: %v", p)
	}
	spool := mk(60, func(int) int64 { return 100 << 10 }, func(int) int { return 30 }, func(i int) int64 { return int64(i * 400) })
	if p, _ := trendProblems(spool); !has(p, "spool reached") {
		t.Fatalf("a growing spool passed: %v", p)
	}
	if p, _ := trendProblems(flat[:5]); !has(p, "too short") {
		t.Fatalf("a short run: %v", p)
	}
	// an unknown RSS (not Linux) is not a leak
	unknown := mk(60, func(int) int64 { return -1 }, func(int) int { return 30 }, func(int) int64 { return 100 })
	if p, _ := trendProblems(unknown); len(p) != 0 {
		t.Fatalf("an unknown RSS: %v", p)
	}
}

func TestDriftJudge(t *testing.T) {
	start := time.Now()
	total := 6 * time.Minute
	gen := func(share func(window int) [3]int) []Outcome {
		var out []Outcome
		for w := 0; w < 6; w++ {
			c := share(w)
			for k, org := range []string{"etabA", "etabB", "etabC"} {
				for i := 0; i < c[k]; i++ {
					out = append(out, Outcome{Req: Req{Org: org}, Status: 200, At: start.Add(time.Duration(w)*time.Minute + time.Duration(i)*time.Millisecond)})
				}
			}
		}
		return out
	}
	steady := gen(func(int) [3]int { return [3]int{50, 30, 20} })
	if p, _ := driftProblems(steady, start, total, 6); len(p) != 0 {
		t.Fatalf("a steady run: %v", p)
	}
	drift := gen(func(w int) [3]int { return [3]int{50 + w*6, 30 - w*3, 20 - w*3} })
	if p, _ := driftProblems(drift, start, total, 6); !has(p, "moved from") {
		t.Fatalf("a drifting share passed: %v", p)
	}
	if p, _ := driftProblems(steady[:60], start, total, 6); !has(p, "proves nothing") {
		t.Fatalf("empty windows: %v", p)
	}
}

// A request the gateway answered 5xx without the upstream ever receiving it
// may be counted (the link was cut), at no cost; a 200 never received may not.
func TestInvariant2AllowsTheCountOfA5xxThatNeverReachedTheUpstream(t *testing.T) {
	cost := Cost(6, 10)
	row := func(n int64) []SpendRow {
		return []SpendRow{{Org: "etabA", Team: "a1", Key: "kA", Requests: n, Cost: cost}}
	}
	mk := func(rows []SpendRow, status int) *World {
		w := adminFake(t, rows, nil)
		cleanReq(w, "key:kA", "etabA", 1, 6, 10)
		w.Ledger.Add(Outcome{Req: Req{ID: 2, Org: "etabA", Who: "key:kA", Marker: "MKetabA-s1-2", MaxTokens: 32}, Status: status, BodyLen: 5})
		return w
	}
	if w := mk(row(2), 502); inv2(w, newJoin(w)).Status != Pass {
		t.Fatal("a counted 502 that never reached the upstream fails")
	}
	if w := mk(row(2), 200); inv2(w, newJoin(w)).Status != Fail {
		t.Fatal("a counted 200 that never reached the upstream passed")
	}
	if w := mk(row(3), 502); inv2(w, newJoin(w)).Status != Fail {
		t.Fatal("a count beyond clean + the 502 passed")
	}
}

// The marker of a request is its identity in the join with the upstream's
// witnesses: two workers of one organisation must never produce the same one,
// however long the run (the ids met after 1000 requests of a worker, which
// corrupted every long run's invariant 2).
func TestRequestIdsOfWorkersNeverMeet(t *testing.T) {
	seen := map[int]string{}
	for oi := 0; oi < 4; oi++ {
		for k := 0; k < 99; k++ {
			for n := 0; n < 99_999; n += 97 { // a sample of the first 100000 requests of each worker
				id := (oi+1)*100_000_000 + k*1_000_000 + n*10
				who := fmt.Sprint(oi, "/", k, "/", n)
				if prev, ok := seen[id]; ok {
					t.Fatalf("id %d of %s is also %s", id, who, prev)
				}
				seen[id] = who
			}
		}
	}
}

func TestNAdmissionJudge(t *testing.T) {
	mk := func(admitted, refused int) *World {
		w := adminFake(t, nil, nil)
		w.Sc = &Scenario{Name: "t", Expect: []string{"n-admission"}}
		id := 0
		for i := 0; i < admitted; i++ {
			id++
			w.Ledger.Add(Outcome{Req: Req{ID: id, Org: Borrower, Who: "key:kA"}, Status: 200})
		}
		for i := 0; i < refused; i++ {
			id++
			w.Ledger.Add(Outcome{Req: Req{ID: id, Org: Borrower, Who: "key:kA"}, Status: 429})
		}
		return w
	}
	if p, _ := mk(1, 100).nAdmissionProbe(); len(p) != 0 {
		t.Fatalf("a gateway that refuses n=4 with 3 free: %v", p)
	}
	if p, _ := mk(100, 0).nAdmissionProbe(); !has(p, "proves nothing") {
		t.Fatalf("an uncontended pool: %v", p)
	}
	if p, _ := mk(60, 40).nAdmissionProbe(); !has(p, "must hold n slots") {
		t.Fatalf("a gateway that admits n=4 with 3 free passed: %v", p)
	}
}

func TestSpoolFairJudge(t *testing.T) {
	from := time.Now()
	to := from.Add(10 * time.Second)
	mk := func(bServed, bRefused, aRefused int) []Outcome {
		at := from.Add(5 * time.Second)
		var out []Outcome
		for i := 0; i < bServed; i++ {
			out = append(out, Outcome{Req: Req{Org: "etabB"}, Status: 200, At: at})
		}
		for i := 0; i < bRefused; i++ {
			out = append(out, Outcome{Req: Req{Org: "etabB"}, Status: 503, At: at})
		}
		for i := 0; i < aRefused; i++ {
			out = append(out, Outcome{Req: Req{Org: "etabA"}, Status: 503, At: at})
		}
		return out
	}
	if p, _ := spoolFairProblems(mk(100, 5, 50), from, to); len(p) != 0 {
		t.Fatalf("a fair spool: %v", p)
	}
	// planted: the innocent is refused with the flooder (v0.38.1: every budgeted request)
	if p, _ := spoolFairProblems(mk(10, 90, 50), from, to); !has(p, "filled it for everyone") {
		t.Fatalf("the innocent refused passed: %v", p)
	}
	if p, _ := spoolFairProblems(mk(100, 0, 0), from, to); !has(p, "never refused") {
		t.Fatalf("a flooder never refused: %v", p)
	}
	if p, _ := spoolFairProblems(mk(3, 0, 50), from, to); !has(p, "proves nothing") {
		t.Fatalf("too few requests: %v", p)
	}
}

func TestTagAtLeast(t *testing.T) {
	for _, c := range []struct {
		tag, min string
		want     bool
	}{{"v0.38.1", "v0.39.0", false}, {"v0.39.0", "v0.39.0", true}, {"v0.40.0", "v0.39.0", true}, {"", "v0.39.0", true}, {"v0.9.9", "v0.39.0", false}} {
		if got := TagAtLeast(c.tag, c.min); got != c.want {
			t.Errorf("TagAtLeast(%q, %q) = %v", c.tag, c.min, got)
		}
	}
}
