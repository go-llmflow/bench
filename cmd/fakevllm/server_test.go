package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T, opt Options) (*httptest.Server, *[]Record) {
	t.Helper()
	var recs []Record
	srv := httptest.NewServer(NewServer(opt, func(r Record) error { recs = append(recs, r); return nil }))
	t.Cleanup(srv.Close)
	return srv, &recs
}

func post(t *testing.T, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRecordsPathHeadersBody(t *testing.T) {
	srv, recs := newTest(t, Options{})
	body := `{"model":"m","messages":[{"role":"user","content":"hello world"}],"priority":3}`
	resp := post(t, srv.URL+"/v1/chat/completions", body, map[string]string{"X-Vllm-Priority": "10", "Authorization": "Bearer up"})
	io.Copy(io.Discard, resp.Body)
	if len(*recs) != 1 {
		t.Fatalf("%d records", len(*recs))
	}
	r := (*recs)[0]
	if r.Method != "POST" || r.Path != "/v1/chat/completions" {
		t.Fatalf("record %+v", r)
	}
	if got := http.Header(r.Headers).Get("X-Vllm-Priority"); got != "10" {
		t.Fatalf("priority header %q", got)
	}
	if got := http.Header(r.Headers).Get("Authorization"); got != "Bearer up" {
		t.Fatalf("authorization %q", got)
	}
	if !bytes.Equal(r.Body, []byte(body)) {
		t.Fatalf("body %s", r.Body)
	}
}

func TestNonJSONBodyIsRecordedAsString(t *testing.T) {
	srv, recs := newTest(t, Options{})
	resp := post(t, srv.URL+"/x", "not json", nil)
	io.Copy(io.Discard, resp.Body)
	var s string
	if err := json.Unmarshal((*recs)[0].Body, &s); err != nil || s != "not json" {
		t.Fatalf("body %s", (*recs)[0].Body)
	}
}

func TestChatJSONIsValidWithUsage(t *testing.T) {
	srv, _ := newTest(t, Options{Reply: "a b c"})
	resp := post(t, srv.URL+"/v1/chat/completions", `{"messages":[{"role":"user","content":"one two three four"}]}`, nil)
	var c struct {
		Object  string
		Choices []struct {
			Message      struct{ Role, Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Object != "chat.completion" || len(c.Choices) != 1 || c.Choices[0].Message.Content != "a b c" || c.Choices[0].FinishReason != "stop" {
		t.Fatalf("%+v", c)
	}
	if c.Usage.Prompt != 4 || c.Usage.Completion != 3 || c.Usage.Total != 7 {
		t.Fatalf("usage %+v", c.Usage)
	}
}

// readSSE returns the (event, data) pairs of a stream.
func readSSE(t *testing.T, r io.Reader) (events, datas []string) {
	t.Helper()
	sc := bufio.NewScanner(r)
	ev := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			events = append(events, ev)
			datas = append(datas, strings.TrimPrefix(line, "data: "))
			ev = ""
		case line == "":
		default:
			t.Fatalf("unexpected SSE line %q", line)
		}
	}
	return
}

func TestChatSSEWithUsage(t *testing.T) {
	srv, _ := newTest(t, Options{Reply: "hello there"})
	resp := post(t, srv.URL+"/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x y"}]}`, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	_, datas := readSSE(t, resp.Body)
	if datas[len(datas)-1] != "[DONE]" {
		t.Fatalf("no [DONE]: %v", datas)
	}
	text := ""
	var usage map[string]int
	finished := false
	for _, d := range datas[:len(datas)-1] {
		var c struct {
			Object  string
			Choices []struct {
				Delta        struct{ Content string }
				FinishReason *string `json:"finish_reason"`
			}
			Usage map[string]int
		}
		if err := json.Unmarshal([]byte(d), &c); err != nil {
			t.Fatalf("chunk %s: %v", d, err)
		}
		if c.Object != "chat.completion.chunk" {
			t.Fatalf("object %q", c.Object)
		}
		for _, ch := range c.Choices {
			text += ch.Delta.Content
			if ch.FinishReason != nil {
				finished = true
			}
		}
		if c.Usage != nil {
			if len(c.Choices) != 0 {
				t.Fatal("usage chunk carries choices")
			}
			usage = c.Usage
		}
	}
	if text != "hello there" || !finished {
		t.Fatalf("text %q finished %v", text, finished)
	}
	if usage["prompt_tokens"] != 2 || usage["completion_tokens"] != 2 {
		t.Fatalf("usage %v", usage)
	}
}

func TestChatSSEWithoutUsage(t *testing.T) {
	srv, _ := newTest(t, Options{})
	resp := post(t, srv.URL+"/v1/chat/completions", `{"stream":true,"messages":[]}`, nil)
	_, datas := readSSE(t, resp.Body)
	for _, d := range datas {
		if strings.Contains(d, `"usage"`) {
			t.Fatalf("usage sent without include_usage: %s", d)
		}
	}
}

func TestMessagesJSON(t *testing.T) {
	srv, recs := newTest(t, Options{Reply: "hi you"})
	resp := post(t, srv.URL+"/v1/messages", `{"model":"m","max_tokens":5,"system":"be brief","messages":[{"role":"user","content":[{"type":"text","text":"a b c"}]}]}`, nil)
	var m struct {
		Type, Role string
		Content    []struct{ Type, Text string }
		StopReason string `json:"stop_reason"`
		Usage      struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "message" || m.Role != "assistant" || len(m.Content) != 1 || m.Content[0].Text != "hi you" || m.StopReason != "end_turn" {
		t.Fatalf("%+v", m)
	}
	if m.Usage.In != 5 || m.Usage.Out != 2 {
		t.Fatalf("usage %+v", m.Usage)
	}
	if (*recs)[0].Path != "/v1/messages" {
		t.Fatal("path not recorded")
	}
}

func TestMessagesSSE(t *testing.T) {
	srv, _ := newTest(t, Options{Reply: "a b"})
	resp := post(t, srv.URL+"/v1/messages", `{"stream":true,"messages":[{"role":"user","content":"q"}]}`, nil)
	events, datas := readSSE(t, resp.Body)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v", events)
	}
	text := ""
	for i, d := range datas {
		var e struct {
			Type  string
			Delta struct{ Text string }
			Usage struct {
				Out int `json:"output_tokens"`
			}
		}
		if err := json.Unmarshal([]byte(d), &e); err != nil || e.Type != events[i] {
			t.Fatalf("event %d %s: %v", i, d, err)
		}
		text += e.Delta.Text
		if e.Type == "message_delta" && e.Usage.Out != 2 {
			t.Fatalf("output tokens %d", e.Usage.Out)
		}
	}
	if text != "a b" {
		t.Fatalf("text %q", text)
	}
}

func TestModelsAndBodyLimit(t *testing.T) {
	srv, _ := newTest(t, Options{Model: "qwen", MaxBody: 16})
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(b, []byte(`"id":"qwen"`)) {
		t.Fatalf("models %s", b)
	}
	big := post(t, srv.URL+"/v1/chat/completions", strings.Repeat("x", 100), nil)
	if big.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: HTTP %d", big.StatusCode)
	}
}

func TestRecorderFailureIsNotAnAnswer(t *testing.T) {
	srv := httptest.NewServer(NewServer(Options{}, func(Record) error { return errors.New("disk full") }))
	defer srv.Close()
	resp := post(t, srv.URL+"/v1/chat/completions", `{}`, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
}

func TestCountWords(t *testing.T) {
	if n := countWords(json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"a  b\nc"}]}]`)); n != 3 {
		t.Fatalf("%d", n)
	}
	if countWords(nil) != 0 || countWords(json.RawMessage(`{bad`)) != 0 {
		t.Fatal("empty or bad input")
	}
}

func TestRootCmdServesAndRecords(t *testing.T) {
	out := filepath.Join(t.TempDir(), "req.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { addrc <- a }
	defer func() { onListen = nil }()
	cmd := RootCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--addr", "127.0.0.1:0", "--out", out})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	var addr net.Addr
	select {
	case addr = <-addrc:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not listening")
	}
	resp := post(t, "http://"+addr.String()+"/v1/chat/completions", `{"messages":[]}`, nil)
	io.Copy(io.Discard, resp.Body)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("recording mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(out)
	var r Record
	if err := json.Unmarshal(bytes.TrimSpace(b), &r); err != nil || r.Path != "/v1/chat/completions" {
		t.Fatalf("recording %s: %v", b, err)
	}
}

func TestRootCmdRejectsArgsAndBadAddr(t *testing.T) {
	for _, args := range [][]string{{"extra"}, {"--addr", "256.0.0.1:x", "--out", ""}} {
		cmd := RootCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func FuzzServe(f *testing.F) {
	f.Add("/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":true},"messages":[{"content":"a"}]}`)
	f.Add("/v1/messages", `{"stream":true,"system":[{"text":"s"}]}`)
	f.Add("/", "\x00")
	s := NewServer(Options{}, func(Record) error { return nil })
	f.Fuzz(func(t *testing.T, path, body string) {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		req := httptest.NewRequest(http.MethodPost, "http://h/", strings.NewReader(body))
		req.URL.Path = path
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != 200 && w.Code != 413 {
			t.Fatalf("HTTP %d", w.Code)
		}
	})
}
