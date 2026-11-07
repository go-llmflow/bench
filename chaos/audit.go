package chaos

import (
	"fmt"
	"sort"
)

// expectedNA reports whether sc is entitled to an N/A for invariant n: either
// it lists the invariant in Skip, or it lacks what the invariant judges.
func expectedNA(sc *Scenario, n int) bool {
	if _, ok := sc.Skip[n]; ok {
		return true
	}
	switch n {
	case 1:
		return !sc.F.Budgets
	case 4:
		return !sc.F.Fluids
	case 5:
		for _, f := range sc.Faults {
			if f.Kind == "month-close" {
				return false
			}
		}
		return true
	case 8:
		return sc.Replicas < 2
	case 9:
		return !sc.F.Federation
	}
	return false
}

// Audit judges a whole campaign: it returns one line per problem. A problem
// is a failed invariant, a run that broke, was skipped or sent nothing, an N/A
// the scenario is not entitled to (a judge that stopped judging), and a
// scenario x seed of the catalogue that has no result at all.
func Audit(rs []Result, seeds []int) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rs {
		key := fmt.Sprintf("%s/%d", r.Scenario, r.Seed)
		if seen[key] {
			out = append(out, key+": two results")
		}
		seen[key] = true
		switch {
		case r.Err != "":
			out = append(out, key+": ERROR "+r.Err)
			continue
		case r.Skipped != "":
			out = append(out, key+": skipped: "+r.Skipped)
			continue
		case r.Requests == 0:
			out = append(out, key+": sent no request")
			continue
		}
		sc := Find(r.Scenario)
		if sc == nil {
			out = append(out, key+": not in the catalogue")
			continue
		}
		have := map[int]bool{}
		for _, v := range r.Verdicts {
			have[v.N] = true
			switch {
			case v.Status == Fail:
				out = append(out, fmt.Sprintf("%s: invariant %d FAIL: %s", key, v.N, v.Detail))
			case v.Status == NA && !expectedNA(sc, v.N):
				out = append(out, fmt.Sprintf("%s: invariant %d unexpected N/A: %s", key, v.N, v.Detail))
			}
		}
		for n := 1; n < len(InvariantNames); n++ {
			if !have[n] {
				out = append(out, fmt.Sprintf("%s: invariant %d has no verdict", key, n))
			}
		}
	}
	for _, sc := range Catalogue() {
		for _, s := range seeds {
			if k := fmt.Sprintf("%s/%d", sc.Name, s); !seen[k] {
				out = append(out, k+": missing from the campaign")
			}
		}
	}
	sort.Strings(out)
	return out
}
