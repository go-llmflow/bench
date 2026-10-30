// Package fluids checks llmflow-gateway's measured-energy accounting (gateway
// #97) end to end: a real gateway process scrapes a fake NVIDIA DCGM exporter
// while real chat requests of two organisations go through it, and
// GET /admin/fluids must give back, to the millijoule, the energy the
// exporter's counters moved by: through a driver reload (the counter restarts)
// and an exporter that is down for several scrapes.
//
// It skips itself unless BENCH_GATEWAY_BIN names a llmflow-gateway that has the
// fluids block (gateway v0.19.0 or later).
package fluids

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
)

const kWhMJ = 3_600_000_000 // a kilowatt-hour in millijoules

var client = &http.Client{Timeout: 30 * time.Second}

type gw struct {
	url, master string
	keys        map[string]string
}

func (g *gw) call(t *testing.T, method, path, bearer, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, g.url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b
}

func startGateway(t *testing.T, vllm, dcgm string) *gw {
	t.Helper()
	bin := os.Getenv("BENCH_GATEWAY_BIN")
	if bin == "" {
		t.Skip("BENCH_GATEWAY_BIN not set: no gateway with fluids to test")
	}
	dir := t.TempDir()
	for _, f := range []string{"master.key", "salt.key", "upstream.key"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("e2e-"+fmt.Sprint(time.Now().UnixNano())+f+"-0123456789abcdef\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listen := freePort(t)
	cfg := fmt.Sprintf(`listen = %q
master_key_file = %q
cache_isolation = "org"
cache_salt_key_file = %q
auth_failures_per_minute = 0
store {
  kind = "memory"
}
health {
  path     = "/health"
  interval = "1s"
}
org "orgA" {
  share = 0.75
  team "a1" {}
}
org "orgB" {
  share = 0.25
  team "b1" {}
}
pool "gpu" { capacity = 4 }
model "chat" {
  upstream_model = "fake"
  pool           = "gpu"
  key_file       = %q
  cost {
    input_per_mtok  = 1000
    output_per_mtok = 5000
    cached_per_mtok = 100
  }
  upstream "node1" {
    base_url = %q
    dcgm_url = %q
  }
}
fluids {
  pue           = 1.4
  price_per_kwh = 0.18
  currency      = "EUR"
  idle_energy   = "by_share"
  interval      = "1s"
  settle        = "1s"
}
`, listen, filepath.Join(dir, "master.key"), filepath.Join(dir, "salt.key"), filepath.Join(dir, "upstream.key"), vllm, dcgm+"/metrics")
	path := filepath.Join(dir, "gateway.hcl")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	cmd := exec.Command(bin, "serve", "--config", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("gateway log:\n%s", log.String())
		}
	})
	g := &gw{url: "http://" + listen, keys: map[string]string{}}
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(g.url + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway not healthy: %v\n%s", err, log.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	m, _ := os.ReadFile(filepath.Join(dir, "master.key"))
	g.master = strings.TrimSpace(string(m))
	for _, team := range []string{"a1", "b1"} {
		code, b := g.call(t, "POST", "/admin/keys", g.master, fmt.Sprintf(`{"team":%q,"name":"e2e"}`, team))
		var k struct{ Key string }
		if code != 201 || json.Unmarshal(b, &k) != nil || k.Key == "" {
			t.Fatalf("key for %s: %d %s", team, code, b)
		}
		g.keys[team] = k.Key
	}
	return g
}

func freePort(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	return strings.TrimPrefix(srv.URL, "http://")
}

type fluidsAnswer struct {
	Rows []struct {
		Org        string  `json:"org"`
		KWh        float64 `json:"kwh"`
		KWhActive  float64 `json:"kwh_active"`
		KWhIdle    float64 `json:"kwh_idle"`
		FluidsCost float64 `json:"fluids_cost"`
		Currency   string  `json:"currency"`
	} `json:"rows"`
}

func (g *gw) fluids(t *testing.T, tok string) fluidsAnswer {
	t.Helper()
	day := time.Now().UTC()
	q := "/admin/fluids?group_by=org&from=" + day.AddDate(0, 0, -1).Format("2006-01-02") + "&to=" + day.AddDate(0, 0, 2).Format("2006-01-02")
	code, b := g.call(t, "GET", q, tok, "")
	if code != 200 {
		t.Fatalf("fluids: %d %s", code, b)
	}
	var a fluidsAnswer
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func total(a fluidsAnswer) (kwh, cost float64) {
	for _, r := range a.Rows {
		kwh += r.KWh
		cost += r.FluidsCost
	}
	return
}

func TestMeasuredEnergyThroughTheGateway(t *testing.T) {
	vllm := httptest.NewServer(fakeupstream.NewServer(fakeupstream.Options{Model: "fake", Reply: "one two three four five", Generate: 40}, nil))
	defer vllm.Close()
	dcgm := fakeupstream.NewDCGM("GPU-aaa", "GPU-bbb")
	exp := httptest.NewServer(dcgm)
	defer exp.Close()
	g := startGateway(t, vllm.URL, exp.URL)

	// The gateway's first scrape only sets the baseline: energy counts from here.
	for deadline := time.Now().Add(20 * time.Second); dcgm.Hits() < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the gateway never scraped the exporter")
		}
		time.Sleep(100 * time.Millisecond)
	}

	chat := func(team string, n int) {
		for i := 0; i < n; i++ {
			code, b := g.call(t, "POST", "/v1/chat/completions", g.keys[team], `{"model":"chat","max_tokens":40,"messages":[{"role":"user","content":"hello there general"}]}`)
			if code != 200 {
				t.Fatalf("chat %s: %d %s", team, code, b)
			}
		}
	}
	// Work and energy together: orgA 6 requests, orgB 2, 2 kWh moving.
	chat("a1", 6)
	dcgm.Add("GPU-aaa", kWhMJ)
	chat("b1", 2)
	dcgm.Add("GPU-bbb", kWhMJ)
	time.Sleep(1500 * time.Millisecond)
	// The exporter goes down for several scrapes while a GPU works: nothing is lost.
	dcgm.SetDown(true)
	dcgm.Add("GPU-aaa", kWhMJ/2)
	time.Sleep(3500 * time.Millisecond)
	dcgm.SetDown(false)
	// A driver reload: GPU-bbb's counter restarts at 0.25 kWh, the energy since the reload.
	dcgm.Reset("GPU-bbb", kWhMJ/4)

	// Expected: 2 + 0.5 kWh added, plus the 0.25 kWh a reload says was used since it.
	want := 2.75
	var a fluidsAnswer
	var kwh, cost float64
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		a = g.fluids(t, g.master)
		if kwh, cost = total(a); math.Abs(kwh-want) < 1e-6 {
			break
		}
	}
	if math.Abs(kwh-want) > 1e-6 {
		t.Fatalf("the gateway accounts %.9f kWh, the exporter moved %.9f (%+v)", kwh, want, a)
	}
	if math.Abs(cost-want*1.4*0.18) > 1e-6 || len(a.Rows) < 2 {
		t.Fatalf("cost %.6f for %.3f kWh at PUE 1.4 and 0.18 EUR: %+v", cost, want, a)
	}
	for _, r := range a.Rows {
		if r.Currency != "EUR" || r.KWh <= 0 {
			t.Fatalf("row %+v", r)
		}
	}
	// What an organisation's admin reads is its own energy, not the total.
	code, b := g.call(t, "POST", "/admin/users", g.master, `{"name":"alice"}`)
	if code != 201 {
		t.Fatalf("user: %d %s", code, b)
	}
	if code, b = g.call(t, "PUT", "/admin/users/alice/roles", g.master, `{"role":"org_admin","org":"orgA"}`); code != 200 {
		t.Fatalf("role: %d %s", code, b)
	}
	code, b = g.call(t, "POST", "/admin/users/alice/tokens", g.master, "")
	var tk struct{ Token string }
	if code != 201 || json.Unmarshal(b, &tk) != nil {
		t.Fatalf("token: %d %s", code, b)
	}
	own := g.fluids(t, tk.Token)
	if len(own.Rows) != 1 || own.Rows[0].Org != "orgA" {
		t.Fatalf("org admin reads %+v", own)
	}
	if k, _ := total(own); k >= kwh {
		t.Fatalf("org admin sees %.6f of %.6f kWh", k, kwh)
	}
}
