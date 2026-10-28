// fakevllm records every request it receives (method, path, headers, JSON
// body) as one JSON line, and answers like vLLM's OpenAI-compatible server:
// chat completions (JSON or SSE, with the usage chunk when asked) and the
// Anthropic Messages API (JSON or SSE). It is the upstream witness for
// "what does a gateway actually forward to vLLM?" and the upstream of
// gwbench.
//
// The recording holds the headers as received, Authorization included:
// that is the point of a witness. It is written with mode 0600; never point
// a real client with a real key at it.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-llmflow/bench/fakeupstream"
	"github.com/spf13/cobra"
)

type (
	Options = fakeupstream.Options
	Record  = fakeupstream.Record
)

var NewServer = fakeupstream.NewServer

var osExit = os.Exit

func main() {
	if err := RootCmd().Execute(); err != nil {
		osExit(1)
	}
}

// RootCmd builds the command; tests drive it with SetArgs.
func RootCmd() *cobra.Command {
	var (
		addr, out string
		opt       Options
		ready     func(net.Addr)
	)
	cmd := &cobra.Command{
		Use:          "fakevllm",
		Short:        "A recording fake vLLM: OpenAI chat (JSON/SSE) and Anthropic Messages (JSON/SSE)",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var rec func(Record) error
			if out != "" {
				f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				enc := json.NewEncoder(f)
				rec = func(r Record) error { return enc.Encode(r) }
			}
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "listening on %s, recording to %q\n", ln.Addr(), out)
			if ready != nil {
				ready(ln.Addr())
			}
			srv := &http.Server{Handler: NewServer(opt, rec), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-cmd.Context().Done()
				srv.Close()
			}()
			if err := srv.Serve(ln); err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&addr, "addr", "127.0.0.1:18000", "listen address")
	f.StringVar(&out, "out", "requests.jsonl", "file receiving one JSON record per request (mode 0600; empty: do not record)")
	f.StringVar(&opt.Model, "model", "fake", "model id served")
	f.StringVar(&opt.Reply, "reply", "ok", "assistant answer; streamed one word per SSE chunk")
	f.DurationVar(&opt.Delay, "delay", 0, "delay before every answer")
	f.DurationVar(&opt.TokenDelay, "token-delay", 0, "delay between two streamed chunks")
	f.Int64Var(&opt.MaxBody, "max-body", 1<<20, "largest request body accepted")
	f.IntVar(&opt.Generate, "generate", 0, "completion tokens reported in usage, capped by the request's max_tokens when it has one (0: the words of --reply)")
	// The Anthropic API's own behaviour, for gateways that front it.
	f.IntVar(&opt.Status, "status", 0, "answer every POST with this status and an Anthropic error body (429, 529, …)")
	f.StringVar(&opt.RetryAfter, "retry-after", "", "retry-after header of --status answers")
	f.IntVar(&opt.CacheRead, "cache-read", 0, "cache_read_input_tokens of every /v1/messages answer")
	f.IntVar(&opt.CacheWrite5m, "cache-write-5m", 0, "5-minute prompt-cache writes of every /v1/messages answer")
	f.IntVar(&opt.CacheWrite1h, "cache-write-1h", 0, "1-hour prompt-cache writes of every /v1/messages answer")
	f.StringVar(&opt.Workspace, "workspace", "", "anthropic-workspace-id header of /v1/messages answers")
	f.BoolVar(&opt.RateLimitHeaders, "ratelimit-headers", false, "send anthropic-ratelimit-* headers on /v1/messages answers")
	ready = onListen
	return cmd
}

// onListen, when set (by tests), learns the listening address.
var onListen func(net.Addr)
