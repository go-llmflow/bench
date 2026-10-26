// gwbench compares the per-request overhead of LLM gateways placed in front
// of the same upstream. Requests are sent one at a time, interleaved in a
// random order within each round, and each gateway's latency is paired with
// the direct latency of the same round. It refuses to measure when the
// machine is loaded.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var osExit = os.Exit

func main() {
	if err := RootCmd().Execute(); err != nil {
		osExit(1)
	}
}

// RootCmd builds the command tree.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "gwbench",
		Short:         "Compare the overhead of LLM gateways in front of one upstream",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(runCmd(), keyCmd())
	return root
}

func readSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ParseTarget reads NAME|URL|KEYFILE. The key is read from the file, never
// taken from the command line.
func ParseTarget(spec, model string) (Target, error) {
	p := strings.Split(spec, "|")
	if len(p) != 3 || p[0] == "" || p[1] == "" {
		return Target{}, fmt.Errorf("target %q: want NAME|URL|KEYFILE", spec)
	}
	if p[0] == "direct" {
		return Target{}, fmt.Errorf("target %q: the name \"direct\" is reserved", spec)
	}
	key, err := readSecret(p[2])
	if err != nil {
		return Target{}, fmt.Errorf("target %q: %w", spec, err)
	}
	return Target{Name: p[0], URL: p[1], Model: model, Key: key}, nil
}

func runCmd() *cobra.Command {
	var (
		direct, litellm, llmflow       string
		model, directModel             string
		litellmKeyFile, llmflowKeyFile string
		rounds, warm                   int
		maxLoad                        float64
		seed                           uint64
		wait                           time.Duration
		runs                           int
		extra                          []string
		promptFile                     string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Measure direct, LiteLLM and llmflow latencies, interleaved",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			lk, err := readSecret(litellmKeyFile)
			if err != nil {
				return err
			}
			fk, err := readSecret(llmflowKeyFile)
			if err != nil {
				return err
			}
			targets := []Target{{Name: "direct", URL: direct, Model: directModel}}
			if litellm != "" {
				targets = append(targets, Target{Name: "litellm", URL: litellm, Model: model, Key: lk})
			}
			if llmflow != "" {
				targets = append(targets, Target{Name: "llmflow", URL: llmflow, Model: model, Key: fk})
			}
			for _, spec := range extra {
				t, err := ParseTarget(spec, model)
				if err != nil {
					return err
				}
				targets = append(targets, t)
			}
			if promptFile != "" {
				b, err := os.ReadFile(promptFile)
				if err != nil {
					return err
				}
				for i := range targets {
					targets[i].Prompt = string(b)
				}
			}
			c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
			// Short runs, each gated by the load before AND after: a run
			// during which the machine got busy is discarded, not averaged.
			var kept [][]Result
			deadline := time.Now().Add(wait)
			for attempt := 0; len(kept) < runs; attempt++ {
				left := time.Until(deadline)
				if left < 0 {
					left = 0
				}
				load0, err := WaitForLoad(cmd.Context(), LoadAvg1, maxLoad, left, 30*time.Second, func(l float64) {
					fmt.Fprintf(out, "load %.2f > %.2f, waiting\n", l, maxLoad)
				})
				if err != nil {
					if len(kept) > 0 {
						fmt.Fprintf(out, "stopping with %d of %d runs: %v\n", len(kept), runs, err)
						break
					}
					return err
				}
				res, err := Run(cmd.Context(), c, targets, rounds, warm, seed+uint64(attempt))
				if err != nil {
					return err
				}
				load1, _ := LoadAvg1()
				if load1 > maxLoad {
					fmt.Fprintf(out, "run %d discarded: load %.2f -> %.2f\n", attempt, load0, load1)
					continue
				}
				fmt.Fprintf(out, "run %d kept: load %.2f -> %.2f\n", attempt, load0, load1)
				kept = append(kept, res)
			}
			report(out, kept, rounds, warm)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&direct, "direct", "http://127.0.0.1:18000/v1/chat/completions", "upstream URL, called directly")
	f.StringVar(&litellm, "litellm", "", "LiteLLM chat completions URL (empty: skip)")
	f.StringVar(&llmflow, "llmflow", "", "llmflow chat completions URL (empty: skip)")
	f.StringVar(&model, "model", "plain", "model name exposed by both gateways")
	f.StringVar(&directModel, "direct-model", "fake", "model name the upstream serves, sent on the direct path")
	f.StringVar(&litellmKeyFile, "litellm-key-file", "", "file holding the LiteLLM key")
	f.StringVar(&llmflowKeyFile, "llmflow-key-file", "", "file holding the llmflow key")
	f.IntVar(&rounds, "rounds", 5000, "measured rounds")
	f.IntVar(&warm, "warmup", 200, "discarded warm-up rounds")
	f.Float64Var(&maxLoad, "max-load", 4, "refuse to run above this 1-minute load average")
	f.Uint64Var(&seed, "seed", 1, "shuffle seed")
	f.DurationVar(&wait, "wait", 0, "wait up to this long in total for the load to drop below --max-load (0: refuse at once)")
	f.IntVar(&runs, "runs", 1, "number of kept runs; a run whose load ends above --max-load is discarded")
	f.StringVar(&promptFile, "prompt-file", "", "user message sent to every target (default \"ping\")")
	f.StringArrayVar(&extra, "target", nil, "extra target NAME|URL|KEYFILE (repeatable; KEYFILE may be empty)")
	return cmd
}

func keyCmd() *cobra.Command {
	var admin, masterFile, team, out string
	cmd := &cobra.Command{
		Use:   "llmflow-key",
		Short: "Create an llmflow key through the admin API and write it to a 0600 file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			master, err := readSecret(masterFile)
			if err != nil {
				return err
			}
			body, _ := json.Marshal(map[string]any{"team": team, "name": "gwbench"})
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, strings.TrimRight(admin, "/")+"/admin/keys", bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+master)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("admin API: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(data))
			}
			var k struct {
				ID  string `json:"id"`
				Key string `json:"key"`
			}
			if err := json.Unmarshal(data, &k); err != nil || k.Key == "" {
				return fmt.Errorf("admin API: no key in the answer")
			}
			if err := os.WriteFile(out, []byte(k.Key+"\n"), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "key %s written to %s\n", k.ID, out)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&admin, "admin", "http://127.0.0.1:18080", "llmflow base URL")
	f.StringVar(&masterFile, "master-key-file", "", "file holding the llmflow master key")
	f.StringVar(&team, "team", "bench", "team of the new key")
	f.StringVar(&out, "out", "llmflow.key", "where to write the key (mode 0600)")
	return cmd
}
