package chaos

import "time"

func s(d float64) time.Duration { return time.Duration(d * float64(time.Second)) }

// steady is a moderate load of every organisation, the borrower included.
func steady() map[string]OrgLoad {
	ld := OrgLoad{Workers: 2, Think: 200 * time.Millisecond, StreamPct: 50, Tokens: []int{16, 32}, Words: []int{6, 10, 14}}
	return map[string]OrgLoad{
		"etabA": ld,
		"etabB": {Workers: 2, Think: 200 * time.Millisecond, StreamPct: 50, Tokens: []int{16, 32}, Words: []int{20, 24}},
		"etabC": {Workers: 1, Think: 300 * time.Millisecond, StreamPct: 50, Tokens: []int{16, 32}, Words: []int{40}},
		"tiers": {Workers: 2, Think: 200 * time.Millisecond, StreamPct: 50, Tokens: []int{16, 32}, Words: []int{30}},
	}
}

func full() Features {
	return Features{Fluids: true, Federation: true, IdPs: true, BMC: true, Budgets: true}
}

func with(sc Scenario) *Scenario { return &sc }

// Catalogue is every scenario, as data. A scenario is replayed by name and seed.
func Catalogue() []*Scenario {
	up := func(name, doc string, f ...FaultAt) *Scenario {
		return with(Scenario{Name: name, Doc: doc, F: full(), Duration: s(14), Load: steady(), Faults: f,
			Expect: nil})
	}
	cat := []*Scenario{
		with(Scenario{Name: "baseline", Doc: "no fault: every invariant must hold on the plain deployment", F: full(), Duration: s(14), Load: steady(),
			Bursts: []Burst{{At: s(5), Org: "etabA", N: 12, Edge: true, Tokens: 32}}}),
		up("upstream-5xx", "the upstream answers 502 to half of the requests", FaultAt{Kind: "upstream-5xx", At: s(3), For: s(6), Arg: 502, Arg2: 2}),
		up("upstream-5xx-all", "the upstream answers 500 to everything for a while", FaultAt{Kind: "upstream-5xx", At: s(3), For: s(5), Arg: 500}),
		up("upstream-hang", "the upstream accepts and says nothing (the gateway's header timeout is 6 s)", FaultAt{Kind: "upstream-hang", At: s(3), For: s(5), Arg: 30}),
		up("upstream-slow-tokens", "300 ms between tokens", FaultAt{Kind: "upstream-slow-tokens", At: s(3), For: s(6), Arg: 300}),
		up("upstream-cut-sse", "the SSE stream is cut after 3 events", FaultAt{Kind: "upstream-cut-sse", At: s(3), For: s(6), Arg: 3}),
		up("upstream-wrong-usage", "the upstream reports 5x the completion tokens, then none", FaultAt{Kind: "upstream-wrong-usage", At: s(3), For: s(3), Arg: 5},
			FaultAt{Kind: "upstream-no-usage", At: s(8), For: s(3)}),
		with(Scenario{Name: "models-down-at-start", Doc: "/v1/models is unavailable at start: requests under a budget must be refused, never admitted, until it is read",
			F: Features{Fluids: true, Federation: true, IdPs: true, BMC: true, Budgets: true, ModelsDownAtStart: true}, Duration: s(12), Load: steady(),
			Faults: []FaultAt{{Kind: "models-up", At: s(5)}}}),
		up("preemption-burst", "vLLM preempts: the waiting queue fills and the preemption counter jumps", FaultAt{Kind: "preemption-burst", At: s(3), For: s(5), Arg: 40, Arg2: 20}),
		with(Scenario{Name: "client-disconnects", Doc: "clients leave at every stream phase, 40 % of the streams",
			F: full(), Duration: s(14), Load: map[string]OrgLoad{
				"etabA": {Workers: 3, Think: 100 * time.Millisecond, StreamPct: 90, LeavePct: 40, Tokens: []int{16, 32}, Words: []int{8}},
				"etabB": {Workers: 2, Think: 100 * time.Millisecond, StreamPct: 90, LeavePct: 40, Tokens: []int{16, 32}, Words: []int{20}},
				"etabC": {Workers: 1, Think: 100 * time.Millisecond, StreamPct: 90, LeavePct: 40, Tokens: []int{16}, Words: []int{40}},
				"tiers": {Workers: 2, Think: 100 * time.Millisecond, StreamPct: 90, LeavePct: 40, Tokens: []int{16}, Words: []int{30}}}}),
		with(Scenario{Name: "retry-storm-n-best-of", Doc: "clients retry refusals at once; part of the requests ask n=2 and best_of=2", F: full(), Duration: s(14),
			Load: map[string]OrgLoad{
				"etabA": {Workers: 4, StreamPct: 30, NPct: 40, RetryMax: 4, Tokens: []int{16, 32}, Words: []int{8}},
				"etabB": {Workers: 4, StreamPct: 30, NPct: 40, RetryMax: 4, Tokens: []int{16, 32}, Words: []int{20}},
				"etabC": {Workers: 2, StreamPct: 30, NPct: 40, RetryMax: 4, Tokens: []int{16}, Words: []int{40}},
				"tiers": {Workers: 4, StreamPct: 30, NPct: 40, RetryMax: 4, Tokens: []int{16}, Words: []int{30}}}}),
		with(Scenario{Name: "budget-edge-burst", Doc: "concurrent bursts at the edge of the hard budget of a key, a team and an org", F: full(), Duration: s(12), Load: steady(),
			Bursts: []Burst{{At: s(2), Org: "etabA", N: 20, Edge: true, Tokens: 32}, {At: s(4), Org: "etabB", N: 30, Tokens: 64},
				{At: s(6), Org: "etabC", N: 40, Tokens: 64}, {At: s(8), Org: "tiers", N: 30, Tokens: 64}, {At: s(9), Org: "etabA", N: 20, Edge: true, Tokens: 16}}}),
		with(Scenario{Name: "idp-jwks-down", Doc: "the IdPs' discovery and JWKS vanish mid-run, and come back", F: full(), Duration: s(14), Load: steady(),
			Faults: []FaultAt{{Kind: "idp-down", Target: "idpA", At: s(3), For: s(5)}, {Kind: "idp-down", Target: "fed", At: s(4), For: s(5)}}}),
		with(Scenario{Name: "idp-jwks-rotated", Doc: "an IdP rotates its signing key mid-run, the old one leaves the key set", F: full(), Duration: s(14), Load: steady(),
			Faults: []FaultAt{{Kind: "idp-rotate", Target: "idpA", At: s(1), Arg: 1}, {Kind: "idp-rotate", Target: "fed", At: s(2), Arg: 0}}}),
		with(Scenario{Name: "idp-schac-spoof", Doc: "POSITIVE CONTROL (v0.38.0 fix): a federation token whose name is scoped to another institution but states the accepted schac value must be refused", F: full(), Duration: s(10), Load: steady(),
			Expect: []string{"schac-spoof"}, ControlSince: "v0.38.0"}),
		with(Scenario{Name: "control-admin-users", Doc: "POSITIVE CONTROL (v0.38.0 fix): an org admin of B creates a bare and a foreign-domain user through POST /admin/users; both must be refused and the gateway must restart",
			F: Features{IdPs: true, SCIM: true}, Duration: s(4), Load: map[string]OrgLoad{"etabA": {Workers: 1, Think: 200 * time.Millisecond, Tokens: []int{16}, Words: []int{6}}},
			Expect: []string{"admin-users"}, ControlSince: "v0.38.0"}),
		with(Scenario{Name: "control-spare-guard", Doc: "POSITIVE CONTROL (v0.38.0 fix): the owners keep the pool full, a guarded spare-only borrower is refused 429: a refused request must cost no guard-model call, and a guard call never runs at priority 0",
			F: Features{Guard: true}, Duration: s(14), UpstreamLatency: 400 * time.Millisecond,
			Load: map[string]OrgLoad{
				"etabA": {Workers: 5, Think: 100 * time.Millisecond, Tokens: []int{16}, Words: []int{8}},
				"etabB": {Workers: 3, Think: 100 * time.Millisecond, Tokens: []int{16}, Words: []int{8}},
				"tiers": {Workers: 6, Think: 50 * time.Millisecond, Tokens: []int{16}, Words: []int{20}}},
			Skip:   map[int]string{3: "the guard model is a second upstream and pool: the borrower-over-owners judge reads the main upstream's concurrency"},
			Expect: []string{"spare-guard"}, ControlSince: "v0.38.0"}),
		with(Scenario{Name: "control-fallback-spare", Doc: "POSITIVE CONTROL (v0.38.0 fix): the owners keep the main pool one slot short of full, so a borrower takes its last slot and looks for a fallback; the fallback pool's vLLM queue is saturated: a spare-only borrower must not take a slot there",
			F: Features{Fallback: true}, Duration: s(12), UpstreamLatency: time.Second,
			Load: map[string]OrgLoad{
				"etabA": {Workers: 6, Tokens: []int{16}, Words: []int{8}},
				"etabB": {Workers: 4, Tokens: []int{16}, Words: []int{8}},
				"tiers": {Workers: 4, Think: 50 * time.Millisecond, Tokens: []int{16}, Words: []int{20}}},
			Faults: []FaultAt{{Kind: "saturate-small", At: s(2), Arg: 5}},
			Skip:   map[int]string{3: "two pools and two upstreams: the borrower-over-owners judge reads one upstream's concurrency"},
			Expect: []string{"fallback-spare"}, ControlSince: "v0.38.0"}),
		with(Scenario{Name: "control-recorder-stall", Doc: "POSITIVE CONTROL (gateway v0.38.1, #165): the store hangs (not restarts), requests are answered meanwhile, the only replica is SIGKILLed before the 10 s insert timeout and restarted: every answered request must be billed exactly once",
			Needs: []string{"pg"}, Replicas: 1, F: Features{}, Duration: s(13),
			Load: map[string]OrgLoad{
				"etabA": {Workers: 4, Think: 100 * time.Millisecond, Tokens: []int{16}, Words: []int{8}},
				"etabB": {Workers: 3, Think: 100 * time.Millisecond, Tokens: []int{16}, Words: []int{8}}},
			// the hang lasts 5.5 s, the kill falls 4.5 s into it (insert timeout: 10 s):
			// v0.38.0 loses those 4.5 s of answers, v0.38.1 only the last second
			// (the documented bound, allowed by the kill window).
			Faults:       []FaultAt{{Kind: "pg-hang", At: s(3), For: s(5.5)}, {Kind: "replica-kill", Target: "r0", At: s(7.5), For: s(1.5)}},
			ControlSince: "v0.38.1"}),
		with(Scenario{Name: "control-schac-institution", Doc: "POSITIVE CONTROL (v0.39.0 fix): a person has one institution, the scope of their name: a multi-valued claim, two spellings of the claim and an issuer with no accept must not map a person of univ-d to univ-c's team",
			F: Features{IdPs: true, SchacInst: true}, Duration: s(4), Load: map[string]OrgLoad{"etabA": {Workers: 1, Think: 200 * time.Millisecond, Tokens: []int{16}, Words: []int{6}}},
			Expect: []string{"schac-institution"}, ControlSince: "v0.39.0"}),
		with(Scenario{Name: "control-shared-scim", Doc: "POSITIVE CONTROL (v0.39.0 fix): a SCIM client shared by two organisations owns no domain: the org admin of the one that does not claim it must be refused when creating its people",
			F: Features{IdPs: true, SharedSCIM: true}, Duration: s(4), Load: map[string]OrgLoad{"etabA": {Workers: 1, Think: 200 * time.Millisecond, Tokens: []int{16}, Words: []int{6}}},
			Expect: []string{"shared-scim"}, ControlSince: "v0.39.0"}),
		with(Scenario{Name: "control-dcgm-glitch", Doc: "POSITIVE CONTROL (v0.39.0 fix): the GPUs carry a month of lifetime energy and one scrape reads 0: the next reading must not bill the lifetime counter in one interval",
			F: Features{Fluids: true, BMC: true, DCGMGlitch: true}, Duration: s(14), Load: steady(),
			Faults: []FaultAt{{Kind: "dcgm-glitch", At: s(5)}}, ControlSince: "v0.39.0"}),
		with(Scenario{Name: "control-spool-fair", Doc: "POSITIVE CONTROL (v0.39.0 fix): PostgreSQL hangs, the spool is 1 MiB and one organisation floods: it is refused for the spool and the others are still served, with no answered request unbilled",
			Needs: []string{"pg"}, Replicas: 1, F: Features{SpoolFair: true}, Duration: s(30),
			Load: map[string]OrgLoad{
				// refused requests come back at once: a think time keeps the flood in the
				// tens of thousands of requests, not the hundreds of thousands (which
				// the gateway then replays for longer than the judge waits)
				"etabA": {Workers: 12, Think: 20 * time.Millisecond, Tokens: []int{16}, Words: []int{8}},
				"etabB": {Workers: 3, Think: 100 * time.Millisecond, Tokens: []int{16}, Words: []int{8}}},
			Faults: []FaultAt{{Kind: "pg-hang", At: s(2), For: s(8)}},
			Expect: []string{"spool-fair"}, ControlSince: "v0.39.0"}),
		with(Scenario{Name: "control-n-slots", Doc: "POSITIVE CONTROL (v0.39.0 fix): the owners keep 5 of 8 slots busy, a spare-only borrower asks n = best_of = 4: a request of n sequences holds n slots and is refused when fewer are free",
			F: Features{}, Duration: s(10), UpstreamLatency: time.Second,
			Load: map[string]OrgLoad{
				"etabA": {Workers: 5, Tokens: []int{16}, Words: []int{8}},
				"tiers": {Workers: 4, Think: 50 * time.Millisecond, NPct: 100, N: 4, Tokens: []int{16}, Words: []int{20}}},
			Skip:   map[int]string{3: "the borrower's n=4 requests weigh four slots: the borrower-over-owners judge counts requests"},
			Expect: []string{"n-admission"}, ControlSince: "v0.39.0"}),
		with(Scenario{Name: "idp-clock-skew", Doc: "IdP clocks off by minutes against a 5 m allowance: 4 min ahead (accepted), 7 min behind and 400 s ahead (refused with 401)", F: full(), Duration: s(14), Load: steady(),
			Faults: []FaultAt{{Kind: "idp-skew", Target: "idpA", At: s(2), For: s(10), Arg: 240}, {Kind: "idp-skew", Target: "idpB", At: s(2), For: s(10), Arg: -420},
				{Kind: "idp-skew", Target: "fed", At: s(2), For: s(10), Arg: 400}}}),
		with(Scenario{Name: "bmc-offline", Doc: "the node's BMC goes offline for a while", F: full(), Duration: s(14), Load: steady(),
			Faults: []FaultAt{{Kind: "bmc-offline", At: s(4), For: s(5)}}}),
		with(Scenario{Name: "bmc-counter-reset", Doc: "the BMC reboots and its energy counter restarts at zero", F: full(), Duration: s(14), Load: steady(),
			Faults: []FaultAt{{Kind: "bmc-counter-reset", At: s(5), Arg: 0}}}),
		with(Scenario{Name: "bmc-power-only", Doc: "the BMC reports a power and no energy counter", F: full(), Duration: s(12), Load: steady(),
			Faults: []FaultAt{{Kind: "bmc-power-only", At: s(3), For: s(5)}}}),
		with(Scenario{Name: "ilaas-saturates-while-owners-ramp", Doc: "the borrower saturates the pool, then the three owners ramp up to more than the pool; the owners' share must converge",
			// no budgets: a budget would stop an owner before the shares can show
			F:               Features{Fluids: true, Federation: true, IdPs: true, BMC: true},
			Duration:        s(26),
			UpstreamLatency: 150 * time.Millisecond,
			PrioritySlots:   PoolCapacity,
			Load: map[string]OrgLoad{
				"tiers": {Workers: 16, Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{30}},
				"etabA": {Workers: 10, Start: s(6), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}},
				"etabB": {Workers: 10, Start: s(6), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}},
				"etabC": {Workers: 10, Start: s(6), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}}},
			Expect: []string{"owner-shares", "owners-first"}}),
		with(Scenario{Name: "combined-upstream-and-clients", Doc: "5xx, a cut stream and wrong usage under disconnecting clients and a retry storm", F: full(), Duration: s(18),
			Load: map[string]OrgLoad{
				"etabA": {Workers: 4, Think: 50 * time.Millisecond, StreamPct: 70, LeavePct: 30, NPct: 20, RetryMax: 3, Tokens: []int{16, 32}, Words: []int{8}},
				"etabB": {Workers: 3, Think: 50 * time.Millisecond, StreamPct: 70, LeavePct: 30, NPct: 20, RetryMax: 3, Tokens: []int{16, 32}, Words: []int{20}},
				"etabC": {Workers: 2, Think: 50 * time.Millisecond, StreamPct: 70, LeavePct: 30, RetryMax: 3, Tokens: []int{16}, Words: []int{40}},
				"tiers": {Workers: 4, Think: 50 * time.Millisecond, StreamPct: 70, LeavePct: 30, RetryMax: 3, Tokens: []int{16}, Words: []int{30}}},
			Faults: []FaultAt{{Kind: "upstream-5xx", At: s(2), For: s(4), Arg: 503, Arg2: 3}, {Kind: "upstream-cut-sse", At: s(5), For: s(4), Arg: 2},
				{Kind: "upstream-wrong-usage", At: s(9), For: s(4), Arg: 3}, {Kind: "idp-down", Target: "idpA", At: s(6), For: s(4)},
				{Kind: "bmc-counter-reset", At: s(7), Arg: 0}, {Kind: "preemption-burst", At: s(10), For: s(3), Arg: 30, Arg2: 10}},
			Bursts: []Burst{{At: s(8), Org: "etabA", N: 20, Edge: true, Tokens: 32}}}),
		// PostgreSQL scenarios: CI only (there is no Docker on the workstation).
		haBase("ha-replica-kill", "two replicas on one PostgreSQL; one is killed (SIGKILL) and restarted under load, then the other",
			FaultAt{Kind: "replica-kill", Target: "r1", At: s(6), For: s(6)}, FaultAt{Kind: "replica-kill", Target: "r0", At: s(16), For: s(5)}),
		haBase("ha-partition", "the replicas cannot reach each other (PostgreSQL stays up)",
			FaultAt{Kind: "ha-partition", At: s(6), For: s(8)}),
		haBase("ha-config-reload", "a rolling configuration reload (stop and start, one replica at a time) in the middle of traffic",
			FaultAt{Kind: "config-reload", Target: "r0", At: s(5), For: s(3)}, FaultAt{Kind: "config-reload", Target: "r1", At: s(12), For: s(3)}),
		haBase("pg-restart", "PostgreSQL restarts: every connection reset, then refused for 5 s (emulated by a relay: no Docker on the workstation)",
			FaultAt{Kind: "pg-restart", At: s(6), For: s(5)}),
		haBase("pg-slow-queries", "every PostgreSQL round trip takes 400 ms for 8 s",
			FaultAt{Kind: "pg-slow", At: s(6), For: s(8), Arg: 400}),
		haBase("ha-kill-during-pg-restart", "combined: a replica is killed while PostgreSQL is down, and restarts after it is back",
			FaultAt{Kind: "pg-restart", At: s(6), For: s(5)}, FaultAt{Kind: "replica-kill", Target: "r1", At: s(8), For: s(6)}),
		pgOne("month-close-during-traffic", "the month that has just ended is closed (POST /admin/billing/close) while traffic runs: its reports must not change, and it cannot be closed twice",
			Features{Fluids: true, BMC: true, Budgets: true}, FaultAt{Kind: "month-close", At: s(5)}),
		pgOne("migrate-users-mid-traffic", "migrate-users --apply renames the stored unscoped users while requests are served",
			Features{Budgets: true, IdPs: true}, FaultAt{Kind: "migrate-users", At: s(6)}),
		pgOne("combined-bmc-reset-and-month-close", "the BMC counter restarts and a month is closed in the same minute",
			Features{Fluids: true, BMC: true, Budgets: true}, FaultAt{Kind: "bmc-counter-reset", At: s(4), Arg: 0}, FaultAt{Kind: "month-close", At: s(7)}),
		with(Scenario{Name: "combined-saturation-5xx-and-owner-ramp", Doc: "the borrower saturates the pool, the upstream answers 502 to every third request, and the owners ramp up",
			F: Features{Fluids: true, Federation: true, IdPs: true, BMC: true}, Duration: s(26), UpstreamLatency: 150 * time.Millisecond, PrioritySlots: PoolCapacity,
			Load: map[string]OrgLoad{
				"tiers": {Workers: 16, Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{30}},
				"etabA": {Workers: 10, Start: s(6), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}},
				"etabB": {Workers: 10, Start: s(6), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}},
				"etabC": {Workers: 10, Start: s(6), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}}},
			Faults: []FaultAt{{Kind: "upstream-5xx", At: s(8), For: s(10), Arg: 502, Arg2: 3}},
			Expect: []string{"owners-first"}}),
		with(Scenario{Name: "combined-jwks-outage-and-retry-storm", Doc: "both IdPs' key sets vanish while clients retry every refusal at once", F: full(), Duration: s(16),
			Load: map[string]OrgLoad{
				"etabA": {Workers: 5, StreamPct: 30, RetryMax: 5, Tokens: []int{16, 32}, Words: []int{8}},
				"etabB": {Workers: 5, StreamPct: 30, RetryMax: 5, Tokens: []int{16, 32}, Words: []int{20}},
				"etabC": {Workers: 2, StreamPct: 30, RetryMax: 5, Tokens: []int{16}, Words: []int{40}},
				"tiers": {Workers: 5, StreamPct: 30, RetryMax: 5, Tokens: []int{16}, Words: []int{30}}},
			Faults: []FaultAt{{Kind: "idp-down", Target: "idpA", At: s(3), For: s(7)}, {Kind: "idp-down", Target: "idpB", At: s(3), For: s(7)},
				{Kind: "idp-down", Target: "fed", At: s(3), For: s(7)}}}),
	}
	return append(append(cat, round2()...), round3()...)
}

// pgOne is a single-gateway scenario on PostgreSQL (CI): operations that need a
// database (closing a month, migrate-users).
func pgOne(name, doc string, f Features, faults ...FaultAt) *Scenario {
	return with(Scenario{Name: name, Doc: doc, Needs: []string{"pg"}, F: f, Duration: s(16), Load: steady(), Faults: faults})
}

// haBase is a two-replica scenario on PostgreSQL: CI only.
func haBase(name, doc string, f ...FaultAt) *Scenario {
	return with(Scenario{Name: name, Doc: doc, Needs: []string{"pg"}, Replicas: 2,
		F: Features{Budgets: true}, Duration: s(24), Load: steady(), Faults: f,
		Bursts: []Burst{{At: s(10), Org: "etabA", N: 16, Edge: true, Tokens: 32}}})
}

// Find returns a scenario by name.
func Find(name string) *Scenario {
	for _, sc := range Catalogue() {
		if sc.Name == name {
			return sc
		}
	}
	return nil
}
