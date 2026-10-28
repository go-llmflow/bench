// Package fakeupstream is a recording fake vLLM: it answers OpenAI chat
// completions (JSON or SSE) and the Anthropic Messages API (JSON or SSE),
// and hands every request it receives to a recorder. cmd/fakevllm serves
// it; tests embed it with net/http/httptest.
package fakeupstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

	// Generate, when positive, is the number of completion tokens the model
	// wants to emit, as reported in usage. Like vLLM it stops at the request's
	// max_tokens / max_completion_tokens when the body carries one, and emits
	// all of them when it does not (vLLM then bounds generation only by the
	// context window). Zero: the usage counts the words of Reply.
	Generate int

	// Status, when set, answers every POST with it and an Anthropic error
	// body (rate_limit_error for 429, overloaded_error for 529, api_error
	// otherwise), with RetryAfter as retry-after when set
	// (platform.claude.com/docs/en/api/errors and /api/rate-limits).
	Status     int
	RetryAfter string
	// CacheRead, CacheWrite5m and CacheWrite1h are the prompt-cache usage of
	// a /v1/messages answer, as Anthropic reports it: input_tokens is then
	// the uncached prompt only, cache_creation_input_tokens the sum of the
	// writes, split in cache_creation
	// (platform.claude.com/docs/en/build-with-claude/prompt-caching).
	CacheRead, CacheWrite5m, CacheWrite1h int
	// Workspace is sent as anthropic-workspace-id on /v1/messages answers;
	// RateLimitHeaders adds anthropic-ratelimit-* headers to them.
	Workspace        string
	RateLimitHeaders bool
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
		Messages            json.RawMessage `json:"messages"`
		System              json.RawMessage `json:"system"`
		MaxTokens           *int            `json:"max_tokens"`
		MaxCompletionTokens *int            `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &req) // a non-JSON body still gets a chat answer
	prompt := countWords(req.Messages) + countWords(req.System)
	if s.opt.Delay > 0 {
		time.Sleep(s.opt.Delay)
	}
	if r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens" {
		switch {
		case s.opt.Workspace == "per-key" && r.Header.Get("X-Api-Key") != "":
			// One workspace per API key, as Anthropic resolves a key to
			// the workspace it belongs to.
			sum := sha256.Sum256([]byte(r.Header.Get("X-Api-Key")))
			w.Header().Set("Anthropic-Workspace-Id", "wrkspc_"+hex.EncodeToString(sum[:6]))
		case s.opt.Workspace != "" && s.opt.Workspace != "per-key":
			w.Header().Set("Anthropic-Workspace-Id", s.opt.Workspace)
		}
		w.Header().Set("Request-Id", "req_fake")
		if s.opt.RateLimitHeaders {
			w.Header().Set("Anthropic-Ratelimit-Requests-Limit", "1000")
			w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "999")
			w.Header().Set("Anthropic-Ratelimit-Tokens-Remaining", "1000000")
		}
	}
	s.mu.Lock()
	status, retryAfter := s.opt.Status, s.opt.RetryAfter
	s.mu.Unlock()
	if status != 0 && r.Method == http.MethodPost {
		s.fail(w, status, retryAfter)
		return
	}
	words := strings.Fields(s.opt.Reply)
	generated := len(words)
	if s.opt.Generate > 0 {
		generated = s.opt.Generate
		for _, limit := range []*int{req.MaxTokens, req.MaxCompletionTokens} {
			if limit != nil && *limit < generated {
				generated = *limit
			}
		}
	}
	switch {
	case r.URL.Path == "/v1/messages/count_tokens":
		// Anthropic's and vLLM's answer: the prompt's input tokens (here
		// its words, as the usage counts them); nothing is generated.
		writeJSON(w, map[string]any{"input_tokens": prompt})
	case r.URL.Path == "/v1/messages" && req.Stream:
		s.messagesSSE(w, words, prompt)
	case r.URL.Path == "/v1/messages":
		writeJSON(w, map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant", "model": s.opt.Model,
			"content":     []any{map[string]any{"type": "text", "text": s.opt.Reply}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": s.anthropicUsage(prompt, len(words)),
		})
	case req.Stream:
		s.chatSSE(w, words, prompt, generated, req.StreamOptions.IncludeUsage)
	default:
		writeJSON(w, map[string]any{
			"id": "cmpl-1", "object": "chat.completion", "created": 1, "model": s.opt.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": s.opt.Reply}, "finish_reason": "stop"}},
			"usage":   usage(prompt, generated),
		})
	}
}

// anthropicUsage is the usage of a Messages answer: the prompt words are
// the uncached input, the configured cache reads and writes come on top.
func (s *Server) anthropicUsage(prompt, output int) map[string]any {
	u := map[string]any{"input_tokens": prompt, "output_tokens": output}
	if s.opt.CacheRead > 0 || s.opt.CacheWrite5m > 0 || s.opt.CacheWrite1h > 0 {
		u["cache_read_input_tokens"] = s.opt.CacheRead
		u["cache_creation_input_tokens"] = s.opt.CacheWrite5m + s.opt.CacheWrite1h
		u["cache_creation"] = map[string]any{"ephemeral_5m_input_tokens": s.opt.CacheWrite5m, "ephemeral_1h_input_tokens": s.opt.CacheWrite1h}
	}
	return u
}

// SetStatus changes Options.Status and RetryAfter while serving (0: answer
// normally again).
func (s *Server) SetStatus(status int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opt.Status, s.opt.RetryAfter = status, retryAfter
}

// fail answers with status and an Anthropic error body.
func (s *Server) fail(w http.ResponseWriter, status int, retryAfter string) {
	typ := "api_error"
	switch status {
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case 529, http.StatusServiceUnavailable:
		typ = "overloaded_error"
	}
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": "fakevllm: answering " + strconv.Itoa(status)}, "request_id": "req_fake"})
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
func (s *Server) chatSSE(w http.ResponseWriter, words []string, prompt, generated int, withUsage bool) {
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
			"choices": []any{}, "usage": usage(prompt, generated)})
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
		"stop_reason": nil, "stop_sequence": nil, "usage": s.anthropicUsage(prompt, 0)}})
	out.send("ping", map[string]any{"type": "ping"})
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
