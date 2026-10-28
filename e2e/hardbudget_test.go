package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Hard budgets (gateway #87, v0.17.0): against a real vLLM, every request
// admitted under a budget uses at most the worst case W the gateway held
// for it, W = prompts · max((L−M)·p_in + n·M·p_out, L·p_in), with L vLLM's
// max_model_len (its GET /v1/models), M the max_tokens the gateway sent
// (the client's, or the one it picked and said in X-Llmflow-Max-Tokens) and
// n the choices. Requests without max_tokens, with n > 1, with a prompt list,
// streamed, and with a prompt near L.
//
// The usage is vLLM's own (its usage object). The bound is checked both in
// tokens (prompt ≤ prompts·L, completion ≤ prompts·n·M) and in cost.

const (
	hbIn, hbOut, hbCached = 1000.0, 5000.0, 100.0 // per million tokens
)

func maxModelLen(t *testing.T, v upstream) int64 {
	t.Helper()
	var m struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int64  `json:"max_model_len"`
		} `json:"data"`
	}
	if err := getJSON(v.URL+"/v1/models", "", &m); err != nil {
		t.Fatal(err)
	}
	for _, d := range m.Data {
		if d.ID == v.Model && d.MaxModelLen > 0 {
			return d.MaxModelLen
		}
	}
	t.Fatalf("no max_model_len for %s in /v1/models", v.Model)
	return 0
}

// worst is W in units.
func worst(l, m, n, prompts int64) float64 {
	m = min(m, l)
	pin := max(hbIn, hbCached)
	per := max(float64(l-m)*pin+float64(n*m)*hbOut, float64(l)*pin)
	return float64(prompts) * per / 1e6
}

type hbUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u hbUsage) cost() float64 {
	cached := int64(0)
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return (float64(u.PromptTokens-cached)*hbIn + float64(cached)*hbCached + float64(u.CompletionTokens)*hbOut) / 1e6
}

// hbPost sends one request and returns vLLM's usage (from the JSON answer or
// the stream's usage chunk) and the response headers.
func hbPost(t *testing.T, url, key, body string) (int, hbUsage, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var u hbUsage
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok || line == "[DONE]" {
				continue
			}
			var ev struct {
				Usage *hbUsage `json:"usage"`
			}
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Usage != nil {
				u = *ev.Usage
			}
		}
		return resp.StatusCode, u, resp.Header, ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var env struct {
		Usage *hbUsage `json:"usage"`
	}
	if json.Unmarshal(b, &env) == nil && env.Usage != nil {
		u = *env.Usage
	}
	return resp.StatusCode, u, resp.Header, string(b)
}

// nearL builds a chat prompt of about l - margin tokens, measured with
// vLLM's own /tokenize (the chat template included).
func nearL(t *testing.T, v upstream, l, margin int64) string {
	t.Helper()
	count := func(words int) int64 {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, v.Model, strings.Repeat("hello ", words))
		var r struct {
			Count int64 `json:"count"`
		}
		if _, err := postJSON(v.URL+"/tokenize", "", body, nil, &r); err != nil {
			t.Fatal(err)
		}
		return r.Count
	}
	lo, hi := 1, int(l)
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if count(mid) <= l-margin {
			lo = mid
		} else {
			hi = mid
		}
	}
	return strings.Repeat("hello ", lo)
}

func TestHardBudgetUsageWithinW(t *testing.T) {
	v := vllmChat(t)
	l := maxModelLen(t, v)
	g := StartGatewayWith(t, v, nil, nil, fmt.Sprintf("  cost {\n    input_per_mtok = %g\n    output_per_mtok = %g\n    cached_per_mtok = %g\n  }\n", hbIn, hbOut, hbCached), "")
	put := func(body string) {
		req, _ := http.NewRequest(http.MethodPut, g.URL+"/admin/budgets", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+g.Master)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("budget %s: %d %s", body, resp.StatusCode, b)
		}
	}
	put(`{"kind":"team","name":"a1","period":"day","max_cost":100000}`)
	long := nearL(t, v, l, 24)
	type tc struct {
		name, path, body string
		n, prompts, m    int64 // m: the client's max_tokens, 0 for none
	}
	cases := []tc{
		{"chat without max_tokens", "/v1/chat/completions", fmt.Sprintf(`{"model":"chat","messages":[{"role":"user","content":"Count from one to fifty."}]}`), 1, 1, 0},
		{"chat n=3", "/v1/chat/completions", `{"model":"chat","n":3,"max_tokens":16,"messages":[{"role":"user","content":"Write a long story."}]}`, 3, 1, 16},
		{"chat stream", "/v1/chat/completions", `{"model":"chat","stream":true,"stream_options":{"include_usage":true},"max_tokens":32,"messages":[{"role":"user","content":"Write a long story."}]}`, 1, 1, 32},
		{"prompt near L", "/v1/chat/completions", fmt.Sprintf(`{"model":"chat","max_tokens":64,"messages":[{"role":"user","content":%q}]}`, long), 1, 1, 64},
		{"prompt near L without max_tokens", "/v1/chat/completions", fmt.Sprintf(`{"model":"chat","messages":[{"role":"user","content":%q}]}`, long), 1, 1, 0},
		{"completions, two prompts, n=2", "/v1/completions", `{"model":"chat","prompt":["Once upon a time","The capital of France is"],"n":2,"max_tokens":8}`, 2, 2, 8},
	}
	var total float64
	for _, c := range cases {
		code, u, hdr, body := hbPost(t, g.URL+c.path, g.Keys["a1"], c.body)
		if code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		m := c.m
		if m == 0 {
			if _, err := fmt.Sscan(hdr.Get("X-Llmflow-Max-Tokens"), &m); err != nil || m <= 0 {
				t.Fatalf("%s: no X-Llmflow-Max-Tokens (%q)", c.name, hdr.Get("X-Llmflow-Max-Tokens"))
			}
		}
		if hdr.Get("X-Llmflow-Budget-Reset") == "" {
			t.Errorf("%s: no X-Llmflow-Budget-Reset", c.name)
		}
		w := worst(l, m, c.n, c.prompts)
		t.Logf("%s: M %d, usage %d prompt / %d completion, cost %.6f ≤ W %.6f", c.name, m, u.PromptTokens, u.CompletionTokens, u.cost(), w)
		if u.PromptTokens == 0 || u.PromptTokens > c.prompts*l || u.CompletionTokens > c.prompts*c.n*min(m, l) || u.cost() > w+1e-9 {
			t.Errorf("%s: usage %+v beyond the bound (L %d, M %d, n %d): cost %.6f > W %.6f", c.name, u, l, m, c.n, u.cost(), w)
		}
		total += u.cost()
	}
	// The ledger: what was settled, and nothing held once all is finished
	// (beyond unused grants), never above the limit.
	time.Sleep(2 * time.Second) // the accounting writer's tick
	var bs []struct {
		Kind, Name      string
		Committed, Held float64
		Limit           float64
	}
	if err := getJSON(g.URL+"/admin/budgets", g.Master, &bs); err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Name == "a1" && (b.Committed+b.Held > b.Limit || b.Committed < total*0.999) {
			t.Errorf("ledger %+v, requests cost %.6f", b, total)
		}
	}
	// A budget below the smallest worst case refuses before vLLM sees it.
	minW := worst(l, 16, 1, 1)
	put(fmt.Sprintf(`{"kind":"team","name":"a2","period":"day","max_cost":%g}`, minW*0.99))
	before := sumMatching(Metrics(t, v.URL+"/metrics"), "vllm:request_success_total")
	code, _, hdr, body := hbPost(t, g.URL+"/v1/chat/completions", g.Keys["a2"], `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`)
	if code != 429 || !strings.Contains(body, "insufficient_quota") || hdr.Get("X-Llmflow-Budget-Reset") == "" {
		t.Fatalf("below the worst case: %d %q %s", code, hdr.Get("X-Llmflow-Budget-Reset"), body)
	}
	if after := sumMatching(Metrics(t, v.URL+"/metrics"), "vllm:request_success_total"); after != before {
		t.Fatalf("a refused request reached vLLM: %v → %v", before, after)
	}
}
