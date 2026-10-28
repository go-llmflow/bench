// Package e2e checks llmflow-gateway against a REAL vLLM. It skips itself
// unless BENCH_VLLM_URL names a running vLLM; see the README for how vLLM
// must be started for each group of checks.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// env names. None of them holds a secret: every key is generated per run.
const (
	envVLLM        = "BENCH_VLLM_URL"         // chat model, e.g. http://127.0.0.1:8000
	envModel       = "BENCH_VLLM_MODEL"       // served model name (default: first of /v1/models)
	envGateway     = "BENCH_GATEWAY_BIN"      // llmflow-gateway binary (default: from PATH)
	envEmbedURL    = "BENCH_VLLM_EMBED_URL"   // optional embedding model server
	envRerankURL   = "BENCH_VLLM_RERANK_URL"  // optional score (rerank) model server
	envKVShortage  = "BENCH_VLLM_KV_SHORTAGE" // "1": vLLM runs with a tiny KV cache
	envMaxNumSeqs  = "BENCH_VLLM_MAX_NUM_SEQS"
	defaultMaxSeqs = 2
)

var client = &http.Client{Timeout: 10 * time.Minute}

type upstream struct {
	URL, Model string
}

// Gateway is one llmflow-gateway process in front of the vLLM servers.
type Gateway struct {
	URL    string
	Keys   map[string]string // team -> virtual key
	Master string            // the master key (admin API), generated per run
	cmd    *exec.Cmd
	log    *bytes.Buffer
}

func vllmChat(t *testing.T) upstream {
	t.Helper()
	u := os.Getenv(envVLLM)
	if u == "" {
		t.Skipf("%s not set: no real vLLM to test against", envVLLM)
	}
	return upstream{URL: strings.TrimRight(u, "/"), Model: modelOf(t, u, os.Getenv(envModel))}
}

func optional(t *testing.T, env string) upstream {
	t.Helper()
	u := os.Getenv(env)
	if u == "" {
		t.Skipf("%s not set", env)
	}
	return upstream{URL: strings.TrimRight(u, "/"), Model: modelOf(t, u, "")}
}

func modelOf(t *testing.T, base, given string) string {
	t.Helper()
	if given != "" {
		return given
	}
	var m struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := getJSON(strings.TrimRight(base, "/")+"/v1/models", "", &m); err != nil || len(m.Data) == 0 {
		t.Fatalf("models of %s: %v", base, err)
	}
	return m.Data[0].ID
}

func maxNumSeqs() int {
	if n, err := strconv.Atoi(os.Getenv(envMaxNumSeqs)); err == nil && n > 0 {
		return n
	}
	return defaultMaxSeqs
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// gatewayConfig writes the HCL configuration. Organisation A owns the pool
// (reservation 1 of a capacity of 2); B has a share whose Hamilton
// apportionment is 0 slots, so every request of B borrows. That makes the
// role of each request certain without depending on timing.
func gatewayConfig(dir, listen string, chat upstream, extra []upstream, names []string) string {
	return gatewayConfigWith(dir, listen, chat, extra, names, "", "")
}

// gatewayConfigWith is gatewayConfig with attributes added to the "chat"
// model block and HCL appended at the end (other pools and models).
func gatewayConfigWith(dir, listen string, chat upstream, extra []upstream, names []string, chatAttrs, tail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `listen = %q
master_key_file = %q
cache_isolation = "org"
cache_salt_key_file = %q
auth_failures_per_minute = 0
store {
  kind = "memory"
}
health {
  path = "/health"
}
org "orgA" {
  share = 0.5
  team "a1" {}
  team "a2" {}
}
org "orgB" {
  share = 0.25
  team "b1" {}
}
pool "cpu" { capacity = 2 }
model "chat" {
  upstream_model = %q
  pool           = "cpu"
  class          = "interactive"
  key_file       = %q
  upstream "v0" { base_url = %q }
%s}
`, listen, filepath.Join(dir, "master.key"), filepath.Join(dir, "salt.key"), chat.Model, filepath.Join(dir, "upstream.key"), chat.URL, chatAttrs)
	for i, u := range extra {
		fmt.Fprintf(&b, `model %q {
  upstream_model = %q
  pool           = "cpu"
  class          = "interactive"
  key_file       = %q
  upstream "v0" { base_url = %q }
}
`, names[i], u.Model, filepath.Join(dir, "upstream.key"), u.URL)
	}
	b.WriteString(strings.ReplaceAll(tail, "UPSTREAM_KEY_FILE", filepath.Join(dir, "upstream.key")))
	return b.String()
}

func gatewayBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(envGateway); p != "" {
		return p
	}
	p, err := exec.LookPath("llmflow-gateway")
	if err != nil {
		t.Skipf("%s not set and llmflow-gateway not on PATH", envGateway)
	}
	return p
}

// StartGateway runs llmflow-gateway with freshly generated keys and creates
// one virtual key per team.
func StartGateway(t *testing.T, chat upstream, extra []upstream, names []string) *Gateway {
	t.Helper()
	return StartGatewayWith(t, chat, extra, names, "", "")
}

// StartGatewayWith is StartGateway with gatewayConfigWith's additions.
func StartGatewayWith(t *testing.T, chat upstream, extra []upstream, names []string, chatAttrs, tail string) *Gateway {
	t.Helper()
	bin := gatewayBin(t)
	dir := t.TempDir()
	for _, f := range []string{"master.key", "salt.key", "upstream.key"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("e2e-"+randomHex(32)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listen := freePort(t)
	cfg := filepath.Join(dir, "gateway.hcl")
	if err := os.WriteFile(cfg, []byte(gatewayConfigWith(dir, listen, chat, extra, names, chatAttrs, tail)), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{URL: "http://" + listen, Keys: map[string]string{}, log: &bytes.Buffer{}}
	g.cmd = exec.Command(bin, "serve", "--config", cfg)
	g.cmd.Stdin = nil
	g.cmd.Stdout = g.log
	g.cmd.Stderr = g.log
	if err := g.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.cmd.Process.Kill()
		g.cmd.Wait()
		if t.Failed() {
			t.Logf("gateway log:\n%s", g.log.String())
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(g.URL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway not healthy: %v\n%s", err, g.log.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	master, _ := os.ReadFile(filepath.Join(dir, "master.key"))
	g.Master = strings.TrimSpace(string(master))
	for _, team := range []string{"a1", "a2", "b1"} {
		var k struct{ ID, Key string }
		body := fmt.Sprintf(`{"team":%q,"name":"e2e"}`, team)
		if code, err := postJSON(g.URL+"/admin/keys", strings.TrimSpace(string(master)), body, nil, &k); err != nil || code != 201 || k.Key == "" {
			t.Fatalf("key for %s: HTTP %d %v", team, code, err)
		}
		g.Keys[team] = k.Key
	}
	return g
}

func getJSON(url, bearer string, v any) error {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, v)
}

// postJSON returns the status; v is decoded only on 2xx.
func postJSON(url, bearer, body string, hdr map[string]string, v any) (int, error) {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, x := range hdr {
		req.Header.Set(k, x)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	if v != nil {
		return resp.StatusCode, json.Unmarshal(b, v)
	}
	return resp.StatusCode, nil
}

// Stream is what a client saw of one streamed chat completion.
type Stream struct {
	Name       string
	Sent       time.Time
	FirstToken time.Time
	Done       time.Time
	Tokens     []time.Time // arrival of every content chunk
	Text       string
	Usage      map[string]any
	Err        error
}

// MaxGap is the longest pause between two content chunks: a preempted
// request stops receiving tokens until it is rescheduled.
func (s *Stream) MaxGap() time.Duration {
	var m time.Duration
	for i := 1; i < len(s.Tokens); i++ {
		if d := s.Tokens[i].Sub(s.Tokens[i-1]); d > m {
			m = d
		}
	}
	return m
}

// ChatStream sends one streamed chat completion and records the time of
// every content chunk.
func ChatStream(ctx context.Context, name, url, bearer, body string, hdr map[string]string) *Stream {
	s := &Stream{Name: name}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, x := range hdr {
		req.Header.Set(k, x)
	}
	s.Sent = time.Now()
	resp, err := client.Do(req)
	if err != nil {
		s.Err = err
		return s
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		s.Err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
		return s
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var c struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			s.Err = fmt.Errorf("chunk %q: %w", data, err)
			return s
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				now := time.Now()
				if s.FirstToken.IsZero() {
					s.FirstToken = now
				}
				s.Tokens = append(s.Tokens, now)
				s.Text += ch.Delta.Content
			}
		}
		if c.Usage != nil {
			s.Usage = c.Usage
		}
	}
	s.Done = time.Now()
	if err := sc.Err(); err != nil {
		s.Err = err
	}
	return s
}

// chatBody builds a streamed chat request of exactly maxTokens generated
// tokens (ignore_eos), so the length of every request is known.
func chatBody(model, prompt string, maxTokens int, extra string) string {
	p, _ := json.Marshal(prompt)
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%s}],"max_tokens":%d,"ignore_eos":true,"temperature":0,"stream":true,"stream_options":{"include_usage":true}%s}`,
		model, p, maxTokens, extra)
}

// Metrics reads a Prometheus text exposition and sums every series of each
// metric name whose labels contain all of match (label="value" pairs).
func Metrics(t *testing.T, url string) map[string]float64 {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		name, labels, v, ok := parseSample(sc.Text())
		if !ok {
			continue
		}
		out[name] += v
		if labels != "" {
			out[name+"{"+labels+"}"] += v
		}
	}
	return out
}

// parseSample splits `name{labels} value [timestamp]`.
func parseSample(line string) (name, labels string, v float64, ok bool) {
	if line == "" || line[0] == '#' {
		return
	}
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return
		}
		name, labels, rest = line[:i], line[i+1:j], strings.TrimSpace(line[j+1:])
	} else {
		f := strings.Fields(line)
		if len(f) < 2 {
			return
		}
		name, rest = f[0], f[1]
	}
	f := strings.Fields(rest)
	if len(f) < 1 {
		return
	}
	x, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return
	}
	return name, labels, x, true
}

// sumMatching adds the series of name whose label set contains every pair.
func sumMatching(m map[string]float64, name string, pairs ...string) float64 {
	var s float64
	for k, v := range m {
		if !strings.HasPrefix(k, name+"{") {
			continue
		}
		all := true
		for _, p := range pairs {
			if !strings.Contains(k, p) {
				all = false
				break
			}
		}
		if all {
			s += v
		}
	}
	return s
}
