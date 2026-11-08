package chaos

import "time"

// round2 is the second campaign's scenarios: network, hostile tenants, an IdP
// that changes its schac, a long run, HA budgets, and combined faults.
func round2() []*Scenario {
	ha := func(name, doc string, f Features, load map[string]OrgLoad, faults []FaultAt, bursts []Burst) *Scenario {
		return with(Scenario{Name: name, Doc: doc, Needs: []string{"pg"}, Replicas: 2, F: f, Duration: s(24), Load: load, Faults: faults, Bursts: bursts})
	}
	rampLoad := map[string]OrgLoad{
		"tiers": {Workers: 16, Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{30}},
		"etabA": {Workers: 8, Start: s(5), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}},
		"etabB": {Workers: 8, Start: s(5), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}},
		"etabC": {Workers: 8, Start: s(5), Think: 20 * time.Millisecond, StreamPct: 30, Tokens: []int{32}, Words: []int{8}}}
	flood := func(n int) map[string]OrgLoad {
		l := steady()
		l["tiers"] = OrgLoad{Workers: n, Think: 20 * time.Millisecond, StreamPct: 30, NPct: 100, N: 4, Tokens: []int{64}, Words: []int{30}}
		return l
	}
	return []*Scenario{
		with(Scenario{Name: "hostile-borrower-flood", Doc: "the borrower floods with n = best_of = 4 requests of 64 tokens at the edge of its budget",
			F: full(), Duration: s(16), Load: flood(12)}),
		with(Scenario{Name: "hostile-admin-flood", Doc: "an organisation hammers the heavy /admin reports (spend, budgets) from four clients while traffic runs",
			F: full(), Duration: s(16), Load: steady(), Faults: []FaultAt{{Kind: "admin-flood", At: s(3), For: s(9), Arg: 4}}, Expect: []string{"flood"}}),
		with(Scenario{Name: "hostile-fed-metrics-hammer", Doc: "a federation peer hammers <prefix>/metrics with four clients while it sends its traffic",
			F: full(), Duration: s(16), Load: steady(), Faults: []FaultAt{{Kind: "fed-hammer", At: s(3), For: s(9), Arg: 4}}, Expect: []string{"flood"}}),
		with(Scenario{Name: "idp-schac-change", Doc: "the federation IdP states another schac_home_organization from now on: its tokens must be refused from then on, and not before",
			F: full(), Duration: s(16), Load: steady(), Faults: []FaultAt{{Kind: "idp-schac-change", At: s(7)}}, Expect: []string{"schac-change"}}),
		with(Scenario{Name: "net-upstream-latency", Doc: "300 ms of latency on every chunk between the gateway and its upstream",
			F: Features{Fluids: true, Federation: true, IdPs: true, BMC: true, Budgets: true, UpstreamNet: true}, Duration: s(16), Load: steady(),
			Faults: []FaultAt{{Kind: "net-slow", At: s(3), For: s(7), Arg: 300}}}),
		with(Scenario{Name: "net-upstream-loss", Doc: "connections between the gateway and its upstream are reset and refused in turns of 300 ms",
			F: Features{Fluids: true, Federation: true, IdPs: true, BMC: true, Budgets: true, UpstreamNet: true}, Duration: s(16), Load: steady(),
			Faults: []FaultAt{{Kind: "net-loss", At: s(3), For: s(7)}}}),
		haBase("net-replica-link-slow", "300 ms of latency on the channel between the replicas", FaultAt{Kind: "ha-link-slow", At: s(6), For: s(10), Arg: 300}),
		ha("ha-budget-edge", "two replicas serve the hard budget of a key, a team and an org at its edge, in three bursts, with no fault",
			Features{Budgets: true}, steady(), nil,
			[]Burst{{At: s(5), Org: "etabA", N: 24, Edge: true, Tokens: 32}, {At: s(9), Org: "etabA", N: 24, Edge: true, Tokens: 32},
				{At: s(13), Org: "etabB", N: 30, Tokens: 64}, {At: s(17), Org: "tiers", N: 30, Tokens: 64}}),
		ha("migrate-users-ha", "migrate-users --apply renames the stored unscoped users while two replicas serve requests",
			Features{Budgets: true, IdPs: true}, steady(), []FaultAt{{Kind: "migrate-users", At: s(8)}}, nil),
		with(Scenario{Name: "long-mixed-traffic", Doc: "30 minutes of mixed traffic with periodic upstream, IdP and BMC faults: the gateway's memory, descriptors and spool do not grow, and the owners' shares do not drift",
			F: Features{Fluids: true, Federation: true, IdPs: true, BMC: true}, Duration: 30 * time.Minute, Load: steady(),
			Faults: []FaultAt{
				{Kind: "upstream-5xx", At: s(300), For: s(20), Arg: 502, Arg2: 2}, {Kind: "idp-down", Target: "idpA", At: s(600), For: s(30)},
				{Kind: "bmc-offline", At: s(900), For: s(30)}, {Kind: "upstream-slow-tokens", At: s(1200), For: s(30), Arg: 100},
				{Kind: "idp-rotate", Target: "idpB", At: s(1500), Arg: 1}},
			Expect: []string{"long-run"}}),
		// combined faults
		ha("combined-saturation-pg-hang-replica-kill", "the borrower saturates the pool, then PostgreSQL hangs and a replica is killed in it",
			Features{}, rampLoad, []FaultAt{{Kind: "pg-hang", At: s(9), For: s(5)}, {Kind: "replica-kill", Target: "r1", At: s(11), For: s(6)}}, nil),
		pgOne("combined-month-close-bmc-reset-pg-slow", "a month is closed, the BMC counter restarts and PostgreSQL slows down in the same minute",
			Features{Fluids: true, BMC: true, Budgets: true}, FaultAt{Kind: "pg-slow", At: s(3), For: s(10), Arg: 300},
			FaultAt{Kind: "bmc-counter-reset", At: s(5), Arg: 0}, FaultAt{Kind: "month-close", At: s(7)}),
		with(Scenario{Name: "combined-hostile-tenants", Doc: "the borrower floods with n = 4 while an organisation floods /admin and a federation peer hammers its metrics",
			F: full(), Duration: s(16), Load: flood(10),
			Faults: []FaultAt{{Kind: "admin-flood", At: s(3), For: s(9), Arg: 3}, {Kind: "fed-hammer", At: s(3), For: s(9), Arg: 3}}, Expect: []string{"flood"}}),
		with(Scenario{Name: "combined-net-loss-idp-down-disconnects", Doc: "the upstream link flaps while the federation IdP is down and clients leave at every stream phase",
			F: Features{Fluids: true, Federation: true, IdPs: true, BMC: true, Budgets: true, UpstreamNet: true}, Duration: s(16),
			Load: map[string]OrgLoad{
				"etabA": {Workers: 3, Think: 100 * time.Millisecond, StreamPct: 90, LeavePct: 40, Tokens: []int{16, 32}, Words: []int{8}},
				"etabB": {Workers: 2, Think: 100 * time.Millisecond, StreamPct: 90, LeavePct: 40, Tokens: []int{16, 32}, Words: []int{20}},
				"etabC": {Workers: 1, Think: 100 * time.Millisecond, StreamPct: 90, LeavePct: 40, Tokens: []int{16}, Words: []int{40}},
				"tiers": {Workers: 2, Think: 100 * time.Millisecond, StreamPct: 90, LeavePct: 40, Tokens: []int{16}, Words: []int{30}}},
			Faults: []FaultAt{{Kind: "net-loss", At: s(3), For: s(8)}, {Kind: "idp-down", Target: "fed", At: s(4), For: s(6)}}}),
		ha("combined-partition-kill-budget-edge", "the replicas are partitioned, one is killed in it, and bursts hit the budgets' edge",
			Features{Budgets: true}, steady(), []FaultAt{{Kind: "ha-partition", At: s(5), For: s(9)}, {Kind: "replica-kill", Target: "r1", At: s(8), For: s(6)}},
			[]Burst{{At: s(7), Org: "etabA", N: 24, Edge: true, Tokens: 32}, {At: s(12), Org: "etabA", N: 24, Edge: true, Tokens: 32}}),
		with(Scenario{Name: "combined-schac-change-and-key-rotation", Doc: "the federation IdP changes its schac and rotates its key in the same minute",
			F: full(), Duration: s(16), Load: steady(),
			Faults: []FaultAt{{Kind: "idp-rotate", Target: "idpA", At: s(3), Arg: 1}, {Kind: "idp-schac-change", At: s(8)}}, Expect: []string{"schac-change"}}),
	}
}
