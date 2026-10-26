package fakeupstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The Anthropic API's own behaviour: prompt-cache usage, workspace and
// rate-limit headers, 429/529 with retry-after.
func TestAnthropicCacheUsageAndHeaders(t *testing.T) {
	srv, _ := newTest(t, Options{Reply: "a b", CacheRead: 100, CacheWrite5m: 30, CacheWrite1h: 20, Workspace: "wrkspc_x", RateLimitHeaders: true})
	resp := post(t, srv.URL+"/v1/messages", `{"messages":[{"role":"user","content":"one two three"}]}`, nil)
	var m struct {
		Usage struct {
			In            int `json:"input_tokens"`
			Out           int `json:"output_tokens"`
			Read          int `json:"cache_read_input_tokens"`
			Creation      int `json:"cache_creation_input_tokens"`
			CacheCreation struct {
				M5 int `json:"ephemeral_5m_input_tokens"`
				H1 int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	u := m.Usage
	if u.In != 3 || u.Out != 2 || u.Read != 100 || u.Creation != 50 || u.CacheCreation.M5 != 30 || u.CacheCreation.H1 != 20 {
		t.Fatalf("usage %+v", u)
	}
	if resp.Header.Get("Anthropic-Workspace-Id") != "wrkspc_x" || resp.Header.Get("Anthropic-Ratelimit-Tokens-Remaining") == "" || resp.Header.Get("Request-Id") == "" {
		t.Fatalf("headers %v", resp.Header)
	}
	// The stream's message_start carries the same input usage.
	resp = post(t, srv.URL+"/v1/messages", `{"stream":true,"messages":[{"role":"user","content":"x"}]}`, nil)
	_, datas := readSSE(t, resp.Body)
	if !strings.Contains(datas[0], `"cache_read_input_tokens":100`) || !strings.Contains(datas[0], `"ephemeral_1h_input_tokens":20`) {
		t.Fatalf("message_start %s", datas[0])
	}
}

func TestAnthropicErrors(t *testing.T) {
	for status, typ := range map[int]string{429: "rate_limit_error", 529: "overloaded_error", 500: "api_error"} {
		srv, recs := newTest(t, Options{Status: status, RetryAfter: "7"})
		resp := post(t, srv.URL+"/v1/messages", `{"messages":[]}`, nil)
		var e struct {
			Type  string
			Error struct{ Type string }
		}
		if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status || resp.Header.Get("Retry-After") != "7" || e.Type != "error" || e.Error.Type != typ {
			t.Errorf("%d: got %d retry-after %q %+v", status, resp.StatusCode, resp.Header.Get("Retry-After"), e)
		}
		if len(*recs) != 1 {
			t.Errorf("%d: the request was not recorded", status)
		}
		// GET /v1/models still answers: the error is for inference.
		r, err := http.Get(srv.URL + "/v1/models")
		if err != nil || r.StatusCode != 200 {
			t.Errorf("models under Status: %v %v", r, err)
		}
	}
}
