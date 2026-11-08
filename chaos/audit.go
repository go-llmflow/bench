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

// ControlSince is set on a scenario that is a positive control: it must FAIL on
// gateway releases older than this tag and PASS from it on.
//
// ExpectedToFail says whether, for the release tag, the scenario is such a
// control that must fail. An unparsable tag is never a control run.
func ExpectedToFail(sc *Scenario, tag string) bool {
	if sc == nil || sc.ControlSince == "" {
		return false
	}
	a, ok1 := semver(tag)
	b, ok2 := semver(sc.ControlSince)
	if !ok1 || !ok2 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func semver(t string) (v [3]int, ok bool) {
	n, err := fmt.Sscanf(t, "v%d.%d.%d", &v[0], &v[1], &v[2])
	return v, err == nil && n == 3
}

// AuditTag is Audit for a campaign against the release tag: the controls that
// must fail on it are required to fail (a control that passes on the old
// release proves nothing) and their failures are not problems.
func AuditTag(rs []Result, seeds []int, tag string) []string {
	var keep []Result
	var out []string
	for _, r := range rs {
		sc := Find(r.Scenario)
		if !ExpectedToFail(sc, tag) {
			keep = append(keep, r)
			continue
		}
		failed := false
		for _, v := range r.Verdicts {
			failed = failed || v.Status == Fail
		}
		if !failed && r.Err == "" && r.Skipped == "" {
			out = append(out, fmt.Sprintf("%s/%d: positive control did not fail on %s", r.Scenario, r.Seed, tag))
		}
		if r.Err != "" || r.Skipped != "" || r.Requests == 0 {
			out = append(out, fmt.Sprintf("%s/%d: control run broke (%s%s)", r.Scenario, r.Seed, r.Err, r.Skipped))
		}
	}
	out = append(out, Audit(keep, seeds)...)
	// the controls removed from keep are not "missing": drop those lines
	var res []string
	for _, l := range out {
		miss := false
		for _, r := range rs {
			if ExpectedToFail(Find(r.Scenario), tag) && l == fmt.Sprintf("%s/%d: missing from the campaign", r.Scenario, r.Seed) {
				miss = true
			}
		}
		if !miss {
			res = append(res, l)
		}
	}
	sort.Strings(res)
	return res
}

// HasFail reports whether an invariant of the run failed.
func (r Result) HasFail() bool {
	for _, v := range r.Verdicts {
		if v.Status == Fail {
			return true
		}
	}
	return false
}

// TagAtLeast reports whether release tag is min or later; an empty or
// unparsable tag is taken as the latest release.
func TagAtLeast(tag, min string) bool {
	a, ok1 := semver(tag)
	b, ok2 := semver(min)
	if !ok1 || !ok2 {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}
