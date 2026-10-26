package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Record is one request as the upstream received it: the witness for
// "what does a gateway actually forward to vLLM?".
type Record struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    json.RawMessage     `json:"body"`
}

// Options shape the answers of the fake upstream.
type Options struct {
	Model      string        // model id listed by /v1/models and echoed in answers
	Reply      string        // assistant text; split on spaces into SSE chunks
	Delay      time.Duration // before the first byte of every answer
	TokenDelay time.Duration // between two SSE chunks
	MaxBody    int64         // request bodies above this are refused with 413
}

// Server is an OpenAI- and Anthropic-compatible fake upstream that records
// every request through rec.
type Server struct {
	opt Options
	mu  sync.Mutex
	rec func(Record) error
}

// NewServer returns a handler. rec is called once per request, serialised.
func NewServer(opt Options, rec func(Record) error) *Server {
	if opt.Model == "" {
		opt.Model = "fake"
	}
	if opt.Reply == "" {
		opt.Reply = "ok"
	}
	if opt.MaxBody <= 0 {
		opt.MaxBody = 1 << 20
	}
	return &Server{opt: opt, rec: rec}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opt.MaxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	rec := Record{Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone()}
	if len(body) > 0 && json.Valid(body) {
		rec.Body = body
	} else {
		rec.Body, _ = json.Marshal(string(body))
	}
	if s.rec != nil {
		s.mu.Lock()
		err := s.rec(rec)
		s.mu.Unlock()
		if err != nil {
			// A witness that cannot write must not answer as if it had.
			http.Error(w, "record: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"id": s.opt.Model, "object": "model", "owned_by": "fakevllm"}}})
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/healthz") {
		w.WriteHeader(http.StatusOK)
		return
	}
	var req struct {
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages json.RawMessage `json:"messages"`
		System   json.RawMessage `json:"system"`
	}
	_ = json.Unmarshal(body, &req) // a non-JSON body still gets a chat answer
	prompt := countWords(req.Messages) + countWords(req.System)
	if s.opt.Delay > 0 {
		time.Sleep(s.opt.Delay)
	}
	words := strings.Fields(s.opt.Reply)
	switch {
	case r.URL.Path == "/v1/messages" && req.Stream:
		s.messagesSSE(w, words, prompt)
	case r.URL.Path == "/v1/messages":
		writeJSON(w, map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant", "model": s.opt.Model,
			"content":     []any{map[string]any{"type": "text", "text": s.opt.Reply}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": prompt, "output_tokens": len(words)},
		})
	case req.Stream:
		s.chatSSE(w, words, prompt, req.StreamOptions.IncludeUsage)
	default:
		writeJSON(w, map[string]any{
			"id": "cmpl-1", "object": "chat.completion", "created": 1, "model": s.opt.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": s.opt.Reply}, "finish_reason": "stop"}},
			"usage":   usage(prompt, len(words)),
		})
	}
}

func usage(prompt, completion int) map[string]any {
	return map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
}

// countWords is the fake tokenizer: one token per whitespace-separated word
// of every string in v, so a test can predict the usage it will be charged.
func countWords(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	n := 0
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			n += len(strings.Fields(x))
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				if k == "role" || k == "type" {
					continue
				}
				walk(e)
			}
		}
	}
	walk(v)
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type sse struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	f, _ := w.(http.Flusher)
	return sse{w, f}
}

func (s sse) send(event string, v any) {
	b, _ := json.Marshal(v)
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.f != nil {
		s.f.Flush()
	}
}

// chatSSE follows the OpenAI chat.completion.chunk stream as vLLM emits it:
// a role chunk, one chunk per piece of content, a finish chunk, the usage
// chunk (empty choices) when include_usage is set, then "data: [DONE]".
func (s *Server) chatSSE(w http.ResponseWriter, words []string, prompt int, withUsage bool) {
	out := newSSE(w)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 1, "model": s.opt.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	out.send("", chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	for i, wd := range words {
		if i > 0 {
			wd = " " + wd
			s.pause()
		}
		out.send("", chunk(map[string]any{"content": wd}, nil))
	}
	out.send("", chunk(map[string]any{}, "stop"))
	if withUsage {
		out.send("", map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 1, "model": s.opt.Model,
			"choices": []any{}, "usage": usage(prompt, len(words))})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if out.f != nil {
		out.f.Flush()
	}
}

// messagesSSE follows the Anthropic Messages stream: message_start,
// content_block_start, content_block_delta per piece, content_block_stop,
// message_delta (stop_reason and output usage), message_stop.
func (s *Server) messagesSSE(w http.ResponseWriter, words []string, prompt int) {
	out := newSSE(w)
	out.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "content": []any{}, "model": s.opt.Model,
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": prompt, "output_tokens": 0}}})
	out.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	for i, wd := range words {
		if i > 0 {
			wd = " " + wd
			s.pause()
		}
		out.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": wd}})
	}
	out.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	out.send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": len(words)}})
	out.send("message_stop", map[string]any{"type": "message_stop"})
}

func (s *Server) pause() {
	if s.opt.TokenDelay > 0 {
		time.Sleep(s.opt.TokenDelay)
	}
}
