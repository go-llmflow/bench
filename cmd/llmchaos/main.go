// llmchaos is a chaos campaign against llmflow-gateway: a real gateway process
// in front of fake vLLM upstreams, identity providers, a BMC and an exporter,
// with faults injected alone and combined, and nine invariants judged after
// every scenario. A scenario is data (chaos.Catalogue) and is replayed from its
// name and a seed.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-llmflow/bench/chaos"
	"github.com/spf13/cobra"
)

var osExit = os.Exit

func main() {
	cmd := RootCmd()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), err)
		osExit(exitCode(err))
	}
}

// failedError is returned when an invariant broke: exit 1, distinct from a
// usage or setup error (exit 2).
type failedError struct{ n int }

func (e failedError) Error() string { return fmt.Sprintf("%d scenario run(s) broke an invariant", e.n) }

func exitCode(err error) int {
	if _, ok := err.(failedError); ok {
		return 1
	}
	return 2
}

// RootCmd builds the command; tests drive it with SetArgs.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "llmchaos",
		Short:        "Chaos campaign for llmflow-gateway: faults in, invariants out",
		SilenceUsage: true, SilenceErrors: true,
	}
	root.AddCommand(listCmd(), runCmd(), mergeCmd(), diffCmd())
	return root
}

func diffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff a.json b.json",
		Short: "List the verdicts whose status differs between two campaigns (two gateway releases)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var sets [2][]chaos.Result
			for i, a := range args {
				b, err := os.ReadFile(a)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(b, &sets[i]); err != nil {
					return fmt.Errorf("%s: %w", a, err)
				}
			}
			d := chaos.Compare(sets[0], sets[1])
			for _, l := range d {
				fmt.Fprintln(cmd.OutOrStdout(), l)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d difference(s)\n", len(d))
			return nil
		},
	}
}

func mergeCmd() *cobra.Command {
	var (
		seeds      []int
		table      string
		resultsOut string
	)
	cmd := &cobra.Command{
		Use:   "merge results.json...",
		Short: "Merge the shards' results, write one table, fail on any problem",
		Long: `Merge the JSON files written by "run --results" (one per shard) into one
table. The exit status is 1 on a failed invariant, a broken, skipped or empty
run, an N/A a scenario is not entitled to, or a scenario x seed of the
catalogue that no shard ran.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var all []chaos.Result
			for _, a := range args {
				b, err := os.ReadFile(a)
				if err != nil {
					return err
				}
				var rs []chaos.Result
				if err := json.Unmarshal(b, &rs); err != nil {
					return fmt.Errorf("%s: %w", a, err)
				}
				all = append(all, rs...)
			}
			sort.SliceStable(all, func(i, j int) bool {
				if all[i].Scenario != all[j].Scenario {
					return catalogueIndex(all[i].Scenario) < catalogueIndex(all[j].Scenario)
				}
				return all[i].Seed < all[j].Seed
			})
			if resultsOut != "" {
				b, err := json.MarshalIndent(all, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(resultsOut, b, 0o600); err != nil {
					return err
				}
			}
			tb := chaos.Table(all)
			if table != "" {
				if err := os.WriteFile(table, []byte(tb), 0o600); err != nil {
					return err
				}
			}
			out := cmd.OutOrStdout()
			fmt.Fprint(out, tb)
			problems := chaos.Audit(all, seeds)
			for _, p := range problems {
				fmt.Fprintln(out, "PROBLEM", p)
			}
			fmt.Fprintf(out, "\n%d run(s), %d problem(s)\n", len(all), len(problems))
			if len(problems) > 0 {
				return failedError{len(problems)}
			}
			return nil
		},
	}
	cmd.Flags().IntSliceVar(&seeds, "seed", []int{1}, "the seeds the campaign had to cover")
	cmd.Flags().StringVar(&table, "table", "", "write the merged table (markdown) to this file")
	cmd.Flags().StringVar(&resultsOut, "results", "", "write the merged results as JSON to this file (input of: llmchaos diff)")
	return cmd
}

func catalogueIndex(name string) int {
	for i, sc := range chaos.Catalogue() {
		if sc.Name == name {
			return i
		}
	}
	return 1 << 30
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the scenarios, the faults and the invariants",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "scenarios:")
			for _, sc := range chaos.Catalogue() {
				pg := ""
				if len(sc.Needs) > 0 {
					pg = " [" + strings.Join(sc.Needs, ",") + ", CI]"
				}
				fmt.Fprintf(out, "  %-36s %s%s\n", sc.Name, sc.Doc, pg)
			}
			fmt.Fprintln(out, "faults:")
			for _, k := range chaos.FaultKinds() {
				fmt.Fprintf(out, "  %s\n", k)
			}
			fmt.Fprintln(out, "invariants:")
			for n := 1; n < len(chaos.InvariantNames); n++ {
				fmt.Fprintf(out, "  %d. %s\n", n, chaos.InvariantNames[n])
			}
			return nil
		},
	}
}

func runCmd() *cobra.Command {
	var (
		gateway, workdir, pgFile string
		seeds                    []int
		all, asJSON, dry, strict bool
		table, resultsFile       string
		maxLoad                  float64
		loadWait                 time.Duration
	)
	cmd := &cobra.Command{
		Use:   "run [scenario...]",
		Short: "Run scenarios with a seed and print every invariant's verdict",
		Long: `Run scenarios against a llmflow-gateway binary. Each run prints one line per
invariant: PASS, FAIL or N/A with the reason. The exit status is 1 when any
invariant failed, 2 on a setup error.

The campaign defers itself while the 1-minute load average is above --max-load
(a timing under load is not a measurement). The PostgreSQL scenarios read their
DSN from --pg-dsn-file (a secret is never on a command line or in the
environment) and are skipped, and said so, without it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			var scs []*chaos.Scenario
			switch {
			case all:
				scs = chaos.Catalogue()
			case len(args) == 0:
				return fmt.Errorf("name a scenario, or use --all (see: llmchaos list)")
			default:
				for _, n := range args {
					sc := chaos.Find(n)
					if sc == nil {
						return fmt.Errorf("no scenario %q (see: llmchaos list)", n)
					}
					scs = append(scs, sc)
				}
			}
			if gateway == "" {
				return fmt.Errorf("--gateway: the llmflow-gateway binary to attack is required")
			}
			if st, err := os.Stat(gateway); err != nil || st.IsDir() {
				return fmt.Errorf("--gateway %q: not a file", gateway)
			}
			gateway, _ = filepath.Abs(gateway)
			if workdir == "" {
				return fmt.Errorf("--workdir: a durable directory is required (never /tmp)")
			}
			workdir, _ = filepath.Abs(workdir)
			if err := refuseVolatile(workdir); err != nil {
				return err
			}
			pg := ""
			if pgFile != "" {
				b, err := os.ReadFile(pgFile)
				if err != nil {
					return fmt.Errorf("--pg-dsn-file: %w", err)
				}
				pg = strings.TrimSpace(string(b))
			}
			if maxLoad > 0 && !dry {
				l, err := chaos.WaitLoad(maxLoad, loadWait, func(f string, a ...any) { fmt.Fprintf(cmd.ErrOrStderr(), f+"\n", a...) })
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "load gate: 1-minute load %.2f <= %.2f\n", l, maxLoad)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			failed := 0
			var all []chaos.Result
			for _, sc := range scs {
				for _, seed := range seeds {
					opt := chaos.Options{GatewayBin: gateway, WorkDir: workdir, Seed: uint64(seed), PG: pg, DryRun: dry,
						Log: func(f string, a ...any) { fmt.Fprintf(cmd.ErrOrStderr(), "  "+f+"\n", a...) }}
					res := chaos.Run(ctx, opt, sc)
					all = append(all, res)
					if !asJSON {
						printResult(out, res)
					}
					// --strict: a scenario that was skipped (no PostgreSQL) judged
					// nothing, which in CI is a failure.
					if res.Failed() || strict && !dry && res.Skipped != "" {
						failed++
					}
					if ctx.Err() != nil {
						break
					}
				}
			}
			if table != "" {
				if err := os.WriteFile(table, []byte(chaos.Table(all)), 0o600); err != nil {
					return err
				}
			}
			if resultsFile != "" {
				b, err := json.MarshalIndent(all, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(resultsFile, b, 0o600); err != nil {
					return err
				}
			}
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				_ = enc.Encode(all)
			} else {
				fmt.Fprintf(out, "\n%d run(s), %d broke an invariant\n", len(all), failed)
			}
			if failed > 0 {
				return failedError{failed}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&gateway, "gateway", "", "path of the llmflow-gateway binary under test")
	f.StringVar(&workdir, "workdir", "", "durable directory for the runs' configs, keys and logs")
	f.StringVar(&pgFile, "pg-dsn-file", "", "file holding a PostgreSQL DSN, for the CI scenarios")
	f.IntSliceVar(&seeds, "seed", []int{1}, "seed(s): the same scenario and seed replay the same plan")
	f.BoolVar(&all, "all", false, "run every scenario")
	f.BoolVar(&asJSON, "json", false, "print the results as JSON")
	f.BoolVar(&strict, "strict", false, "a skipped scenario (no PostgreSQL) is a failure: for CI")
	f.StringVar(&table, "table", "", "write the scenario x seed x invariant table (markdown) to this file")
	f.StringVar(&resultsFile, "results", "", "write the results as JSON to this file (input of: llmchaos merge)")
	f.BoolVar(&dry, "check-config", false, "only generate every scenario's configuration and run the gateway's check-config on it; start nothing")
	f.Float64Var(&maxLoad, "max-load", 8, "defer while the 1-minute load average is above this (0: no gate)")
	f.DurationVar(&loadWait, "load-wait", 10*time.Minute, "how long to wait for the load to fall before giving up")
	return cmd
}

// refuseVolatile refuses scratch directories: a campaign's evidence must
// outlive the session.
func refuseVolatile(dir string) error {
	for _, bad := range []string{"/tmp/", "/private/tmp/", "/var/folders/", "/private/var/folders/"} {
		if strings.HasPrefix(dir+"/", bad) {
			return fmt.Errorf("--workdir %q is volatile; use a durable directory", dir)
		}
	}
	if t := os.TempDir(); t != "" && strings.HasPrefix(dir+"/", filepath.Clean(t)+"/") {
		return fmt.Errorf("--workdir %q is under $TMPDIR; use a durable directory", dir)
	}
	return nil
}

func printResult(out interface{ Write([]byte) (int, error) }, r chaos.Result) {
	fmt.Fprintf(out, "\n== %s seed=%d (%s)\n", r.Scenario, r.Seed, r.Elapsed.Round(time.Millisecond))
	switch {
	case r.Skipped != "":
		fmt.Fprintf(out, "   SKIPPED: %s\n", r.Skipped)
		return
	case r.Err != "":
		fmt.Fprintf(out, "   ERROR: %s\n", r.Err)
		return
	}
	fmt.Fprintf(out, "   %d requests\n", r.Requests)
	vs := append([]chaos.Verdict(nil), r.Verdicts...)
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].N < vs[j].N })
	for _, v := range vs {
		fmt.Fprintf(out, "   %d. %-4s %s\n", v.N, v.Status, v.Detail)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(out, "   note: %s\n", n)
	}
	fmt.Fprintf(out, "   replay: llmchaos run %s --seed %d --gateway ... --workdir ...\n", r.Scenario, r.Seed)
}
