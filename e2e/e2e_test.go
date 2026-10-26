package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// target says how one team reaches vLLM: through the gateway with its
// virtual key, or directly (the control).
type target struct {
	url, key, model string
}

func viaGateway(g *Gateway, team string) target {
	return target{g.URL + "/v1/chat/completions", g.Keys[team], "chat"}
}

func direct(u upstream) target {
	return target{u.URL + "/v1/chat/completions", "", u.Model}
}

// saturation runs the scheduling scenario. With max_num_seqs = 2:
//
//	B1 (short) and B2 (long) start and fill both running slots;
//	B3 arrives and waits; then A1 arrives and waits;
//	B1 finishes, one slot frees: B3 (earlier) or A1 (better priority).
//
// It returns A1 and B3.
func saturation(t *testing.T, a, b target, b3Extra string, b3Hdr map[string]string) (a1, b3, b1 *Stream) {
	t.Helper()
	if maxNumSeqs() != 2 {
		t.Skipf("scenario written for --max-num-seqs 2, %s says %d", envMaxNumSeqs, maxNumSeqs())
	}
	const short, long = 12, 48
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	start := func(name string, tg target, n int, extra string, hdr map[string]string) **Stream {
		var s *Stream
		wg.Add(1)
		go func() {
			defer wg.Done()
			s = ChatStream(ctx, name, tg.url, tg.key, chatBody(tg.model, "Write the numbers from one upward, one per line. "+randomHex(4), n, extra), hdr)
		}()
		return &s
	}
	// A free slot admits a request at once; the pauses only fix the
	// arrival order.
	pb1 := start("B1", b, short, "", nil)
	time.Sleep(1500 * time.Millisecond)
	pb2 := start("B2", b, long, "", nil)
	time.Sleep(1500 * time.Millisecond)
	pb3 := start("B3", b, short, b3Extra, b3Hdr)
	time.Sleep(700 * time.Millisecond)
	pa1 := start("A1", a, short, "", nil)
	wg.Wait()
	for _, s := range []*Stream{*pb1, *pb2, *pb3, *pa1} {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.Name, s.Err)
		}
		t.Logf("%s sent %s first token %s done %s (%d chunks)", s.Name, s.Sent.Format("15:04:05.000"),
			s.FirstToken.Format("15:04:05.000"), s.Done.Format("15:04:05.000"), len(s.Tokens))
	}
	b1 = *pb1
	// Precondition: B3 really waited, otherwise vLLM was not saturated
	// and the order tells nothing.
	if (*pb3).FirstToken.Before(b1.Done.Add(-200 * time.Millisecond)) {
		t.Fatalf("B3 got its first token at %s before B1 finished at %s: vLLM was not saturated (is --max-num-seqs 2?)",
			(*pb3).FirstToken.Format("15:04:05.000"), b1.Done.Format("15:04:05.000"))
	}
	return *pa1, *pb3, b1
}

// TestControlFCFSWithoutPriorities is the control of the priority checks:
// straight to vLLM, every request at priority 0, the earlier B3 is served
// before A1. If this failed, the scenario could not tell the gateway's
// priorities from luck.
func TestControlFCFSWithoutPriorities(t *testing.T) {
	v := vllmChat(t)
	a1, b3, _ := saturation(t, direct(v), direct(v), "", nil)
	if !b3.FirstToken.Before(a1.FirstToken) {
		t.Fatalf("control: at equal priority A1 (%s) was served before the earlier B3 (%s)", a1.FirstToken, b3.FirstToken)
	}
}

// TestVLLMHeaderOverridesBodyPriority checks the vLLM behaviour the gateway
// rests on: X-Vllm-Priority wins over the body's priority, and a lower
// number runs earlier. B3 says priority 100 in the body and -100 in the
// header; if the header wins B3 overtakes nothing worse than A1's 0... so it
// is served before A1.
func TestVLLMHeaderOverridesBodyPriority(t *testing.T) {
	v := vllmChat(t)
	a1, b3, _ := saturation(t, direct(v), direct(v), `,"priority":100`, map[string]string{"X-Vllm-Priority": "-100"})
	if !b3.FirstToken.Before(a1.FirstToken) {
		t.Fatalf("header -100 with body 100: B3 (%s) not before A1 at 0 (%s): the header did not override the body", b3.FirstToken, a1.FirstToken)
	}
	// And the body alone is obeyed when no header is sent.
	a1, b3, _ = saturation(t, direct(v), direct(v), `,"priority":100`, nil)
	if !a1.FirstToken.Before(b3.FirstToken) {
		t.Fatalf("body priority 100 alone: B3 (%s) still before A1 (%s)", b3.FirstToken, a1.FirstToken)
	}
}

// TestWithinShareBeforeBorrowers is check (a): through the gateway, A1
// (within its organisation's reservation, priority 0) gets its first token
// before B3 (borrowing, priority >= 10) although B3 arrived first.
func TestWithinShareBeforeBorrowers(t *testing.T) {
	v := vllmChat(t)
	g := StartGateway(t, v, nil, nil)
	a1, b3, _ := saturation(t, viaGateway(g, "a1"), viaGateway(g, "b1"), "", nil)
	if !a1.FirstToken.Before(b3.FirstToken) {
		t.Fatalf("borrower B3 (%s) served before within-share A1 (%s)", b3.FirstToken, a1.FirstToken)
	}
	t.Logf("A1 first token %v before B3", b3.FirstToken.Sub(a1.FirstToken))
}

// TestClientCannotRaiseItsPriority is check (b): the borrower B3 sends every
// spelling of a priority vLLM would honour (body field, header, vllm_xargs,
// extra_body); through the gateway the order is unchanged. The positive
// control is TestVLLMHeaderOverridesBodyPriority: sent straight to vLLM,
// the same header does reorder.
func TestClientCannotRaiseItsPriority(t *testing.T) {
	v := vllmChat(t)
	g := StartGateway(t, v, nil, nil)
	extra := `,"priority":-100,"vllm_xargs":{"priority":-100},"extra_body":{"priority":-100},"cache_salt":"attacker"`
	hdr := map[string]string{"X-Vllm-Priority": "-100"}
	a1, b3, _ := saturation(t, viaGateway(g, "a1"), viaGateway(g, "b1"), extra, hdr)
	if !a1.FirstToken.Before(b3.FirstToken) {
		t.Fatalf("a client priority reached vLLM: borrower B3 (%s) before A1 (%s)", b3.FirstToken, a1.FirstToken)
	}
}

// kvCapacity returns vLLM's KV cache size in tokens from
// vllm:cache_config_info (num_gpu_blocks x block_size).
func kvCapacity(t *testing.T, v upstream) int {
	t.Helper()
	m := Metrics(t, v.URL+"/metrics")
	for k := range m {
		if !strings.HasPrefix(k, "vllm:cache_config_info{") {
			continue
		}
		blocks, bs := labelInt(k, "num_gpu_blocks"), labelInt(k, "block_size")
		if blocks > 0 && bs > 0 {
			return blocks * bs
		}
	}
	t.Fatal("vllm:cache_config_info has no num_gpu_blocks/block_size")
	return 0
}

func labelInt(series, label string) int {
	i := strings.Index(series, label+`="`)
	if i < 0 {
		return 0
	}
	rest := series[i+len(label)+2:]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return 0
	}
	var n int
	fmt.Sscanf(rest[:j], "%d", &n)
	return n
}

// preemption runs two long requests that each fit the KV cache alone but
// not together: B1 starts first, A1 second. vLLM must preempt one.
func preemption(t *testing.T, v upstream, a, b target) (a1, b1 *Stream, preempted float64) {
	t.Helper()
	if os.Getenv(envKVShortage) != "1" {
		t.Skipf("%s=1 not set: vLLM must run with a tiny KV cache for this check", envKVShortage)
	}
	capTok := kvCapacity(t, v)
	n := capTok * 6 / 10 // each alone fits; together 120 % of the cache
	t.Logf("KV cache %d tokens; each request generates %d", capTok, n)
	before := Metrics(t, v.URL+"/metrics")["vllm:num_preemptions_total"]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b1 = ChatStream(ctx, "B1", b.url, b.key, chatBody(b.model, "Count. "+randomHex(4), n, ""), nil)
	}()
	time.Sleep(2 * time.Second)
	go func() {
		defer wg.Done()
		a1 = ChatStream(ctx, "A1", a.url, a.key, chatBody(a.model, "Count. "+randomHex(4), n, ""), nil)
	}()
	wg.Wait()
	for _, s := range []*Stream{b1, a1} {
		if s.Err != nil {
			t.Fatalf("%s: %v", s.Name, s.Err)
		}
		t.Logf("%s first %s done %s chunks %d max gap %v", s.Name, s.FirstToken.Format("15:04:05.000"), s.Done.Format("15:04:05.000"), len(s.Tokens), s.MaxGap())
	}
	preempted = Metrics(t, v.URL+"/metrics")["vllm:num_preemptions_total"] - before
	if preempted < 1 {
		t.Fatalf("no preemption happened (KV %d tokens, 2 x %d generated): the scenario did not create a shortage", capTok, n)
	}
	t.Logf("vllm:num_preemptions_total +%v", preempted)
	return
}

// TestControlPreemptsLatestArrival: straight to vLLM at equal priority,
// the victim is the later arrival (A1), so B1 finishes first.
func TestControlPreemptsLatestArrival(t *testing.T) {
	v := vllmChat(t)
	a1, b1, n := preemption(t, v, direct(v), direct(v))
	if !b1.Done.Before(a1.Done) {
		t.Fatalf("control: equal priorities, %v preemptions, but the earlier B1 finished after A1", n)
	}
}

// TestBorrowerPreemptedFirst is the second half of check (a): through the
// gateway, under a KV shortage, the borrower B1 is preempted although it
// started first, and the within-share A1 runs to the end.
func TestBorrowerPreemptedFirst(t *testing.T) {
	v := vllmChat(t)
	g := StartGateway(t, v, nil, nil)
	a1, b1, n := preemption(t, v, viaGateway(g, "a1"), viaGateway(g, "b1"))
	if !a1.Done.Before(b1.Done) {
		t.Fatalf("%v preemptions, but the within-share A1 finished after the borrower B1", n)
	}
	if a1.MaxGap() > b1.MaxGap() {
		t.Fatalf("A1 stalled longer (%v) than the borrower B1 (%v)", a1.MaxGap(), b1.MaxGap())
	}
}

// prefixHits sends one request and returns the prefix-cache hits it caused
// (tokens), read from vLLM's own counters, not from timing.
func prefixHits(t *testing.T, v upstream, tg target, prompt, extra string) (hits, queries float64) {
	t.Helper()
	m0 := Metrics(t, v.URL+"/metrics")
	p, _ := json.Marshal(prompt)
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%s}],"max_tokens":1,"temperature":0%s}`, tg.model, p, extra)
	var out struct {
		Usage struct {
			Prompt int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if code, err := postJSON(tg.url, tg.key, body, nil, &out); err != nil {
		t.Fatalf("HTTP %d: %v", code, err)
	}
	// The counters are updated by the engine's stat loggers: wait until
	// the queries of this request are counted.
	deadline := time.Now().Add(10 * time.Second)
	for {
		m1 := Metrics(t, v.URL+"/metrics")
		hits = m1["vllm:prefix_cache_hits_total"] - m0["vllm:prefix_cache_hits_total"]
		queries = m1["vllm:prefix_cache_queries_total"] - m0["vllm:prefix_cache_queries_total"]
		if queries >= float64(out.Usage.Prompt) || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if queries < 1 {
		t.Fatalf("vllm:prefix_cache_queries_total did not move: is prefix caching on?")
	}
	return hits, queries
}

func longPrompt() string {
	// A fresh random head makes the whole prompt new to the cache; the
	// body is long enough to span many KV blocks.
	var b strings.Builder
	b.WriteString("Session " + randomHex(16) + ". ")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "Paragraph %d: the quick brown fox jumps over the lazy dog near the river bank. ", i)
	}
	return b.String()
}

// TestPrefixCacheIsolation is checks (c) and the cache half of (b).
func TestPrefixCacheIsolation(t *testing.T) {
	v := vllmChat(t)
	g := StartGateway(t, v, nil, nil)

	// Control: the instrument sees a hit. Same prompt twice, straight to
	// vLLM, no salt: the second request hits.
	p0 := longPrompt()
	prefixHits(t, v, direct(v), p0, "")
	h, q := prefixHits(t, v, direct(v), p0, "")
	t.Logf("control, direct, no salt, 2nd request: %v hits / %v queries", h, q)
	if h < q/2 {
		t.Fatalf("control: an identical unsalted prompt hit only %v of %v tokens: the metric cannot see a hit", h, q)
	}

	// The gateway always salts: a1 does not reach the unsalted entries.
	h, q = prefixHits(t, v, viaGateway(g, "a1"), p0, "")
	t.Logf("a1 via gateway on the prompt cached unsalted: %v / %v", h, q)
	if h != 0 {
		t.Errorf("gateway request hit %v tokens cached without salt: no salt applied", h)
	}

	// Within an organisation the cache is shared (cache_isolation = org).
	p1 := longPrompt()
	prefixHits(t, v, viaGateway(g, "a1"), p1, "")
	h, q = prefixHits(t, v, viaGateway(g, "a2"), p1, "")
	t.Logf("a2 after a1 (same org): %v / %v", h, q)
	if h < q/2 {
		t.Errorf("same organisation: a2 hit only %v of %v tokens a1 had cached", h, q)
	}

	// Across organisations it is not.
	h, q = prefixHits(t, v, viaGateway(g, "b1"), p1, "")
	t.Logf("b1 after a1 (other org): %v / %v", h, q)
	if h != 0 {
		t.Errorf("other organisation: b1 hit %v tokens of org A's cache", h)
	}

	// (b) a client cache_salt changes nothing: a1 and b1 both send the
	// same client salt; if the gateway forwarded it they would share.
	p2 := longPrompt()
	prefixHits(t, v, viaGateway(g, "a1"), p2, `,"cache_salt":"shared-by-clients"`)
	h, q = prefixHits(t, v, viaGateway(g, "b1"), p2, `,"cache_salt":"shared-by-clients"`)
	t.Logf("b1 with the same client salt as a1: %v / %v", h, q)
	if h != 0 {
		t.Errorf("client cache_salt let b1 hit %v tokens of a1's cache", h)
	}
	// and a client salt does not move a1 out of its own scope either
	h, q = prefixHits(t, v, viaGateway(g, "a2"), p2, `,"cache_salt":"something-else"`)
	t.Logf("a2 with another client salt after a1: %v / %v", h, q)
	if h < q/2 {
		t.Errorf("a client salt moved a2 out of its organisation's scope: %v of %v", h, q)
	}
}

// TestResponsesStatelessThroughGateway is the Responses part of check (d).
// vLLM must run with VLLM_ENABLE_RESPONSES_API_STORE=1 for the control
// (without it vLLM stores nothing whatever the request says).
func TestResponsesStatelessThroughGateway(t *testing.T) {
	v := vllmChat(t)
	g := StartGateway(t, v, nil, nil)
	var r struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Store  *bool  `json:"store"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage map[string]any `json:"usage"`
	}
	body := `{"model":"chat","input":"Say hello.","max_output_tokens":8,"store":true}`
	if code, err := postJSON(g.URL+"/v1/responses", g.Keys["a1"], body, nil, &r); err != nil {
		t.Fatalf("gateway /v1/responses: HTTP %d %v", code, err)
	}
	text := ""
	for _, o := range r.Output {
		for _, c := range o.Content {
			text += c.Text
		}
	}
	t.Logf("gateway response id=%s status=%s store=%v text=%q usage=%v", r.ID, r.Status, deref(r.Store), text, r.Usage)
	if r.ID == "" || text == "" {
		t.Fatalf("no id or no output text")
	}
	if r.Store != nil && *r.Store {
		t.Errorf("response says store=true through the gateway")
	}
	var any map[string]any
	if err := getJSON(v.URL+"/v1/responses/"+r.ID, "", &any); err == nil {
		t.Errorf("vLLM kept the gateway's response %s: store was not forced false", r.ID)
	}

	// Control: straight to vLLM with store=true, the response can be
	// read back, so the probe above can see a stored response.
	r.ID = ""
	if code, err := postJSON(v.URL+"/v1/responses", "", strings.Replace(body, `"chat"`, fmt.Sprintf("%q", v.Model), 1), nil, &r); err != nil {
		t.Fatalf("direct /v1/responses: HTTP %d %v", code, err)
	}
	if err := getJSON(v.URL+"/v1/responses/"+r.ID, "", &any); err != nil {
		t.Skipf("control: vLLM did not store a store=true response (%v); start it with VLLM_ENABLE_RESPONSES_API_STORE=1 to make this check meaningful", err)
	}
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// TestMessagesThroughGateway: /v1/messages, JSON and SSE.
func TestMessagesThroughGateway(t *testing.T) {
	v := vllmChat(t)
	g := StartGateway(t, v, nil, nil)
	var m struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type, Text string
		} `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      map[string]any `json:"usage"`
	}
	body := `{"model":"chat","max_tokens":8,"messages":[{"role":"user","content":"Say hello."}]}`
	if code, err := postJSON(g.URL+"/v1/messages", g.Keys["a1"], body, nil, &m); err != nil {
		t.Fatalf("HTTP %d %v", code, err)
	}
	t.Logf("messages JSON: type=%s role=%s stop=%s usage=%v content=%+v", m.Type, m.Role, m.StopReason, m.Usage, m.Content)
	if m.Type != "message" || m.Role != "assistant" || len(m.Content) == 0 || m.Content[0].Text == "" {
		t.Fatalf("not an Anthropic message: %+v", m)
	}
	events := streamEvents(t, g.URL+"/v1/messages", g.Keys["a1"], strings.Replace(body, `"max_tokens":8`, `"max_tokens":8,"stream":true`, 1))
	t.Logf("messages SSE events: %v", events)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if !subsequence(events, want) {
		t.Fatalf("events %v do not contain %v in order", events, want)
	}
}

func streamEvents(t *testing.T, url, key, body string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	var evs []string
	buf := new(strings.Builder)
	b := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "event: ") {
			e := strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			if len(evs) == 0 || evs[len(evs)-1] != e {
				evs = append(evs, e)
			}
		}
	}
	return evs
}

func subsequence(have, want []string) bool {
	i := 0
	for _, h := range have {
		if i < len(want) && h == want[i] {
			i++
		}
	}
	return i == len(want)
}

// TestTokenizeThroughGateway: /tokenize through the gateway counts the same
// tokens as vLLM itself.
func TestTokenizeThroughGateway(t *testing.T) {
	v := vllmChat(t)
	g := StartGateway(t, v, nil, nil)
	type tok struct {
		Count  int   `json:"count"`
		Tokens []int `json:"tokens"`
	}
	var viaGW, viaV tok
	if code, err := postJSON(g.URL+"/tokenize", g.Keys["a1"], `{"model":"chat","prompt":"Hello tokenizer, count me."}`, nil, &viaGW); err != nil {
		t.Fatalf("gateway: HTTP %d %v", code, err)
	}
	if code, err := postJSON(v.URL+"/tokenize", "", fmt.Sprintf(`{"model":%q,"prompt":"Hello tokenizer, count me."}`, v.Model), nil, &viaV); err != nil {
		t.Fatalf("direct: HTTP %d %v", code, err)
	}
	t.Logf("tokenize: gateway count=%d, vLLM count=%d", viaGW.Count, viaV.Count)
	if viaGW.Count == 0 || viaGW.Count != viaV.Count {
		t.Fatalf("gateway count %d, vLLM %d", viaGW.Count, viaV.Count)
	}
}

// TestEmbeddingsThroughGateway needs BENCH_VLLM_EMBED_URL.
func TestEmbeddingsThroughGateway(t *testing.T) {
	v := vllmChat(t)
	e := optional(t, envEmbedURL)
	g := StartGateway(t, v, []upstream{e}, []string{"embed"})
	var r struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage map[string]any `json:"usage"`
	}
	if code, err := postJSON(g.URL+"/v1/embeddings", g.Keys["a1"], `{"model":"embed","input":["hello world","bonjour"]}`, nil, &r); err != nil {
		t.Fatalf("HTTP %d %v", code, err)
	}
	t.Logf("embeddings: %d vectors of %d, usage %v", len(r.Data), dim(r.Data), r.Usage)
	if len(r.Data) != 2 || dim(r.Data) == 0 {
		t.Fatal("no embeddings")
	}
}

func dim(d []struct {
	Embedding []float64 `json:"embedding"`
}) int {
	if len(d) == 0 {
		return 0
	}
	return len(d[0].Embedding)
}

// TestRerankThroughGateway needs BENCH_VLLM_RERANK_URL.
func TestRerankThroughGateway(t *testing.T) {
	v := vllmChat(t)
	rr := optional(t, envRerankURL)
	g := StartGateway(t, v, []upstream{rr}, []string{"rerank"})
	var r struct {
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		} `json:"results"`
		Usage map[string]any `json:"usage"`
	}
	body := `{"model":"rerank","query":"What is the capital of France?","documents":["Paris is the capital of France.","Bananas are yellow."]}`
	for _, path := range []string{"/v1/rerank", "/v2/rerank"} {
		if code, err := postJSON(g.URL+path, g.Keys["a1"], body, nil, &r); err != nil {
			t.Fatalf("%s: HTTP %d %v", path, code, err)
		}
		t.Logf("%s: %+v usage %v", path, r.Results, r.Usage)
		if len(r.Results) != 2 || r.Results[0].Index != 0 {
			t.Fatalf("%s: Paris not ranked first: %+v", path, r.Results)
		}
	}
}

// TestAccountingMatchesVLLM is check (e): the tokens the gateway charges
// equal what vLLM itself counted, over chat (JSON and stream), Messages and
// Responses. Nothing else may talk to vLLM meanwhile.
func TestAccountingMatchesVLLM(t *testing.T) {
	v := vllmChat(t)
	g := StartGateway(t, v, nil, nil)
	key := g.Keys["a2"]
	v0, g0 := Metrics(t, v.URL+"/metrics"), Metrics(t, g.URL+"/metrics")
	var client struct{ prompt, completion float64 }
	// chat, JSON
	var c struct {
		Usage struct {
			P float64 `json:"prompt_tokens"`
			C float64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if _, err := postJSON(g.URL+"/v1/chat/completions", key, `{"model":"chat","messages":[{"role":"user","content":"Name three colours."}],"max_tokens":12}`, nil, &c); err != nil {
		t.Fatal(err)
	}
	client.prompt += c.Usage.P
	client.completion += c.Usage.C
	// chat, stream, the client does not ask for usage: the gateway must
	// still count it.
	s := ChatStream(context.Background(), "stream", g.URL+"/v1/chat/completions", key,
		`{"model":"chat","messages":[{"role":"user","content":"Name three animals."}],"max_tokens":12,"stream":true}`, nil)
	if s.Err != nil {
		t.Fatal(s.Err)
	}
	if s.Usage != nil {
		t.Errorf("usage chunk delivered to a client that did not ask for it: %v", s.Usage)
	}
	// Messages
	var m struct {
		Usage struct {
			In  float64 `json:"input_tokens"`
			Out float64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if _, err := postJSON(g.URL+"/v1/messages", key, `{"model":"chat","max_tokens":10,"messages":[{"role":"user","content":"Name a fruit."}]}`, nil, &m); err != nil {
		t.Fatal(err)
	}
	client.prompt += m.Usage.In
	client.completion += m.Usage.Out
	// Responses
	var r struct {
		Usage struct {
			In  float64 `json:"input_tokens"`
			Out float64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if _, err := postJSON(g.URL+"/v1/responses", key, `{"model":"chat","input":"Name a city.","max_output_tokens":10}`, nil, &r); err != nil {
		t.Fatal(err)
	}
	client.prompt += r.Usage.In
	client.completion += r.Usage.Out
	time.Sleep(2 * time.Second) // vLLM's stat loggers
	v1, g1 := Metrics(t, v.URL+"/metrics"), Metrics(t, g.URL+"/metrics")
	vp := v1["vllm:prompt_tokens_total"] - v0["vllm:prompt_tokens_total"]
	vc := v1["vllm:generation_tokens_total"] - v0["vllm:generation_tokens_total"]
	gp := sumMatching(g1, "llmflow_tokens_total", `team="a2"`, `kind="prompt"`) - sumMatching(g0, "llmflow_tokens_total", `team="a2"`, `kind="prompt"`)
	gc := sumMatching(g1, "llmflow_tokens_total", `team="a2"`, `kind="completion"`) - sumMatching(g0, "llmflow_tokens_total", `team="a2"`, `kind="completion"`)
	t.Logf("vLLM counted prompt=%v completion=%v; gateway charged prompt=%v completion=%v; non-streamed answers told the client prompt=%v completion=%v (+ the stream)",
		vp, vc, gp, gc, client.prompt, client.completion)
	if math.Abs(vp-gp) > 0 || math.Abs(vc-gc) > 0 {
		t.Errorf("gateway charged prompt %v / completion %v, vLLM counted %v / %v", gp, gc, vp, vc)
	}
}

// TestVLLMQueuesInsteadOfRefusing records how vLLM answers more concurrent
// requests than --max-num-seqs: it queues them (all 200), it does not
// answer 429 or 503. The gateway's fallback on 429/503 is therefore only
// reachable when vLLM is down or overloaded at the HTTP layer.
func TestVLLMQueuesInsteadOfRefusing(t *testing.T) {
	v := vllmChat(t)
	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = postJSON(v.URL+"/v1/chat/completions", "", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":2}`, v.Model), nil, nil)
		}(i)
	}
	wg.Wait()
	t.Logf("%d concurrent requests, max-num-seqs %d: statuses %v", n, maxNumSeqs(), codes)
	for _, c := range codes {
		if c != 200 {
			t.Errorf("vLLM answered %d to a request beyond --max-num-seqs", c)
		}
	}
}
