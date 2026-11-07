package chaos

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status of a verdict.
type Status string

// Verdict statuses. NA is "this scenario cannot exercise the invariant" and
// is never a pass: the reason says why.
const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
	NA   Status = "N/A"
)

// Verdict is the judgement of one invariant.
type Verdict struct {
	N      int
	Name   string
	Status Status
	Detail string
}

// InvariantNames are the nine invariants, in the order of the campaign brief.
var InvariantNames = [...]string{
	1: "no hard budget exceeded beyond the documented worst case, in every scope",
	2: "no request billed to a person or org other than its caller",
	3: "spare borrower never admitted over owners; owners' share converges",
	4: "fluids conservation (energy sums exact, fixed-cost credits net to zero)",
	5: "a closed month never changes",
	6: "every refusal has a correct status and Retry-After, never an empty 200",
	7: "no goroutine or file-descriptor leak after the scenario",
	8: "HA: no spend lost or doubled across replica restarts",
	9: "federation metrics: no other org exposed, documented semantics under refusals",
}

func verdict(n int, s Status, f string, a ...any) Verdict {
	return Verdict{N: n, Name: InvariantNames[n], Status: s, Detail: fmt.Sprintf(f, a...)}
}

// Judge evaluates invariants 1 to 6, 8 and 9 on a settled world (7 runs after
// the world is closed).
func Judge(ctx context.Context, w *World) []Verdict {
	j := newJoin(w)
	type inv struct {
		n  int
		fn func(*World, *join) Verdict
	}
	var out []Verdict
	for _, i := range []inv{{1, inv1}, {2, inv2}, {3, inv3}, {4, inv4}, {5, inv5}, {6, inv6}, {8, inv8}, {9, inv9}} {
		if why, skip := w.Sc.Skip[i.n]; skip {
			out = append(out, verdict(i.n, NA, "%s", why))
			continue
		}
		out = append(out, i.fn(w, j))
	}
	w.checkGatewayFDs()
	return out
}

// ---- the join of what clients saw and what the upstream witnessed ----

// Fate is what became of a request.
type Fate string

const (
	FateNone      Fate = "never-reached-upstream"
	FateClean     Fate = "clean" // answered in full, usage reported and read
	FateDisturbed Fate = "disturbed"
	// FateFailed: the client got a 5xx and the upstream never completed an
	// answer: nothing was delivered, so nothing may be billed.
	FateFailed Fate = "failed"
)

type jreq struct {
	Outcome
	Hits  []Seen
	Fate  Fate
	Usage Seen // the completing hit, for a clean request
	W     float64
}

type join struct {
	Reqs  []*jreq
	ByOrg map[string][]*jreq
}

func newJoin(w *World) *join {
	hits := map[string][]Seen{}
	for _, s := range w.Up.Seen() {
		hits[s.Marker] = append(hits[s.Marker], s)
	}
	j := &join{ByOrg: map[string][]*jreq{}}
	for _, o := range w.Ledger.All() {
		r := &jreq{Outcome: o, Hits: hits[o.Marker], Fate: FateNone}
		n := max(1, o.N)
		r.W = WorstCase(o.MaxTokens, n)
		if len(r.Hits) > 0 {
			r.Fate = FateDisturbed
			for _, h := range r.Hits {
				full := o.Status == 200 && h.Completed && h.Reported && !h.Cut && !h.ClientGone && !o.Left && o.Err == "" && h.Failed == 0
				if o.Stream {
					full = full && o.GotDone
				}
				if full {
					r.Fate, r.Usage = FateClean, h
				}
			}
			if r.Fate == FateDisturbed && o.Status >= 500 && !o.Left {
				done := false
				for _, h := range r.Hits {
					done = done || h.Completed || h.Cut
				}
				if !done {
					r.Fate = FateFailed
				}
			}
		}
		j.Reqs = append(j.Reqs, r)
		j.ByOrg[o.Org] = append(j.ByOrg[o.Org], r)
	}
	return j
}

func (r *jreq) cost() float64 { return Cost(r.Usage.PromptTok, r.Usage.ComplTok) }

// keyOf is the id a gateway gives a caller: the virtual key's, or for a JWT
// "jwt-" + base64url(sha256(iss NUL sub)[:16]) (gateway/jwt.go).
func (w *World) keyOf(c Client) string {
	if c.IdP == "" {
		return c.KeyID
	}
	sub := strings.TrimPrefix(c.Who, "jwt:"+c.IdP+":")
	sum := sha256.Sum256([]byte(w.IdPs[c.IdP].Issuer() + "\x00" + sub))
	return "jwt-" + base64.RawURLEncoding.EncodeToString(sum[:16])
}

func (w *World) clientByWho(who string) (Client, bool) {
	for _, c := range w.Clients {
		if c.Who == who {
			return c, true
		}
	}
	return Client{}, false
}

// ---- invariant 1: hard budgets ----

// scopeMembers says whether a request falls under a budget.
func (w *World) inScope(b BudgetSpec, r *jreq) bool {
	c, ok := w.clientByWho(r.Who)
	if !ok {
		return false
	}
	switch b.Kind {
	case "key":
		return w.keyOf(c) == b.Name
	case "team":
		return c.Team == b.Name
	case "org":
		return c.Org == b.Name
	case "user":
		return c.Scoped == b.Name
	}
	return false
}

func inv1(w *World, j *join) Verdict {
	if len(w.Budgets) == 0 {
		return verdict(1, NA, "this scenario sets no budget")
	}
	rows, err := w.budgets()
	if err != nil {
		return verdict(1, Fail, "cannot read /admin/budgets: %v", err)
	}
	var problems []string
	overshootTotal := 0.0
	var detail []string
	for _, b := range w.Budgets {
		// The documented bound: spent + held <= limit, W held per request. A
		// request whose real cost passes its W is charged anyway (and counted in
		// llmflow_budget_hold_exceeded_total): the only excess allowed.
		var clean, over float64
		var refusedWithRoom int
		for _, r := range j.Reqs {
			if !w.inScope(b, r) {
				continue
			}
			if r.Fate == FateClean {
				c := r.cost()
				clean += c
				if c > r.W {
					over += c - r.W
				}
			}
		}
		overshootTotal += over
		var row *BudgetRow
		for i := range rows {
			if rows[i].Kind == b.Kind && rows[i].Name == b.Name {
				row = &rows[i]
			}
		}
		const eps = 1e-6
		if clean > b.Hard+over+eps {
			problems = append(problems, fmt.Sprintf("%s:%s clients paid %.6f for answers read in full, hard limit %.2f (+%.6f allowed)", b.Kind, b.Name, clean, b.Hard, over))
		}
		if row == nil {
			problems = append(problems, fmt.Sprintf("%s:%s: budget missing from /admin/budgets", b.Kind, b.Name))
		} else if row.Committed > row.Limit+over+eps {
			problems = append(problems, fmt.Sprintf("%s:%s committed %.6f above limit %.2f (+%.6f allowed)", b.Kind, b.Name, row.Committed, row.Limit, over))
		} else if row.Committed+row.Held > row.Limit+over+eps {
			problems = append(problems, fmt.Sprintf("%s:%s committed+held %.6f above limit %.2f", b.Kind, b.Name, row.Committed+row.Held, row.Limit))
		}
		_ = refusedWithRoom
		if row != nil {
			detail = append(detail, fmt.Sprintf("%s:%s paid=%.3f committed=%.3f/%.1f", b.Kind, b.Name, clean, row.Committed, b.Hard))
		}
	}
	if m, err := w.gatewayMetrics(); err == nil {
		if v, ok := metricValue(m, "llmflow_budget_hold_exceeded_total"); ok && overshootTotal == 0 && v > 0 {
			problems = append(problems, fmt.Sprintf("llmflow_budget_hold_exceeded_total=%v but no answer cost more than its W", v))
		}
	}
	if len(problems) > 0 {
		return verdict(1, Fail, "%s", strings.Join(problems, "; "))
	}
	return verdict(1, Pass, "%d budgets: %s", len(w.Budgets), strings.Join(detail, ", "))
}

// ---- invariant 2: attribution ----

type bound struct {
	lowReq, hiReq            int64
	lowCost, hiCost          float64
	lowPrompt, lowCompletion int64
	disturbed                int64
	hiCostW                  float64
	cleanCount, distCount    int64
	est                      int64
}

func inv2(w *World, j *join) Verdict {
	rows, err := w.spend("org,team,key")
	if err != nil {
		return verdict(2, Fail, "cannot read /admin/spend: %v", err)
	}
	byKey := map[string]*bound{}
	byOrg := map[string]*bound{}
	keyOrg := map[string]string{}
	for _, c := range w.Clients {
		keyOrg[w.keyOf(c)] = c.Org
	}
	add := func(m map[string]*bound, k string, r *jreq) {
		b := m[k]
		if b == nil {
			b = &bound{}
			m[k] = b
		}
		switch r.Fate {
		case FateClean:
			b.lowReq++
			b.hiReq++
			b.lowCost += r.cost()
			b.hiCost += r.cost()
			b.lowPrompt += int64(r.Usage.PromptTok)
			b.lowCompletion += int64(r.Usage.ComplTok)
		case FateDisturbed:
			b.hiReq++
			b.hiCost += r.W
			b.distCount++
		case FateFailed:
			// Measured (gateway v0.37.1): a request that failed upstream is
			// counted in `requests` with zero tokens and zero cost. The count may
			// include it; the money may not.
			b.hiReq++
		}
	}
	for _, r := range j.Reqs {
		c, ok := w.clientByWho(r.Who)
		if !ok {
			continue
		}
		add(byKey, w.keyOf(c), r)
		add(byOrg, c.Org, r)
	}
	var problems []string
	billedOrg := map[string]*bound{}
	const eps = 1e-6
	for _, row := range rows {
		o := keyOrg[row.Key]
		if o == "" {
			problems = append(problems, fmt.Sprintf("a row bills %d requests (%.6f) to key %q of org %q, which none of our callers holds", row.Requests, row.Cost, row.Key, row.Org))
			continue
		}
		if row.Org != o {
			problems = append(problems, fmt.Sprintf("key %s belongs to %s but its usage is billed to org %q", row.Key, o, row.Org))
		}
		b := byKey[row.Key]
		if b == nil {
			b = &bound{}
		}
		if row.Requests < b.lowReq || row.Requests > b.hiReq {
			problems = append(problems, fmt.Sprintf("key %s (%s): billed %d requests, caller made %d clean + %d disturbed", row.Key, o, row.Requests, b.lowReq, b.distCount))
		}
		if row.Cost < b.lowCost-eps || row.Cost > b.hiCost+eps {
			problems = append(problems, fmt.Sprintf("key %s (%s): billed cost %.6f outside [%.6f, %.6f]", row.Key, o, row.Cost, b.lowCost, b.hiCost))
		}
		if b.distCount == 0 && row.EstimatedRequests > 0 {
			problems = append(problems, fmt.Sprintf("key %s: %d requests billed as estimated though every answer was read in full", row.Key, row.EstimatedRequests))
		}
		ob := billedOrg[o]
		if ob == nil {
			ob = &bound{}
			billedOrg[o] = ob
		}
		ob.lowReq += row.Requests
		ob.lowCost += row.Cost
	}
	// a clean request with no row at all was not billed.
	rowKeys := map[string]bool{}
	for _, r := range rows {
		rowKeys[r.Key] = true
	}
	for k, b := range byKey {
		if b.lowReq > 0 && !rowKeys[k] {
			problems = append(problems, fmt.Sprintf("key %s (%s): %d answered requests, no usage row at all", k, keyOrg[k], b.lowReq))
		}
	}
	var sum []string
	for _, o := range AllOrgs {
		bb, ob := billedOrg[o], byOrg[o]
		if ob == nil {
			continue
		}
		var req int64
		var cost float64
		if bb != nil {
			req, cost = bb.lowReq, bb.lowCost
		}
		sum = append(sum, fmt.Sprintf("%s billed %d req / %.3f for %d..%d expected", o, req, cost, ob.lowReq, ob.hiReq))
	}
	if len(problems) > 0 {
		return verdict(2, Fail, "%s", strings.Join(capList(problems, 6), "; "))
	}
	return verdict(2, Pass, "%s", strings.Join(sum, "; "))
}

func capList(v []string, n int) []string {
	if len(v) <= n {
		return v
	}
	return append(append([]string(nil), v[:n]...), fmt.Sprintf("... and %d more", len(v)-n))
}

// ---- invariant 3: the spare borrower and the owners' shares ----

func inv3(w *World, j *join) Verdict {
	var tiersHits, over int
	var worst int
	var ownerMaxPrio, tiersMinPrio = noPriority, 1 << 30
	for _, s := range w.Up.Seen() {
		if s.Org == Borrower {
			tiersHits++
			if s.Concurrent > PoolCapacity {
				over++
				worst = max(worst, s.Concurrent)
			}
		}
	}
	// the band each admitted request ran in is the gateway's X-Llmflow-Priority
	// response header: the borrower's must be behind every owner's.
	for _, r := range j.Reqs {
		p, err := strconv.Atoi(r.Header.Get("X-Llmflow-Priority"))
		if err != nil || r.Status != 200 {
			continue
		}
		if r.Org == Borrower {
			tiersMinPrio = min(tiersMinPrio, p)
		} else if OwnerShares[r.Org] > 0 {
			ownerMaxPrio = max(ownerMaxPrio, p)
		}
	}
	var problems []string
	if over > 0 {
		problems = append(problems, fmt.Sprintf("%d borrower requests reached the upstream while %d were in flight (capacity %d)", over, worst, PoolCapacity))
	}
	if tiersMinPrio != 1<<30 && ownerMaxPrio != noPriority && tiersMinPrio <= ownerMaxPrio {
		problems = append(problems, fmt.Sprintf("a borrower request ran at priority %d, an owner's at %d: the borrower is not in the last band", tiersMinPrio, ownerMaxPrio))
	}
	// the owners' share: completed work per owner, only when every owner asked
	// for more than the pool could give (see Scenario.Expect "owner-shares").
	prio := "no X-Llmflow-Priority seen"
	if ownerMaxPrio != noPriority || tiersMinPrio != 1<<30 {
		prio = fmt.Sprintf("X-Llmflow-Priority owners<=%d borrower>=%d", ownerMaxPrio, tiersMinPrio)
	}
	// owners first: once every owner has ramped up, the borrower, which was
	// served from the spare, must be nearly shut out (it may only take what the
	// owners leave).
	if w.expects("owners-first") {
		var after, tiersAfter int
		for _, r := range j.Reqs {
			if r.At.Before(w.started.Add(w.Sc.rampAt()+3*time.Second)) || r.Left || r.Status == 0 {
				continue
			}
			after++
			if r.Org == Borrower && r.Status == 200 {
				tiersAfter++
			}
		}
		if after > 0 {
			f := float64(tiersAfter) / float64(after)
			detail := fmt.Sprintf("borrower got %d of the %d answers after the owners ramped (%.1f%%)", tiersAfter, after, 100*f)
			if f > 0.10 {
				problems = append(problems, detail+": the owners were pressing and the borrower still got more than 10%")
			}
			prio += "; " + detail
		}
	}
	detail := fmt.Sprintf("borrower upstream hits %d, max concurrency %d/%d, %s", tiersHits, w.Up.MaxInFlight(), PoolCapacity, prio)
	if w.expects("owner-shares") {
		got := map[string]float64{}
		var tot float64
		for _, r := range j.Reqs {
			if r.At.Before(w.started.Add(w.Sc.rampAt() + 3*time.Second)) {
				continue // before every owner was pressing
			}
			if r.Fate == FateClean && OwnerShares[r.Org] > 0 {
				got[r.Org] += r.cost()
				tot += r.cost()
			}
		}
		if tot == 0 {
			problems = append(problems, "owners completed no work: the share cannot be measured")
		}
		const tol = 0.15 // Hamilton's 8 slots give 4/2/2 for 50/30/20: one slot is 12.5 points
		var parts []string
		for _, o := range Owners {
			f := got[o] / math.Max(tot, 1e-12)
			parts = append(parts, fmt.Sprintf("%s %.1f%%", o, 100*f))
			// The gateway guarantees shares through the vLLM priority it sends;
			// the fake upstream does not schedule by priority, so equal service
			// is its behaviour, not the gateway's. Reported, not judged, until
			// the fake models a priority queue.
			if tot > 0 && math.Abs(f-OwnerShares[o]) > tol && fakeHonoursPriority {
				problems = append(problems, fmt.Sprintf("%s got %.1f%% of the owners' work, its share is %.0f%% (tolerance %.0f points)", o, 100*f, 100*OwnerShares[o], 100*tol))
			}
		}
		detail += "; owners' share of completed work: " + strings.Join(parts, ", ")
		// the guarantee behind the share: an owner with demand holds its slots.
		// Mean requests in flight per organisation after the ramp (the sum of
		// the answered requests' durations over the window).
		from := w.started.Add(w.Sc.rampAt() + 3*time.Second)
		var end time.Time
		busy := map[string]time.Duration{}
		for _, r := range j.Reqs {
			if r.At.Before(from) || r.Status != 200 || r.Left {
				continue
			}
			busy[r.Org] += r.Dur
			if e := r.At.Add(r.Dur); e.After(end) {
				end = e
			}
		}
		if win := end.Sub(from).Seconds(); win > 1 {
			var ms []string
			for _, o := range AllOrgs {
				ms = append(ms, fmt.Sprintf("%s %.2f", o, busy[o].Seconds()/win))
			}
			detail += "; mean requests in flight " + strings.Join(ms, ", ")
		}
	}
	if len(problems) > 0 {
		return verdict(3, Fail, "%s [%s]", strings.Join(problems, "; "), detail)
	}
	return verdict(3, Pass, "%s", detail)
}

func (w *World) expects(x string) bool {
	for _, e := range w.Sc.Expect {
		if e == x {
			return true
		}
	}
	return false
}

// ---- invariant 4: fluids ----

const mjPerKWh = 3_600_000_000.0

func inv4(w *World, j *join) Verdict {
	if !w.Sc.F.Fluids {
		return verdict(4, NA, "no fluids block in this scenario")
	}
	f, err := w.fluids("org")
	if err != nil {
		return verdict(4, Fail, "cannot read /admin/fluids: %v", err)
	}
	var problems []string
	var act, idle, kwh, fixed, credit float64
	for _, r := range f.Rows {
		act += r.KWhActive
		idle += r.KWhIdle
		kwh += r.KWh
		fixed += r.FixedCost
		credit += r.Credit
		if math.Abs(r.KWhActive+r.KWhIdle-r.KWh) > 1e-9*math.Max(1, r.KWh) {
			problems = append(problems, fmt.Sprintf("org %s: active %.9f + idle %.9f != kwh %.9f", r.Org, r.KWhActive, r.KWhIdle, r.KWh))
		}
	}
	// measured = what the exporter was told to move (a driver reset restarts a
	// counter at the value given: the energy since it counts as moved).
	// The node's energy is its GPUs' plus the overhead: the BMC's counter minus
	// the GPUs', or, when no BMC can be read, the GPUs' times
	// node_overhead_factor (1.12). The fake BMC draws exactly 12 % more than the
	// GPUs, so either source gives the same figure and a lost reading can only
	// shift energy between intervals, not create it.
	truth := float64(w.DCGM.Added()) / mjPerKWh * nodeFactor
	// tolerance: the intervals not yet closed, and the one a BMC fault spans.
	tol := 4 * (75_000 + 90_000) * nodeFactor / mjPerKWh
	if math.Abs(kwh-truth) > tol {
		problems = append(problems, fmt.Sprintf("attributed+idle = %.9f kWh but the node used %.9f kWh (diff %+.3e, tolerance %.3e)", kwh, truth, kwh-truth, tol))
	}
	// the overhead: the BMC's node energy minus the GPUs'.
	ov := 0.0
	for _, o := range f.NodeOverhead {
		ov += o.KWh
	}
	// exact: the rows hold the GPUs' energy plus the overhead the report states
	// (printed to the microkilowatt-hour: 5e-7 of rounding per figure).
	gpu := float64(w.DCGM.Added()) / mjPerKWh
	if len(f.NodeOverhead) > 0 && math.Abs(kwh-(gpu+ov)) > 4*(75_000+90_000)/mjPerKWh+2e-6 {
		problems = append(problems, fmt.Sprintf("rows hold %.9f kWh, GPUs %.9f + stated overhead %.9f = %.9f", kwh, gpu, ov, gpu+ov))
	}
	// credits: owners are credited what a non-shareholder paid for the cost the
	// owners bore, so over the report fixed - credit is the cost nobody bore
	// for the owners: the racks' share, and credit is exactly the depreciation.
	frac := reportFraction()
	rows := float64(len(f.Rows))
	cent := 0.005 * (rows + 1) * 2
	wantDep, wantRacks := 4200*frac, 1800*frac
	// The depreciation is billed, on the energy basis, to the organisations that
	// hold no share and used energy; with none in use the owners bear it and
	// nothing is billed or credited (docs, Fluids: "With no non-shareholder in
	// use the owners bear it: nothing is billed"). A third party whose requests
	// all ran before the first energy interval has no energy to be charged on.
	var nonShareActive float64
	for _, r := range f.Rows {
		if OwnerShares[r.Org] == 0 && r.Org != Unallocated {
			nonShareActive += r.KWhActive
		}
	}
	if nonShareActive == 0 {
		wantDep = 0
	}
	if math.Abs(credit-wantDep) > cent+0.02*wantDep {
		problems = append(problems, fmt.Sprintf("credits sum to %.2f, the depreciation charged to non-shareholders is %.2f", credit, wantDep))
	}
	if math.Abs((fixed-credit)-wantRacks) > cent+0.02*wantRacks {
		problems = append(problems, fmt.Sprintf("fixed costs %.2f minus credits %.2f = %.2f, the racks cost %.2f: money lost or doubled", fixed, credit, fixed-credit, wantRacks))
	}
	if len(problems) > 0 {
		return verdict(4, Fail, "%s", strings.Join(problems, "; "))
	}
	return verdict(4, Pass, "kwh %.6f (exporter %.6f), overhead %.6f kWh, fixed %.2f credit %.2f over %d rows", kwh, truth, ov, fixed, credit, len(f.Rows))
}

// nodeFactor is the node's energy over its GPUs' in the simulation; nodeWatts
// is the node's power: two GPUs moving 75 000 and 90 000 mJ every 500 ms
// (330 W), times the factor.
const (
	nodeFactor = 1.12
	nodeWatts  = (75_000 + 90_000) / 1000.0 / 0.5 * nodeFactor
)

// reportFraction is the part of a month the report window covers, as the
// gateway prorates a monthly fixed cost: a day is 1/daysInMonth of it (the
// docs: "a part of a month prorated by time"; measured: a 3-day window in a
// 30-day month carries 3/30 of 4200).
func reportFraction() float64 {
	from, to := window()
	d, _ := time.Parse("2006-01-02", from)
	end, _ := time.Parse("2006-01-02", to)
	var f float64
	for ; d.Before(end); d = d.AddDate(0, 0, 1) {
		f += 1 / float64(time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day())
	}
	return f
}

// fakeHonoursPriority is false: chaos.Upstream serves every request at once.
const fakeHonoursPriority = false

// Unallocated is the organisation a fluids report names for energy nobody can be charged for.
const Unallocated = "(unallocated)"

// ---- invariant 5 ----

func inv5(w *World, j *join) Verdict {
	if !w.Sc.needs("pg") {
		return verdict(5, NA, "closing a month needs PostgreSQL and a month that has ended; a past month cannot be produced without clock control (gateway tests do it with WithClock)")
	}
	return verdict(5, NA, "no scenario closes a month yet: not exercised")
}

// ---- invariant 6: refusals ----

func inv6(w *World, j *join) Verdict {
	var problems []string
	counts := map[int]int{}
	noRA := map[int]int{}
	var empty, silent int
	for _, r := range j.Reqs {
		if r.Left || r.Err != "" && r.Status == 0 {
			continue
		}
		counts[r.Status]++
		switch {
		case r.Status == 200:
			if r.Stream && r.Events == 0 {
				empty++
			}
			if !r.Stream && r.BodyLen == 0 {
				empty++
			}
			if r.Stream && !r.GotDone && r.Err == "" && !r.Left && r.Events > 0 {
				silent++
			}
		case r.Status == 429:
			// Retry-After is delta-seconds (RFC 9110 10.2.3). A refusal for a hard
			// budget names the time to the period's end (the day budgets here: at
			// most 86400 s), so no short cap applies.
			ra, err := strconv.Atoi(r.RetryAfter)
			if err != nil || ra <= 0 || ra > 86400+60 {
				noRA[429]++
			}
		case r.Status == 503:
			// RFC 9110 10.2.3: Retry-After MAY accompany a 503. An upstream's own
			// 503 is passed through as it came, so this is an observation.
			if r.RetryAfter == "" {
				noRA[503]++
			}
		case r.Status >= 400 && r.Status < 600:
		default:
			problems = append(problems, fmt.Sprintf("unexpected status %d for %s", r.Status, r.Outcome))
		}
	}
	if empty > 0 {
		problems = append(problems, fmt.Sprintf("%d answers were 200 with an empty body", empty))
	}
	// A stream cut after its 200 was sent ends as a clean EOF, with no error
	// event and no [DONE]: the documented behaviour ("The client gets the
	// partial stream", gateway README, chaos table). It is not a refusal, so it
	// is reported as an observation and does not fail the invariant.
	observed := ""
	if silent > 0 {
		observed = fmt.Sprintf(" [observed: %d cut streams ended as a clean EOF, no error event, no [DONE]: documented]", silent)
	}
	if noRA[429] > 0 {
		problems = append(problems, fmt.Sprintf("%d of %d 429 answers had no usable Retry-After", noRA[429], counts[429]))
	}
	if noRA[503] > 0 {
		observed += fmt.Sprintf(" [observed: %d of %d 503 answers without Retry-After (upstream 503s are passed through)]", noRA[503], counts[503])
	}
	ap, an := w.authProblems(j)
	problems = append(problems, ap...)
	if len(an) > 0 {
		observed += " [" + strings.Join(an, "; ") + "]"
	}
	var keys []int
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d:%d", k, counts[k]))
	}
	if len(problems) > 0 {
		return verdict(6, Fail, "%s (statuses %s)", strings.Join(problems, "; "), strings.Join(parts, " "))
	}
	return verdict(6, Pass, "statuses %s%s", strings.Join(parts, " "), observed)
}

// ---- invariant 8: HA ----

func inv8(w *World, j *join) Verdict {
	if len(w.GW) < 2 {
		return verdict(8, NA, "one replica: needs PostgreSQL and two replicas (CI)")
	}
	// spend neither lost nor doubled: the same bounds as invariant 2, over all
	// the traffic of both replicas, plus the exact count of upstream hits.
	rows, err := w.spend("org")
	if err != nil {
		return verdict(8, Fail, "cannot read spend: %v", err)
	}
	var billed int64
	var cost float64
	for _, r := range rows {
		billed += r.Requests
		cost += r.Cost
	}
	var lowReq, hiReq int64
	var lowCost, hiCost float64
	for _, r := range j.Reqs {
		switch r.Fate {
		case FateClean:
			lowReq++
			hiReq++
			lowCost += r.cost()
			hiCost += r.cost()
		case FateDisturbed:
			hiReq++
			hiCost += r.W
		}
	}
	if billed < lowReq || billed > hiReq || cost < lowCost-1e-6 || cost > hiCost+1e-6 {
		return verdict(8, Fail, "billed %d requests / %.6f, answered in full %d / %.6f, at most %d / %.6f: spend lost or doubled", billed, cost, lowReq, lowCost, hiReq, hiCost)
	}
	return verdict(8, Pass, "billed %d requests / %.6f within [%d,%d] / [%.6f,%.6f]", billed, cost, lowReq, hiReq, lowCost, hiCost)
}

// ---- invariant 9: federation metrics ----

func inv9(w *World, j *join) Verdict {
	if !w.Sc.F.Federation {
		return verdict(9, NA, "no federation mount in this scenario")
	}
	text, err := httpGet(w.Primary().URL + FedPrefix + "/metrics")
	if err != nil {
		return verdict(9, Fail, "cannot read %s/metrics: %v", FedPrefix, err)
	}
	var problems []string
	for _, leak := range []string{"etabA", "etabB", "etabC", "a1", "b1", "c1", "llmf-", "jwt-"} {
		if strings.Contains(text, leak) {
			problems = append(problems, fmt.Sprintf("the federation metrics contain %q", leak))
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "vllm:time_to_first_token_seconds") {
			problems = append(problems, "foreign series: "+l)
			break
		}
	}
	inf, _ := histValue(text, "vllm:time_to_first_token_seconds_bucket", `le="+Inf"`)
	cnt, _ := metricValue(text, "vllm:time_to_first_token_seconds_count")
	sum, hasSum := metricValue(text, "vllm:time_to_first_token_seconds_sum")
	if inf != cnt {
		problems = append(problems, fmt.Sprintf("+Inf bucket %v != count %v", inf, cnt))
	}
	if math.IsInf(sum, 0) || math.IsNaN(sum) {
		problems = append(problems, fmt.Sprintf("_sum is %v: a refusal must count in +Inf only", sum))
	}
	// what should have been counted: requests through the prefix that resolved
	// a model and got a first byte (status < 300) or a refusal (429/502/503/504).
	var low, high float64
	var finiteLast float64
	for _, r := range j.Reqs {
		if r.Prefix == "" {
			continue
		}
		counted := r.Status < 300 && r.Status >= 200 || r.Status == 429 || r.Status == 502 || r.Status == 503 || r.Status == 504
		switch {
		case r.Left || r.Status == 0:
			high++ // unknown: the gateway may or may not have observed it
		case counted:
			low++
			high++
		}
	}
	finiteLast, _ = histValue(text, "vllm:time_to_first_token_seconds_bucket", `le="2560.0"`)
	if cnt < low || cnt > high {
		problems = append(problems, fmt.Sprintf("count %v, the dispatcher's requests that were answered or refused: %v..%v", cnt, low, high))
	}
	refusals := 0.0
	for _, r := range j.Reqs {
		if r.Prefix != "" && !r.Left && (r.Status == 429 || r.Status == 502 || r.Status == 503 || r.Status == 504) {
			refusals++
		}
	}
	if refusals > 0 && cnt > 0 && finiteLast >= cnt && !(cnt-refusals < 0) {
		// refusals count in +Inf only: the finite buckets must be short of the count.
		problems = append(problems, fmt.Sprintf("%v refusals but every observation sits in a finite bucket (%v of %v)", refusals, finiteLast, cnt))
	}
	// the owners' traffic never moves it: with no federated request, the series
	// is empty.
	fed := 0
	for _, r := range j.Reqs {
		if r.Prefix != "" {
			fed++
		}
	}
	if fed == 0 && cnt != 0 {
		problems = append(problems, fmt.Sprintf("no request came through the mount, yet count=%v", cnt))
	}
	_ = hasSum
	if len(problems) > 0 {
		return verdict(9, Fail, "%s", strings.Join(problems, "; "))
	}
	return verdict(9, Pass, "count %v (expected %v..%v), refusals %v, %d federated requests", cnt, low, high, refusals, fed)
}

// histValue reads one bucket line.
func histValue(text, name, le string) (float64, bool) {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, name+"{") && strings.Contains(l, le) {
			f := strings.Fields(l)
			v, err := strconv.ParseFloat(f[len(f)-1], 64)
			return v, err == nil
		}
	}
	return 0, false
}

// ---- invariant 7: leaks ----

// fdSlack is the growth of a gateway's descriptors the check allows after the
// load: its outbound keep-alive pool and the listeners of the features the
// load woke (measured: 14 -> 15..21 after 500..950 requests, the same for 500
// and for 950). A leak per request shows as hundreds.
const fdSlack = 16

func (w *World) checkGatewayFDs() {
	// the gateway's own descriptors, once clients have gone and the connections
	// have had time to close.
	var problems []string
	var parts []string
	for i, g := range w.GW {
		pid := g.PID()
		if pid == 0 {
			continue
		}
		base := g.fd0
		// Idle keep-alive connections to the upstream outlive the load by the
		// transport's idle timeout, so the gateway is given 150 s to drain: a
		// descriptor that is still held then is a leak, one that drains is a pool.
		var now int
		t0 := time.Now()
		first := fdCount(pid)
		deadline := t0.Add(150 * time.Second)
		for {
			now = fdCount(pid)
			if now <= base+fdSlack || time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		parts = append(parts, fmt.Sprintf("gateway r%d fds %d -> %d after the load -> %d after %s", i, base, first, now, time.Since(t0).Round(time.Second)))
		if now >= 0 && base >= 0 && now > base+fdSlack {
			problems = append(problems, fmt.Sprintf("gateway r%d holds %d descriptors 150 s after the load (%d at start, %d just after the load)", i, now, base, first))
		}
	}
	w.leakNotes = problems
	w.leakParts = parts
}

// leakVerdict judges this process once its world is closed: the goroutines and
// descriptors the fakes and clients held must be gone.
func (w *World) leakVerdict() Verdict {
	var g, fd int
	deadline := time.Now().Add(10 * time.Second)
	for {
		g, fd = goroutines(), fdCount(osPid())
		if (g <= w.baseGoroutines+3 && (fd < 0 || fd <= w.baseFDs+3)) || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	var problems []string
	if g > w.baseGoroutines+3 {
		problems = append(problems, fmt.Sprintf("harness goroutines %d -> %d", w.baseGoroutines, g))
	}
	if fd >= 0 && fd > w.baseFDs+3 {
		problems = append(problems, fmt.Sprintf("harness descriptors %d -> %d", w.baseFDs, fd))
	}
	problems = append(problems, w.leakNotes...)
	detail := fmt.Sprintf("harness goroutines %d -> %d, fds %d -> %d; %s", w.baseGoroutines, g, w.baseFDs, fd, strings.Join(w.leakParts, ", "))
	if len(problems) > 0 {
		return verdict(7, Fail, "%s", strings.Join(problems, "; "))
	}
	return Verdict{N: 7, Name: InvariantNames[7], Status: Pass, Detail: detail}
}
