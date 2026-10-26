// Package anthropic checks llmflow-gateway's Anthropic upstream (gateway
// v0.12.0) end to end: the gateway binary in front of fakeupstream in its
// Anthropic mode, and, when BENCH_ANTHROPIC_KEY_FILE names a file holding a
// real API key, in front of api.anthropic.com. No key is ever read from the
// environment or a command line: the variable names a file.
package anthropic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
)

const (
	envGateway = "BENCH_GATEWAY_BIN"        // llmflow-gateway v0.12.0 or later
	envKeyFile = "BENCH_ANTHROPIC_KEY_FILE" // a file holding a real Anthropic API key
	envModel   = "BENCH_ANTHROPIC_MODEL"    // default claude-haiku-5-5
)

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type gateway struct {
	url, master string
	keys        map[string]string // team → virtual key
}

// start runs the gateway with an Anthropic model in front of upstream;
// orgKeys maps an organisation to the file of its API key.
func start(t *testing.T, upstream string, orgKeys map[string]string, model string) *gateway {
	t.Helper()
	bin := os.Getenv(envGateway)
	if bin == "" {
		if p, err := exec.LookPath("llmflow-gateway"); err == nil {
			bin = p
		} else {
			t.Skipf("%s not set and llmflow-gateway not on PATH", envGateway)
		}
	}
	dir := t.TempDir()
	w := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	g := &gateway{master: "e2e-" + randomHex(24), keys: map[string]string{}}
	port := freePort(t)
	g.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	var orgs, keyBlocks []string
	for org, f := range orgKeys {
		orgs = append(orgs, fmt.Sprintf("%q", org))
		keyBlocks = append(keyBlocks, fmt.Sprintf("  org_key %q { key_file = %q }", org, f))
	}
	cfg := fmt.Sprintf(`listen = "127.0.0.1:%d"
master_key_file = %q
cache_salt_key_file = %q
auth_failures_per_minute = 0
store {
  kind = "memory"
}
org "orgA" {
  share = 0.5
  team "teamA" {}
}
org "orgB" {
  share = 0.5
  team "teamB" {}
}
pool "anthropic" { capacity = 4 }
model "claude" {
  provider       = "anthropic"
  upstream_model = %q
  pool           = "anthropic"
  allow_external = true
  orgs           = [%s]
  cost {
    unit                    = "USD"
    price_date              = "2026-10-26"
    input_per_mtok          = 2
    output_per_mtok         = 10
    cached_per_mtok         = 0.1
    cache_write_5m_per_mtok = 2.5
    cache_write_1h_per_mtok = 4
  }
  upstream "api" { base_url = %q }
%s
}
guardrail "pii" {
  type = "pii"
  orgs = ["orgA", "orgB"]
}
guardrail "secrets" {
  type = "hide_secrets"
  orgs = ["orgA", "orgB"]
}
`, port, w("master.key", g.master), w("salt.key", randomHex(32)), model, strings.Join(orgs, ", "), upstream, strings.Join(keyBlocks, "\n"))
	path := w("gateway.hcl", cfg)
	if out, err := exec.Command(bin, "check-config", "--config", path).CombinedOutput(); err != nil {
		t.Fatalf("check-config: %v\n%s", err, out)
	}
	logs := &bytes.Buffer{}
	cmd := exec.Command(bin, "serve", "--config", path)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("gateway log:\n%s", logs)
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(g.url + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway not healthy: %v\n%s", err, logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "off the site") {
		t.Errorf("no startup warning that prompts leave the site:\n%s", logs)
	}
	for _, team := range []string{"teamA", "teamB"} {
		code, body := g.call(t, "POST", "/admin/keys", "Authorization", "Bearer "+g.master, fmt.Sprintf(`{"team":%q,"name":"e2e"}`, team))
		var k struct{ Key string }
		if code != 201 || json.Unmarshal([]byte(body), &k) != nil {
			t.Fatalf("key for %s: %d %s", team, code, body)
		}
		g.keys[team] = k.Key
	}
	return g
}

func (g *gateway) call(t *testing.T, method, path, hk, hv, body string, more ...string) (int, string) {
	t.Helper()
	code, _, out := g.callH(t, method, path, hk, hv, body, more...)
	return code, out
}

func (g *gateway) callH(t *testing.T, method, path, hk, hv, body string, more ...string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(method, g.url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if hk != "" {
		req.Header.Set(hk, hv)
	}
	for i := 0; i+1 < len(more); i += 2 {
		req.Header.Set(more[i], more[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

const pii = "jean.dupont@example.fr"

func message(stream bool) string {
	return fmt.Sprintf(`{"model":"claude","max_tokens":16,"stream":%v,"priority":-1,"cache_salt":"x","messages":[{"role":"user","content":"Say hi. Contact: %s"}]}`, stream, pii)
}

// The gateway binary in front of the fake Anthropic API.
func TestGatewayInFrontOfFakeAnthropic(t *testing.T) {
	var mu sync.Mutex
	var recs []fakeupstream.Record
	fake := fakeupstream.NewServer(fakeupstream.Options{Reply: "hi there", CacheRead: 100, CacheWrite5m: 30, CacheWrite1h: 20,
		Workspace: "per-key", RateLimitHeaders: true}, func(r fakeupstream.Record) error {
		mu.Lock()
		defer mu.Unlock()
		recs = append(recs, r)
		return nil
	})
	up := httptest.NewServer(fake)
	t.Cleanup(up.Close)
	dir := t.TempDir()
	keyA, keyB := "sk-ant-fake-a-"+randomHex(8), "sk-ant-fake-b-"+randomHex(8)
	fa, fb := filepath.Join(dir, "a.key"), filepath.Join(dir, "b.key")
	os.WriteFile(fa, []byte(keyA), 0o600)
	os.WriteFile(fb, []byte(keyB), 0o600)
	g := start(t, up.URL, map[string]string{"orgA": fa, "orgB": fb}, "claude-sonnet-5-5")
	last := func() fakeupstream.Record {
		mu.Lock()
		defer mu.Unlock()
		if len(recs) == 0 {
			t.Fatal("the provider received nothing")
		}
		return recs[len(recs)-1]
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(recs) }

	// Claude Code's way: the gateway key as x-api-key.
	code, h, body := g.callH(t, "POST", "/v1/messages", "X-Api-Key", g.keys["teamA"], message(false),
		"Anthropic-Version", "2023-06-01", "Anthropic-Beta", "oauth-2025-04-20")
	if code != 200 || !strings.Contains(body, "hi there") {
		t.Fatalf("teamA: %d %s", code, body)
	}
	if h.Get("Anthropic-Ratelimit-Tokens-Remaining") != "" {
		t.Error("the shared organisation's rate-limit headers reached the client")
	}
	r := last()
	hd := http.Header(r.Headers)
	if hd.Get("X-Api-Key") != keyA || hd.Get("Authorization") != "" || hd.Get("Anthropic-Beta") != "" {
		t.Errorf("at the provider: x-api-key %q authorization %q beta %q", hd.Get("X-Api-Key"), hd.Get("Authorization"), hd.Get("Anthropic-Beta"))
	}
	if strings.Contains(string(r.Body), pii) || strings.Contains(string(r.Body), "cache_salt") || strings.Contains(string(r.Body), "priority") {
		t.Errorf("body at the provider: %s", r.Body)
	}
	// teamB, streamed: its own key.
	if code, body := g.call(t, "POST", "/v1/messages", "Authorization", "Bearer "+g.keys["teamB"], message(true)); code != 200 || !strings.Contains(body, "message_stop") {
		t.Fatalf("teamB stream: %d %s", code, body)
	}
	if hd := http.Header(last().Headers); hd.Get("X-Api-Key") != keyB {
		t.Errorf("teamB used key %q", hd.Get("X-Api-Key"))
	}
	// OpenAI route refused.
	if code, body := g.call(t, "POST", "/v1/chat/completions", "Authorization", "Bearer "+g.keys["teamA"], `{"model":"claude","messages":[{"role":"user","content":"x"}]}`); code != 400 {
		t.Errorf("chat completions: %d %s", code, body)
	}

	// Accounting in USD, from the provider's usage: 4 words uncached
	// ("Say hi. Contact: <masked>"), 100 read, 30 + 20 written, 2 out.
	// The records are written asynchronously: poll a little.
	var rep string
	for i := 0; i < 40; i++ {
		_, rep = g.call(t, "GET", "/admin/spend?from=2000-01-01&to=2100-01-01&group_by=org", "Authorization", "Bearer "+g.master, "")
		if strings.Count(rep, `"unit":"USD"`) >= 2 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	var report struct {
		Rows []struct {
			Org          string
			Unit         string
			Cost         float64
			Requests     int
			PromptTokens int `json:"prompt_tokens"`
			CachedTokens int `json:"cached_tokens"`
		}
	}
	if err := json.Unmarshal([]byte(rep), &report); err != nil {
		t.Fatalf("spend %s: %v", rep, err)
	}
	for _, row := range report.Rows {
		in := float64(row.PromptTokens - row.CachedTokens - 50)
		want := (in*2 + 100*0.1 + 30*2.5 + 20*4 + 2*10) / 1e6
		t.Logf("spend %s: %d requests, %d prompt tokens (%d read), %v %s; expected %v", row.Org, row.Requests, row.PromptTokens, row.CachedTokens, row.Cost, row.Unit, want)
		if row.Unit != "USD" || row.Requests != 1 || row.CachedTokens != 100 || math.Abs(row.Cost-want) > 1e-12 {
			t.Errorf("row %+v, want USD %v", row, want)
		}
	}
	if len(report.Rows) != 2 {
		t.Errorf("spend rows %s", rep)
	}

	// retry-after honoured: the second request does not reach the provider.
	fake.SetStatus(429, "3")
	code, h, _ = g.callH(t, "POST", "/v1/messages", "Authorization", "Bearer "+g.keys["teamA"], message(false))
	if code != 429 || h.Get("Retry-After") != "3" {
		t.Fatalf("429: %d retry-after %q", code, h.Get("Retry-After"))
	}
	fake.SetStatus(0, "")
	n := count()
	code, h, _ = g.callH(t, "POST", "/v1/messages", "Authorization", "Bearer "+g.keys["teamA"], message(false))
	if code != 429 || h.Get("Retry-After") == "" || count() != n {
		t.Fatalf("inside the wait: %d retry-after %q, provider calls %d → %d", code, h.Get("Retry-After"), n, count())
	}
	if code, _ := g.call(t, "POST", "/v1/messages", "Authorization", "Bearer "+g.keys["teamB"], message(false)); code != 200 {
		t.Fatalf("the other organisation waited too: %d", code)
	}
	time.Sleep(3100 * time.Millisecond)
	if code, _ := g.call(t, "POST", "/v1/messages", "Authorization", "Bearer "+g.keys["teamA"], message(false)); code != 200 {
		t.Fatalf("after the wait: %d", code)
	}
}

// The gateway in front of the real Anthropic API, when a key file is given.
func TestGatewayInFrontOfRealAnthropic(t *testing.T) {
	keyFile := os.Getenv(envKeyFile)
	if keyFile == "" {
		t.Skipf("%s not set: no real Anthropic key (the variable names a file, never the key)", envKeyFile)
	}
	model := os.Getenv(envModel)
	if model == "" {
		model = "claude-haiku-5-5"
	}
	g := start(t, "https://api.anthropic.com", map[string]string{"orgA": keyFile}, model)
	code, h, body := g.callH(t, "POST", "/v1/messages", "Authorization", "Bearer "+g.keys["teamA"],
		`{"model":"claude","max_tokens":16,"messages":[{"role":"user","content":"Reply with the word ok."}]}`, "Anthropic-Version", "2023-06-01")
	t.Logf("real API: %d request-id %q %s", code, h.Get("Request-Id"), body)
	if code != 200 || h.Get("Request-Id") == "" {
		t.Fatalf("real API: %d", code)
	}
	var rep string
	for i := 0; i < 40 && !strings.Contains(rep, `"unit":"USD"`); i++ {
		time.Sleep(250 * time.Millisecond)
		_, rep = g.call(t, "GET", "/admin/spend?from=2000-01-01&to=2100-01-01&group_by=org", "Authorization", "Bearer "+g.master, "")
	}
	t.Logf("spend: %s", rep)
	if !strings.Contains(rep, `"unit":"USD"`) {
		t.Fatalf("no USD spend row: %s", rep)
	}
	if code, _ := g.call(t, "POST", "/v1/messages", "Authorization", "Bearer "+g.keys["teamB"], `{"model":"claude","max_tokens":4,"messages":[{"role":"user","content":"x"}]}`); code != 404 {
		t.Errorf("an organisation without access: %d, want 404", code)
	}
}
