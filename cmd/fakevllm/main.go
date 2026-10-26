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

	"github.com/spf13/cobra"
)

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
	ready = onListen
	return cmd
}

// onListen, when set (by tests), learns the listening address.
var onListen func(net.Addr)
