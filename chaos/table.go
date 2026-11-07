package chaos

import (
	"fmt"
	"strings"
)

// Table renders results as a markdown table: one row per scenario and seed, one
// column per invariant (P pass, F fail, - not applicable), then totals per
// invariant. A skipped or broken run is a row of its own, never a blank.
func Table(rs []Result) string {
	var b strings.Builder
	b.WriteString("| scenario | seed | req | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	var pass, fail, na [10]int
	for _, r := range rs {
		cells := [10]string{}
		for i := range cells {
			cells[i] = "?"
		}
		switch {
		case r.Err != "":
			for i := 1; i < 10; i++ {
				cells[i] = "ERR"
			}
		case r.Skipped != "":
			for i := 1; i < 10; i++ {
				cells[i] = "skip"
			}
		default:
			for _, v := range r.Verdicts {
				switch v.Status {
				case Pass:
					cells[v.N] = "P"
					pass[v.N]++
				case Fail:
					cells[v.N] = "**F**"
					fail[v.N]++
				default:
					cells[v.N] = "-"
					na[v.N]++
				}
			}
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %s |\n", r.Scenario, r.Seed, r.Requests, strings.Join(cells[1:], " | "))
	}
	b.WriteString("\n| invariant | PASS | FAIL | N/A |\n|---|---|---|---|\n")
	for i := 1; i < 10; i++ {
		fmt.Fprintf(&b, "| %d | %d | %d | %d |\n", i, pass[i], fail[i], na[i])
	}
	return b.String()
}
