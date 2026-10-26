# bench

Measuring tools for LLM gateways such as
[llmflow-gateway](https://github.com/go-llmflow/gateway), in pure Go:

| command | what it is |
|---|---|
| [`cmd/gwbench`](cmd/gwbench) | interleaved A/B of gateways placed in front of one upstream: the latency each gateway **adds**, paired round by round against a direct call, refused when the machine is loaded |
| [`cmd/fakevllm`](cmd/fakevllm) | a recording fake vLLM: answers OpenAI chat completions (JSON or SSE) and the Anthropic Messages API (JSON or SSE), and writes every request it receives (method, path, headers, body) as one JSON line |

```sh
go install github.com/go-llmflow/bench/cmd/gwbench@latest
go install github.com/go-llmflow/bench/cmd/fakevllm@latest
```

## gwbench: the protocol

The question is "how much latency does a gateway add?", not "how fast is the
model". The upstream must therefore answer as fast as possible (`fakevllm`),
and the number reported is a **difference**, not a latency.

1. **One upstream, several paths.** `direct` calls the upstream itself; every
   other target (`--litellm`, `--llmflow`, `--target NAME|URL|KEYFILE`,
   repeatable) is a gateway forwarding to that same upstream.
2. **Interleaving.** Each round sends one request to every target, one at a
   time, in a fresh random order (`--seed`). Drift of the machine (thermal
   state, a background job, the cache) spreads evenly over all targets instead
   of penalising whichever runs last. `--warmup` rounds are discarded.
3. **Pairing.** A gateway's overhead in round *r* is its latency minus the
   direct latency **of the same round**. Percentiles are taken over these
   paired differences, which cancels what both paths share (client, loopback,
   upstream).
4. **Load gating.** Ratios measured on a busy machine are meaningless. A run
   starts only when the 1-minute load average is at most `--max-load`
   (default 4; `--wait` waits for it, otherwise it refuses at once), and a run
   whose load **ends** above it is discarded, not averaged. `--runs` kept runs
   are collected; rounds stay paired when they are merged.
5. **The control.** `TestPlantedOverheadIsMeasured` puts a proxy that sleeps a
   known 3 ms and a transparent proxy in front of a test upstream: the
   instrument must report about 3 ms for the first, about 0 for the second,
   and separate the two. If it could not see a planted delay, its verdict on
   real gateways would mean nothing.

Requests are `POST` chat completions with `max_tokens: 1`, one user message
(`ping`, or the content of `--prompt-file`, sent as a JSON string). An HTTP
status other than 200 aborts the run: an error is never timed as a success.

### Keys

Keys are read from files, never from the command line or the environment.
`KEYFILE` may be empty for an open endpoint. `gwbench llmflow-key` creates an
llmflow virtual key through the admin API (`--master-key-file`) and writes it
to a mode 0600 file. Keep key files outside the repository.

### Example

```sh
fakevllm --addr 127.0.0.1:18000 --out /tmp/requests.jsonl &
llmflow-gateway serve --config llmflow.hcl &      # forwards to 127.0.0.1:18000
gwbench llmflow-key --admin http://127.0.0.1:18080 --master-key-file ~/keys/master --out ~/keys/llmflow
gwbench run --direct http://127.0.0.1:18000/v1/chat/completions \
  --target 'v0.8.0|http://127.0.0.1:18080/v1/chat/completions|'"$HOME"'/keys/llmflow' \
  --rounds 2000 --warmup 200 --runs 8 --wait 2h --max-load 4
```

### Result format

```
run 7 kept: load 3.50 -> 3.70            # or: run N discarded: load a -> b
kept runs=8, rounds per run=2000, warm-up=200
run 0 overhead p50: v0.1.0=96.125µs v0.6.0=109.999µs
...
target          p50        p90        p99          # raw latencies, all kept runs
direct     81.833µs   93.041µs  138.458µs
v0.6.0    193.875µs  212.292µs  351.834µs
overhead vs direct, paired per round (all kept runs):
v0.6.0    111.042µs  129.541µs  261.624µs          # the answer
```

The per-run p50 lines show whether runs agree; the last table is the result.
Report it with the load range of the kept runs, the machine, and the
`GOMAXPROCS` of each gateway.

## fakevllm

```sh
fakevllm --addr 127.0.0.1:18000 --out requests.jsonl --model fake --reply "ok"
```

| route | answer |
|---|---|
| `GET /v1/models` | one model, `--model` |
| `POST /v1/messages` | an Anthropic message; with `"stream": true` the event sequence `message_start`, `content_block_start`, `content_block_delta`…, `content_block_stop`, `message_delta` (with `usage.output_tokens`), `message_stop` |
| any other `POST` | an OpenAI `chat.completion`; with `"stream": true`, `chat.completion.chunk` events (role, one chunk per word of `--reply`, finish), the usage chunk with empty `choices` when `stream_options.include_usage` is set, then `data: [DONE]` |

Usage is predictable: one prompt token per whitespace-separated word of the
request's message and system strings, one completion token per word of
`--reply`. `--delay` and `--token-delay` slow the answer and the stream.

Every request is appended to `--out` (mode 0600) as
`{"method","path","headers","body"}`; a body that is not JSON is recorded as
a string. **The recording holds the headers as received, `Authorization`
included**: that is what a witness is for. Use test keys only. If the record
cannot be written the request gets a 500, so a test never reads a silent gap
as "nothing was forwarded". Bodies are capped by `--max-body` (1 MiB, 413
above).

References for the answer shapes: the Anthropic
[streaming messages](https://platform.claude.com/docs/en/build-with-claude/streaming)
event sequence, and vLLM's own OpenAI-compatible server (the usage chunk
with empty `choices`, `data: [DONE]`).

## Development

```sh
go vet ./... && go test -race ./...
go test -run '^$' -fuzz '^FuzzServe$' -fuzztime 30s ./cmd/fakevllm
```

CI runs gofmt, vet, the race tests on Linux amd64/arm64 and macOS, a fuzz
smoke, `govulncheck`, and a `CGO_ENABLED=0` cross-build for Linux
(amd64, arm64, riscv64, s390x, 386), macOS and Windows. The load gate reads
`/proc/loadavg` on Linux and `sysctl vm.loadavg` on macOS; elsewhere it
cannot read the load and refuses to measure.

## License

See [LICENSE](LICENSE).
