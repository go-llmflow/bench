//go:build unix

package e2e

// Can LiteLLM exceed a team max_budget despite its pre-call budget
// reservation? Source read: litellm/proxy/spend_tracking/budget_reservation.py
// at v1.106.5.
//
//   - _apply_over_budget_reservation_policy (l.171-207): when the estimate does
//     not fit, the reservation is resized to what is left and the request is
//     admitted, unless general_settings.fail_closed_budget_enforcement is on.
//   - _estimate_output_tokens (l.1651-1670): the reservation is
//     min(client max_tokens or 16384, model_info.max_output_tokens or 16384).
//
// Three scenarios, each on a fresh PostgreSQL and Redis (throw-away docker
// containers) and a fresh LiteLLM, in front of a recording fake vLLM
// (fakeupstream) that bills like vLLM: it stops at the request's max_tokens
// when the body carries one, and generates its whole answer when it does not.
// The test measures; it asserts only what was measured on 2026-10-28 and
// writes every number to BENCH_LITELLM_RESULTS (JSON) when that is set.
//
// Skipped unless BENCH_LITELLM_BIN names a litellm 1.106.x executable
// ("litellm[proxy]" in a throw-away venv) and docker answers.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
)

const (
	envLiteLLMBin     = "BENCH_LITELLM_BIN"
	envLiteLLMResults = "BENCH_LITELLM_RESULTS"
	envLiteLLMPG      = "BENCH_LITELLM_PG_IMAGE"    // default postgres:18
	envLiteLLMRedis   = "BENCH_LITELLM_REDIS_IMAGE" // default redis:7

	// Prices in USD per token, declared in model_info.
	lbIn, lbOut = 1e-6, 1e-6
)

// lbScenario is one run.
type lbScenario struct {
	Name          string  `json:"name"`
	FailClosed    bool    `json:"fail_closed_budget_enforcement"`
	ClientMax     int     `json:"client_max_tokens"`   // 0: the client sends none
	Generate      int     `json:"fake_generates"`      // completion tokens the fake wants to emit
	MaxOutput     int     `json:"model_max_output"`    // model_info.max_output_tokens (0: not declared)
	Requests      int     `json:"concurrent_requests"` // all sent at once
	MaxBudget     float64 `json:"max_budget"`
	WantOvershoot bool    `json:"-"` // what was measured; asserted
}

// lbResult is what the run measured.
type lbResult struct {
	lbScenario
	LiteLLMVersion      string    `json:"litellm_version"`
	Config              string    `json:"litellm_config"`
	Admitted            int       `json:"admitted"`
	Refused             int       `json:"refused_budget"`
	OtherErrors         []string  `json:"other_errors,omitempty"`
	RefusalExample      string    `json:"refusal_example,omitempty"`
	FinalSpend          float64   `json:"final_spend_team_info"`
	FakeBilledCost      float64   `json:"cost_of_tokens_the_fake_served"`
	OvershootUSD        float64   `json:"overshoot_usd"`
	OvershootPct        float64   `json:"overshoot_pct"`
	FollowUpStatus      int       `json:"follow_up_request_status_after_settle"`
	UpstreamRequests    int       `json:"upstream_requests"`
	UpstreamWithMax     int       `json:"upstream_requests_carrying_max_tokens"`
	UpstreamMaxTokens   []float64 `json:"upstream_max_tokens_values,omitempty"`
	UpstreamBodyExample string    `json:"upstream_body_example"`
	RedisSpendKeys      []string  `json:"redis_spend_keys"`
}

func TestLiteLLMBudgetOvershoot(t *testing.T) {
	bin := os.Getenv(envLiteLLMBin)
	if bin == "" {
		t.Skipf("%s not set: no litellm 1.106.x to test", envLiteLLMBin)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found")
	}
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Skipf("docker does not answer: %v: %s", err, out)
	}
	scenarios := []lbScenario{
		// 1. Default mode. The worst case of one request is 4000 tokens (0.004
		// USD) and the room is 0.01: two requests reserve 0.008, the third is
		// resized to the 0.002 left and admitted, then the room is gone.
		{Name: "1-default-resize", Requests: 10, MaxBudget: 0.01, ClientMax: 4000, Generate: 4000, WantOvershoot: true},
		// 2. Strict mode, no client max_tokens, a vLLM that then generates
		// 20000 tokens. LiteLLM reserves 16384 tokens (0.016384): three of
		// them fit in 0.05, and each really costs 0.02.
		{Name: "2-fail-closed-no-max-tokens", FailClosed: true, Requests: 10, MaxBudget: 0.05, Generate: 20000, WantOvershoot: true},
		// 3. Control: strict mode, explicit max_tokens, a model that uses all
		// of it.
		{Name: "3-fail-closed-explicit-max-tokens", FailClosed: true, Requests: 10, MaxBudget: 0.01, ClientMax: 4000, Generate: 20000},
	}
	var all []lbResult
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			r := runLiteLLMScenario(t, bin, sc)
			all = append(all, r)
			t.Logf("%s: max_budget=%.6f final_spend=%.6f (fake billed %.6f) overshoot=%.6f USD = %.1f%%; admitted=%d refused=%d other=%v; upstream %d requests, %d carried max_tokens %v; redis keys %v",
				sc.Name, sc.MaxBudget, r.FinalSpend, r.FakeBilledCost, r.OvershootUSD, r.OvershootPct, r.Admitted, r.Refused, r.OtherErrors,
				r.UpstreamRequests, r.UpstreamWithMax, r.UpstreamMaxTokens, r.RedisSpendKeys)
			if len(r.RedisSpendKeys) == 0 {
				t.Errorf("no spend:* key in Redis: the run did not exercise the Redis counters")
			}
			if len(r.OtherErrors) > 0 {
				t.Errorf("requests failed for a reason other than the budget: %v", r.OtherErrors)
			}
			if sc.WantOvershoot && r.OvershootUSD <= 1e-9 {
				t.Errorf("expected the budget to be exceeded, final spend %.6f <= %.6f", r.FinalSpend, sc.MaxBudget)
			}
			if !sc.WantOvershoot && r.OvershootUSD > 1e-9 {
				t.Errorf("expected the budget to hold, final spend %.6f > %.6f", r.FinalSpend, sc.MaxBudget)
			}
		})
	}
	if p := os.Getenv(envLiteLLMResults); p != "" {
		b, _ := json.MarshalIndent(all, "", "  ")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("results written to %s", p)
	}
}

func runLiteLLMScenario(t *testing.T, bin string, sc lbScenario) lbResult {
	t.Helper()
	res := lbResult{lbScenario: sc}

	// The fake vLLM, in process, recording every body it receives.
	var mu sync.Mutex
	var bodies []json.RawMessage
	var billed float64
	fake := httptest.NewServer(fakeupstream.NewServer(fakeupstream.Options{
		Model: "fake", Reply: "ok", Generate: sc.Generate, Delay: 1500 * time.Millisecond,
	}, func(r fakeupstream.Record) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/chat/completions") {
			mu.Lock()
			bodies = append(bodies, r.Body)
			mu.Unlock()
		}
		return nil
	}))
	t.Cleanup(fake.Close)

	pgPort, redisPort, redisName := startContainers(t)
	master := "sk-bench-" + randomHex(12) // generated per run, never reused
	port := strings.Split(freePort(t), ":")[1]
	dir := t.TempDir()
	cfg := filepath.Join(dir, "litellm.yaml")
	maxOut := ""
	if sc.MaxOutput > 0 {
		maxOut = "\n      max_output_tokens: " + strconv.Itoa(sc.MaxOutput)
	}
	yaml := fmt.Sprintf(`model_list:
  - model_name: m
    litellm_params:
      model: hosted_vllm/fake
      api_base: %s/v1
      api_key: not-a-secret
    model_info:
      input_cost_per_token: %g
      output_cost_per_token: %g%s
litellm_settings:
  cache: true
  cache_params:
    type: redis
    host: 127.0.0.1
    port: %s
router_settings:
  redis_host: 127.0.0.1
  redis_port: %s
general_settings:
  master_key: %s
  database_url: postgresql://postgres:bench@127.0.0.1:%s/litellm
  proxy_batch_write_at: 5
  fail_closed_budget_enforcement: %v
`, fake.URL, lbIn, lbOut, maxOut, redisPort, redisPort, master, pgPort, sc.FailClosed)
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	res.Config = strings.ReplaceAll(yaml, master, "<generated>")

	cmd := exec.Command(bin, "--config", cfg, "--host", "127.0.0.1", "--port", port, "--num_workers", "1")
	cmd.Stdin = nil
	logf, err := os.Create(filepath.Join(dir, "litellm.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // the whole group, by PID
		_ = cmd.Wait()
		logf.Close()
	})
	base := "http://127.0.0.1:" + port
	lbWaitFor(t, 240*time.Second, "litellm readiness", func() bool {
		resp, err := http.Get(base + "/health/readiness")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	}, func() string { b, _ := os.ReadFile(logf.Name()); return tail(b, 3000) })

	vout, _ := exec.Command(bin, "--version").CombinedOutput()
	res.LiteLLMVersion = strings.TrimSpace(string(vout))
	if !strings.Contains(res.LiteLLMVersion, "1.106.") {
		t.Logf("warning: measured with %q, not a 1.106.x", res.LiteLLMVersion)
	}

	// A team with the budget, and a key in it.
	var team struct {
		TeamID string `json:"team_id"`
	}
	mustPost(t, base+"/team/new", master, fmt.Sprintf(`{"team_alias":"bench","max_budget":%g,"models":["m"]}`, sc.MaxBudget), &team)
	var key struct {
		Key string `json:"key"`
	}
	mustPost(t, base+"/key/generate", master, fmt.Sprintf(`{"team_id":%q,"models":["m"]}`, team.TeamID), &key)

	// N concurrent requests, released together.
	body := `{"model":"m","messages":[{"role":"user","content":"Say hello."}]`
	if sc.ClientMax > 0 {
		body += fmt.Sprintf(`,"max_tokens":%d`, sc.ClientMax)
	}
	body += `}`
	statuses := make([]int, sc.Requests)
	texts := make([]string, sc.Requests)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range sc.Requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses[i], texts[i] = rawPost(base+"/v1/chat/completions", key.Key, body)
		}()
	}
	close(start)
	wg.Wait()
	for i, st := range statuses {
		switch {
		case st == 200:
			res.Admitted++
		case strings.Contains(texts[i], "udget"):
			res.Refused++
			if res.RefusalExample == "" {
				res.RefusalExample = fmt.Sprintf("HTTP %d: %s", st, tail([]byte(texts[i]), 400))
			}
		default:
			res.OtherErrors = append(res.OtherErrors, fmt.Sprintf("HTTP %d: %s", st, tail([]byte(texts[i]), 200)))
		}
	}

	// Spend is written to PostgreSQL in batches (proxy_batch_write_at): wait
	// until team spend has been stable for four polls.
	var last float64 = -1
	stable := 0
	lbWaitFor(t, 180*time.Second, "team spend to settle", func() bool {
		var ti struct {
			TeamInfo struct {
				Spend float64 `json:"spend"`
			} `json:"team_info"`
		}
		if err := getJSON(base+"/team/info?team_id="+team.TeamID, master, &ti); err != nil {
			return false
		}
		if ti.TeamInfo.Spend > 0 && ti.TeamInfo.Spend == last {
			stable++
		} else {
			stable = 0
		}
		last = ti.TeamInfo.Spend
		return stable >= 4
	}, func() string { return fmt.Sprintf("last spend %v", last) })
	res.FinalSpend = last
	st, _ := rawPost(base+"/v1/chat/completions", key.Key, body)
	res.FollowUpStatus = st

	mu.Lock()
	res.UpstreamRequests = len(bodies)
	for _, b := range bodies {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			if v, ok := m[k].(float64); ok {
				res.UpstreamWithMax++
				res.UpstreamMaxTokens = append(res.UpstreamMaxTokens, v)
				break
			}
		}
	}
	if len(bodies) > 0 {
		res.UpstreamBodyExample = string(bodies[0])
	}
	// What the fake actually served, priced with the declared prices: it
	// answers the follow-up too when that one was admitted, so count only the
	// first Admitted answers, which all have the same usage.
	for _, b := range bodies[:min(len(bodies), res.Admitted)] {
		billed += fakeCost(b, sc.Generate)
	}
	mu.Unlock()
	res.FakeBilledCost = billed
	res.OvershootUSD = math.Max(0, res.FinalSpend-sc.MaxBudget)
	res.OvershootPct = 100 * (res.FinalSpend - sc.MaxBudget) / sc.MaxBudget

	out, _ := exec.Command("docker", "exec", redisName, "redis-cli", "--scan", "--pattern", "spend:*").CombinedOutput()
	res.RedisSpendKeys = strings.Split(strings.TrimSpace(string(out)), "\n") // tag keys hold spaces
	return res
}

// fakeCost is the price of one answer of the fake for body: its prompt words
// (fakeupstream's tokenizer) plus its completion tokens.
func fakeCost(body json.RawMessage, generate int) float64 {
	var req struct {
		Messages  []struct{ Content string } `json:"messages"`
		MaxTokens *int                       `json:"max_tokens"`
	}
	_ = json.Unmarshal(body, &req)
	prompt := 0
	for _, m := range req.Messages {
		prompt += len(strings.Fields(m.Content))
	}
	out := generate
	if req.MaxTokens != nil && *req.MaxTokens < out {
		out = *req.MaxTokens
	}
	return float64(prompt)*lbIn + float64(out)*lbOut
}

// startContainers starts a throw-away PostgreSQL and Redis, removed at the
// end of the test, and returns their host ports and the Redis container name.
func startContainers(t *testing.T) (pg, redis, redisName string) {
	t.Helper()
	pgImage, redisImage := envOr(envLiteLLMPG, "postgres:18"), envOr(envLiteLLMRedis, "redis:7")
	suffix := randomHex(4)
	pgName := "bench-pg-" + suffix
	redisName = "bench-redis-" + suffix
	docker(t, "run", "-d", "--rm", "--name", pgName, "-e", "POSTGRES_PASSWORD=bench", "-e", "POSTGRES_DB=litellm", "-p", "127.0.0.1::5432", pgImage)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", pgName).Run() })
	docker(t, "run", "-d", "--rm", "--name", redisName, "-p", "127.0.0.1::6379", redisImage)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", redisName).Run() })
	pg = hostPort(t, pgName, "5432/tcp")
	redis = hostPort(t, redisName, "6379/tcp")
	lbWaitFor(t, 90*time.Second, "postgres", func() bool {
		return exec.Command("docker", "exec", pgName, "pg_isready", "-U", "postgres", "-d", "litellm").Run() == nil &&
			exec.Command("docker", "exec", pgName, "psql", "-U", "postgres", "-d", "litellm", "-c", "select 1").Run() == nil
	}, nil)
	lbWaitFor(t, 30*time.Second, "redis", func() bool {
		return exec.Command("docker", "exec", redisName, "redis-cli", "ping").Run() == nil
	}, nil)
	return pg, redis, redisName
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// hostPort is the host port docker published for container port cport.
func hostPort(t *testing.T, name, cport string) string {
	t.Helper()
	out := docker(t, "port", name, cport)
	for _, line := range strings.Split(out, "\n") {
		if host, port, err := net.SplitHostPort(strings.TrimSpace(line)); err == nil && (host == "127.0.0.1") {
			return port
		}
	}
	t.Fatalf("no 127.0.0.1 mapping for %s %s in %q", name, cport, out)
	return ""
}

func lbWaitFor(t *testing.T, d time.Duration, what string, ok func() bool, diag func() string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Second)
	}
	extra := ""
	if diag != nil {
		extra = ": " + diag()
	}
	t.Fatalf("timeout waiting for %s%s", what, extra)
}

func tail(b []byte, n int) string {
	b = bytes.TrimSpace(b)
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

func mustPost(t *testing.T, url, bearer, body string, v any) {
	t.Helper()
	if _, err := postJSON(url, bearer, body, nil, v); err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
}

// rawPost returns the status and the body whatever the status is (0 and the
// error on a transport failure).
func rawPost(url, bearer, body string) (int, string) {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}
