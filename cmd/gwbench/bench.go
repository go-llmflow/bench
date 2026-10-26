package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Target is one way of reaching the same upstream: directly, or through a
// gateway.
type Target struct {
	Name  string
	URL   string // full chat completions URL
	Model string
	Key   string // bearer secret, may be empty
	// Prompt is the user message; empty means "ping".
	Prompt string
}

// Result holds the latencies of one target, in the order of the rounds,
// so that a gateway's latency can be paired with the direct latency
// measured in the same round.
type Result struct {
	Name      string
	Latencies []time.Duration
}

// Run sends rounds × len(targets) requests, one at a time. Within a round
// every target is hit once, in a fresh random order, so that drift of the
// machine (thermal, background load) spreads evenly over all targets
// instead of penalising whichever ran last. A warm-up of `warm` rounds is
// discarded.
func Run(ctx context.Context, c *http.Client, targets []Target, rounds, warm int, seed uint64) ([]Result, error) {
	if len(targets) == 0 || rounds <= 0 {
		return nil, errors.New("need at least one target and one round")
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	res := make([]Result, len(targets))
	for i, t := range targets {
		res[i] = Result{Name: t.Name, Latencies: make([]time.Duration, 0, rounds)}
	}
	order := make([]int, len(targets))
	for i := range order {
		order[i] = i
	}
	for r := 0; r < warm+rounds; r++ {
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for _, i := range order {
			d, err := once(ctx, c, targets[i])
			if err != nil {
				return nil, fmt.Errorf("%s, round %d: %w", targets[i].Name, r, err)
			}
			if r >= warm {
				res[i].Latencies = append(res[i].Latencies, d)
			}
		}
	}
	return res, nil
}

func once(ctx context.Context, c *http.Client, t Target) (time.Duration, error) {
	prompt := t.Prompt
	if prompt == "" {
		prompt = "ping"
	}
	msg, err := json.Marshal(prompt)
	if err != nil {
		return 0, err
	}
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%s}],"max_tokens":1}`, t.Model, msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Key != "" {
		req.Header.Set("Authorization", "Bearer "+t.Key)
	}
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	d := time.Since(t0)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(data))
	}
	return d, nil
}

// Percentile returns the p-th percentile (0..100) by nearest rank.
func Percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	k := int(math.Ceil(p/100*float64(len(s)))) - 1
	if k < 0 {
		k = 0
	}
	if k >= len(s) {
		k = len(s) - 1
	}
	return s[k]
}

// Merge concatenates the latencies of several kept runs, target by target.
// Rounds stay paired because every run measures every target once per round.
func Merge(runs [][]Result) []Result {
	if len(runs) == 0 {
		return nil
	}
	out := make([]Result, len(runs[0]))
	for i := range out {
		out[i].Name = runs[0][i].Name
		for _, r := range runs {
			out[i].Latencies = append(out[i].Latencies, r[i].Latencies...)
		}
	}
	return out
}

// report prints, per run and merged, the latency percentiles and the
// paired overhead of every gateway against the direct path.
func report(w io.Writer, runs [][]Result, rounds, warm int) {
	fmt.Fprintf(w, "kept runs=%d, rounds per run=%d, warm-up=%d\n", len(runs), rounds, warm)
	if len(runs) == 0 {
		fmt.Fprintln(w, "no run kept: no measurement")
		return
	}
	for k, res := range runs {
		fmt.Fprintf(w, "run %d overhead p50:", k)
		for _, r := range res[1:] {
			ov, _ := Overhead(r, res[0])
			fmt.Fprintf(w, " %s=%s", r.Name, Percentile(ov, 50))
		}
		fmt.Fprintln(w)
	}
	all := Merge(runs)
	fmt.Fprintf(w, "%-8s %10s %10s %10s\n", "target", "p50", "p90", "p99")
	for _, r := range all {
		fmt.Fprintf(w, "%-8s %10s %10s %10s\n", r.Name, Percentile(r.Latencies, 50), Percentile(r.Latencies, 90), Percentile(r.Latencies, 99))
	}
	fmt.Fprintln(w, "overhead vs direct, paired per round (all kept runs):")
	for _, r := range all[1:] {
		ov, _ := Overhead(r, all[0])
		fmt.Fprintf(w, "%-8s %10s %10s %10s\n", r.Name, Percentile(ov, 50), Percentile(ov, 90), Percentile(ov, 99))
	}
}

// Overhead returns, round by round, gateway latency minus the direct
// latency of the same round.
func Overhead(gw, direct Result) ([]time.Duration, error) {
	if len(gw.Latencies) != len(direct.Latencies) {
		return nil, fmt.Errorf("%s has %d rounds, %s has %d", gw.Name, len(gw.Latencies), direct.Name, len(direct.Latencies))
	}
	out := make([]time.Duration, len(gw.Latencies))
	for i := range out {
		out[i] = gw.Latencies[i] - direct.Latencies[i]
	}
	return out, nil
}
