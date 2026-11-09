package chaos

import "time"

// round3 is the third campaign's scenarios: a movable clock, a spool on a tiny
// or a slow filesystem, and a PostgreSQL that turns read-only.
func round3() []*Scenario {
	return []*Scenario{
		with(Scenario{Name: "clock-month-close-with-content", Doc: "the gateway's clock starts on the 20th of the month that has just ended, traffic is stamped in it, the clock returns to real time and that month is closed: invariant 5 judges a month WITH content",
			Needs: []string{"pg", "clock"}, F: Features{Clock: "prev-month-20th"}, Duration: s(18), Load: map[string]OrgLoad{
				"etabA": {Workers: 3, Think: 100 * time.Millisecond, Tokens: []int{16, 32}, Words: []int{8}},
				"etabB": {Workers: 2, Think: 100 * time.Millisecond, Tokens: []int{16, 32}, Words: []int{20}}},
			Faults: []FaultAt{{Kind: "clock-real", At: s(8)}, {Kind: "month-close", At: s(11)}},
			Skip: map[int]string{2: "the clock puts the records in last month: the harness's reports window is today's",
				3: "no borrower in this scenario"},
			Expect: []string{"month-content"}}),
		with(Scenario{Name: "clock-jump-tariff-band", Doc: "the gateway's clock jumps back one hour, forward two and back one again around the 23:00 change of the tariff band (an NTP step, a DST change): the energy is still conserved, the budgets and the refusals hold",
			Needs: []string{"clock"}, F: Features{Fluids: true, BMC: true, Budgets: true, Clock: "tariff-edge"}, Duration: s(20), Load: steady(),
			Faults: []FaultAt{{Kind: "clock-step", At: s(4), Arg: -3600}, {Kind: "clock-step", At: s(9), Arg: 7200}, {Kind: "clock-step", At: s(14), Arg: -3600}},
			Skip:   map[int]string{9: "no federation mount"},
			Known:  map[int]string{4: "go-llmflow/gateway#175"}}),
		with(Scenario{Name: "combined-clock-jump-month-close-bmc-reset", Doc: "the clock steps back an hour, the BMC counter restarts and a month is closed, then the clock steps forward two hours, in ten seconds",
			Needs: []string{"pg", "clock"}, F: Features{Fluids: true, BMC: true, Budgets: true, Clock: "tariff-edge"}, Duration: s(18), Load: steady(),
			Faults: []FaultAt{{Kind: "clock-step", At: s(3), Arg: -3600}, {Kind: "bmc-counter-reset", At: s(5), Arg: 0}, {Kind: "month-close", At: s(7)},
				{Kind: "clock-step", At: s(10), Arg: 7200}},
			Skip: map[int]string{9: "no federation mount"}}),
		haBase("pg-readonly-failover", "PostgreSQL turns read-only mid-run (every session is cut and reconnects into a read-only database), then writable again: no hard budget exceeded, no spend lost or doubled",
			FaultAt{Kind: "pg-readonly", At: s(6), For: s(8)}),
		with(Scenario{Name: "spool-disk-full", Doc: "PostgreSQL hangs while the accounting spool's filesystem (1 MiB) fills up under a flooding organisation: every answered request is billed exactly once when PostgreSQL is back",
			Needs: []string{"pg", "fs"}, Replicas: 1, F: Features{SmallFS: true}, Duration: s(24),
			Load: map[string]OrgLoad{
				"etabA": {Workers: 12, Think: 20 * time.Millisecond, Tokens: []int{16}, Words: []int{8}},
				"etabB": {Workers: 3, Think: 100 * time.Millisecond, Tokens: []int{16}, Words: []int{8}}},
			Faults: []FaultAt{{Kind: "pg-hang", At: s(2), For: s(14)}}, Expect: []string{"spool-full"}}),
		with(Scenario{Name: "spool-slow-fsync", Doc: "PostgreSQL hangs while the spool's writes are slow (a delayed block device): the requests are not slowed by the spool, and every answered request is billed exactly once",
			Needs: []string{"pg", "slowfs"}, Replicas: 1, F: Features{SlowFS: true}, Duration: s(26),
			Load: map[string]OrgLoad{
				"etabA": {Workers: 12, Think: 20 * time.Millisecond, Tokens: []int{16}, Words: []int{8}},
				"etabB": {Workers: 3, Think: 100 * time.Millisecond, Tokens: []int{16}, Words: []int{8}}},
			Faults: []FaultAt{{Kind: "pg-hang", At: s(2), For: s(18)}}, Expect: []string{"slow-fsync"}, ControlSince: "v0.39.0"}),
	}
}
