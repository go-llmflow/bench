package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	ds := []time.Duration{5, 1, 4, 2, 3, 10, 9, 8, 7, 6}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{50, 5}, {90, 9}, {99, 10}, {0, 1}, {100, 10}} {
		if got := Percentile(ds, tc.p); got != tc.want {
			t.Errorf("p%v = %v, want %v", tc.p, got, tc.want)
		}
	}
	if Percentile(nil, 50) != 0 {
		t.Error("empty slice")
	}
}

func TestOverheadPairsRounds(t *testing.T) {
	gw := Result{Name: "gw", Latencies: []time.Duration{12, 15, 30}}
	d := Result{Name: "d", Latencies: []time.Duration{10, 10, 25}}
	ov, err := Overhead(gw, d)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{2, 5, 5}
	for i := range want {
		if ov[i] != want[i] {
			t.Fatalf("overhead %v, want %v", ov, want)
		}
	}
	if _, err := Overhead(gw, Result{Latencies: []time.Duration{1}}); err == nil {
		t.Fatal("mismatched lengths accepted")
	}
}

// TestPlantedOverheadIsMeasured is the control: a "gateway" that adds a
// known 3 ms before forwarding must come out at about 3 ms of overhead,
// and a transparent one at about 0. If the instrument could not see a
// planted delay, its verdict on real gateways would mean nothing.
func TestPlantedOverheadIsMeasured(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer up.Close()
	proxy := func(delay time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			resp, err := http.Post(up.URL, "application/json", r.Body)
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			defer resp.Body.Close()
			io.Copy(w, resp.Body)
		}))
	}
	slow, fast := proxy(3*time.Millisecond), proxy(0)
	defer slow.Close()
	defer fast.Close()
	res, err := Run(context.Background(), http.DefaultClient, []Target{
		{Name: "direct", URL: up.URL}, {Name: "slow", URL: slow.URL}, {Name: "fast", URL: fast.URL},
	}, 200, 20, 7)
	if err != nil {
		t.Fatal(err)
	}
	ovSlow, _ := Overhead(res[1], res[0])
	ovFast, _ := Overhead(res[2], res[0])
	ps, pf := Percentile(ovSlow, 50), Percentile(ovFast, 50)
	if ps < 3*time.Millisecond || ps > 6*time.Millisecond {
		t.Errorf("planted 3 ms measured as %v", ps)
	}
	if pf > 2*time.Millisecond {
		t.Errorf("transparent proxy measured at %v", pf)
	}
	if ps-pf < 2500*time.Microsecond {
		t.Errorf("instrument cannot separate a 3 ms difference: slow %v fast %v", ps, pf)
	}
}

func TestPromptIsSentAsJSONString(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p := "line \"quoted\"\nIBAN FR76 3000 6000 0112 3456 7890 189"
	if _, err := once(context.Background(), http.DefaultClient, Target{URL: srv.URL, Model: "m", Prompt: p}); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil || len(req.Messages) != 1 || req.Messages[0].Content != p {
		t.Fatalf("body %s: %v", got, err)
	}
}

func TestRunReportsHTTPErrors(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer bad.Close()
	if _, err := Run(context.Background(), http.DefaultClient, []Target{{Name: "bad", URL: bad.URL}}, 1, 0, 1); err == nil {
		t.Fatal("a 401 must abort the run, not be timed")
	}
}

func TestMergeKeepsRoundsPaired(t *testing.T) {
	r1 := []Result{{Name: "direct", Latencies: []time.Duration{10, 20}}, {Name: "gw", Latencies: []time.Duration{15, 26}}}
	r2 := []Result{{Name: "direct", Latencies: []time.Duration{30}}, {Name: "gw", Latencies: []time.Duration{37}}}
	all := Merge([][]Result{r1, r2})
	ov, err := Overhead(all[1], all[0])
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5, 6, 7}
	for i := range want {
		if ov[i] != want[i] {
			t.Fatalf("merged overhead %v, want %v", ov, want)
		}
	}
	if Merge(nil) != nil {
		t.Fatal("merge of nothing")
	}
}

func TestReportWithoutRunsSaysNoMeasurement(t *testing.T) {
	var b bytes.Buffer
	report(&b, nil, 10, 1)
	if !bytes.Contains(b.Bytes(), []byte("no measurement")) {
		t.Fatalf("report without runs: %q", b.String())
	}
}

func TestParseTarget(t *testing.T) {
	dir := t.TempDir()
	kf := dir + "/k"
	if err := os.WriteFile(kf, []byte("sekret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tg, err := ParseTarget("v6|http://h/v1/chat/completions|"+kf, "m")
	if err != nil || tg.Name != "v6" || tg.Key != "sekret" || tg.Model != "m" {
		t.Fatalf("%+v %v", tg, err)
	}
	if tg, err := ParseTarget("open|http://h/x|", "m"); err != nil || tg.Key != "" {
		t.Fatalf("no key file: %+v %v", tg, err)
	}
	for _, bad := range []string{"", "a|b", "|http://h|", "direct|http://h|", "a|http://h|/nonexistent/key"} {
		if _, err := ParseTarget(bad, "m"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// The load average reading and the wait live in, and are tested by,
// github.com/go-fleettools/fleettools/loadavg.

func TestRunCmdRefusesUnderLoad(t *testing.T) {
	cmd := RootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"run", "--max-load", "-1", "--direct", "http://127.0.0.1:1/"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("ran despite load above --max-load")
	}
}
