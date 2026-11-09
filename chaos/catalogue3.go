package chaos

import "time"

// round3 is the third campaign's scenarios: a movable clock, a spool on a tiny
// or a slow filesystem, and a PostgreSQL that turns read-only.
func round3() []*Scenario {
	return []*Scenario{
		with(Scenario{Name: "clock-month-close-with-content", Doc: "the gateway's clock starts on the 5th of the next month, so the current month, which holds the traffic, has just ended for it: that month is closed while traffic runs and the clock steps an hour back and forth, and invariant 5 judges a month WITH content",
			Needs: []string{"pg", "clock"}, F: Features{Fluids: true, BMC: true, Clock: "next-month-5th"}, Duration: s(18), Load: map[string]OrgLoad{
				"etabA": {Workers: 3, Think: 100 * time.Millisecond, Tokens: []int{16, 32}, Words: []int{8}},
				"etabB": {Workers: 2, Think: 100 * time.Millisecond, Tokens: []int{16, 32}, Words: []int{20}}},
			// the clock is a month ahead of the records, which are stamped in real
			// time; it is stepped an hour back and forth around the close
			Faults: []FaultAt{{Kind: "month-close", At: s(8)}, {Kind: "clock-step", At: s(11), Arg: -3600}, {Kind: "clock-step", At: s(14), Arg: 3600}},
			Skip: map[int]string{2: "the gateway's clock is a month ahead of the records: the harness's reports window is today's",
				3: "no borrower in this scenario",
				4: "the gateway's clock is a month ahead of the energy: the harness's reports window is today's"},
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
			Needs: []string{"pg", "slowfs"}, Replicas: 1, F: Features{SlowFS: true}, Duration: s(22),
			Load: map[string]OrgLoad{
				"etabA": {Workers: 9, Think: 20 * time.Millisecond, Tokens: []int{16}, Words: []int{8}},
				"etabB": {Workers: 3, Think: 100 * time.Millisecond, Tokens: []int{16}, Words: []int{8}}},
			Faults: []FaultAt{{Kind: "pg-hang", At: s(2), For: s(14)}}, Expect: []string{"slow-fsync"}, ControlSince: "v0.39.0"}),
	}
}
