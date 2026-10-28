package fakeupstream

import (
	"encoding/json"
	"testing"
)

// Generate behaves like vLLM: bounded by the request's max_tokens when it has
// one, unbounded (here: the whole Generate) when it has none.
func TestGenerateStopsAtMaxTokensOrNot(t *testing.T) {
	srv, _ := newTest(t, Options{Generate: 20000})
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"no bound", `{"messages":[{"role":"user","content":"hi"}]}`, 20000},
		{"max_tokens below", `{"max_tokens":4000,"messages":[{"role":"user","content":"hi"}]}`, 4000},
		{"max_completion_tokens below", `{"max_completion_tokens":7,"messages":[{"role":"user","content":"hi"}]}`, 7},
		{"max_tokens above", `{"max_tokens":99999,"messages":[{"role":"user","content":"hi"}]}`, 20000},
	} {
		resp := post(t, srv.URL+"/v1/chat/completions", tc.body, nil)
		var c struct {
			Usage struct {
				Completion int `json:"completion_tokens"`
			}
		}
		if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
			t.Fatal(err)
		}
		if c.Usage.Completion != tc.want {
			t.Errorf("%s: completion_tokens %d, want %d", tc.name, c.Usage.Completion, tc.want)
		}
	}
}
