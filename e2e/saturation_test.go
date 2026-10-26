package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postModel sends a non-streamed chat and returns the status and the model
// group that served it (X-Llmflow-Model; empty: the one asked for).
func postModel(t *testing.T, url, bearer, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Llmflow-Model")
}

// TestFallbackOnSaturation (gateway v0.11.0, go-llmflow/gateway#58): vLLM
// never refuses under load, so the gateway decides saturation from its own
// pool. "chat" (pool cpu, C = 2) falls back to "chat-spare" (pool spare,
// C = 2, the same vLLM) on "saturated". Two long borrowing streams of b1
// fill pool cpu; then a borrower of b1 must be served by chat-spare, and a
// within-share request of a1 (orgA reserves 1 slot of cpu) must stay on
// chat. vLLM needs --max-num-seqs >= 4 so that the streams of both pools run.
func TestFallbackOnSaturation(t *testing.T) {
	v := vllmChat(t)
	if maxNumSeqs() < 4 {
		t.Skipf("needs vLLM --max-num-seqs >= 4 and %s set to it (both pools run on one server)", envMaxNumSeqs)
	}
	g := StartGatewayWith(t, v, nil, nil,
		`  fallbacks   = ["chat-spare"]
  fallback_on = ["unavailable", "saturated"]
`, fmt.Sprintf(`pool "spare" { capacity = 2 }
model "chat-spare" {
  upstream_model = %q
  pool           = "spare"
  class          = "interactive"
  key_file       = "UPSTREAM_KEY_FILE"
  upstream "v0" { base_url = %q }
}
`, v.Model, v.URL))
	url := g.URL + "/v1/chat/completions"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	long := chatBody("chat", "Count slowly from one to two hundred, one number per line.", 400, "")
	streams := make(chan *Stream, 2)
	for i := 0; i < 2; i++ {
		go func(i int) { streams <- ChatStream(ctx, fmt.Sprintf("B%d", i+1), url, g.Keys["b1"], long, nil) }(i)
	}
	// Both are in flight at vLLM through group chat once the gateway's
	// upstream gauge says 2.
	deadline := time.Now().Add(60 * time.Second)
	for sumMatching(Metrics(t, g.URL+"/metrics"), "llmflow_upstream_inflight", `group="chat"`) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the two borrowing streams never were both in flight")
		}
		time.Sleep(200 * time.Millisecond)
	}

	short := chatBody("chat", "Say hi.", 4, "")
	code, model := postModel(t, url, g.Keys["b1"], short)
	t.Logf("borrower b1 while pool cpu is full: HTTP %d, served by %q", code, model)
	if code != 200 || model != "chat-spare" {
		t.Errorf("the borrower was not moved to chat-spare: %d %q", code, model)
	}
	code, model = postModel(t, url, g.Keys["a1"], short)
	t.Logf("within-share a1 while pool cpu is full: HTTP %d, served by %q", code, model)
	if code != 200 || model == "chat-spare" {
		t.Errorf("a within-share request left its reservation: %d %q", code, model)
	}
	m := Metrics(t, g.URL+"/metrics")
	if n := sumMatching(m, "llmflow_fallbacks_total", `reason="saturated"`); n != 1 {
		t.Errorf("llmflow_fallbacks_total{reason=saturated} = %v, want 1", n)
	}
	cancel()
	for i := 0; i < 2; i++ {
		<-streams
	}
}
