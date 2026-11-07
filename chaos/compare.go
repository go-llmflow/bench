package chaos

import (
	"fmt"
	"sort"
)

// Compare lists every scenario x seed x invariant whose status differs between
// two campaigns (a: one gateway release, b: another), and the runs only one of
// them has. Details are not compared: they carry timings and counts.
func Compare(a, b []Result) []string {
	runKey := func(r Result) string { return fmt.Sprintf("%s/%d", r.Scenario, r.Seed) }
	byRun := func(rs []Result) map[string]Result {
		m := map[string]Result{}
		for _, r := range rs {
			m[runKey(r)] = r
		}
		return m
	}
	am, bm := byRun(a), byRun(b)
	var out []string
	for k, ra := range am {
		rb, ok := bm[k]
		if !ok {
			out = append(out, k+": only in the first campaign")
			continue
		}
		bv := map[int]Verdict{}
		for _, v := range rb.Verdicts {
			bv[v.N] = v
		}
		for _, v := range ra.Verdicts {
			if w, ok := bv[v.N]; ok && w.Status != v.Status {
				out = append(out, fmt.Sprintf("%s invariant %d: %s -> %s | %s | %s", k, v.N, v.Status, w.Status, v.Detail, w.Detail))
			}
		}
	}
	for k := range bm {
		if _, ok := am[k]; !ok {
			out = append(out, k+": only in the second campaign")
		}
	}
	sort.Strings(out)
	return out
}
