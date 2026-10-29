# bench

Measuring tools for LLM gateways such as
[llmflow-gateway](https://github.com/go-llmflow/gateway), in pure Go:

| command | what it is |
|---|---|
| [`cmd/gwbench`](cmd/gwbench) | interleaved A/B of gateways placed in front of one upstream: the latency each gateway **adds**, paired round by round against a direct call, refused when the machine is loaded |
| [`cmd/fakevllm`](cmd/fakevllm) | a recording fake vLLM: answers OpenAI chat completions (JSON or SSE) and the Anthropic Messages API (JSON or SSE), and writes every request it receives (method, path, headers, body) as one JSON line |
| [`e2e`](e2e) | end-to-end checks of llmflow-gateway in front of a **real** vLLM: fair-share priorities and preemption, prefix-cache isolation, Responses / Messages / tokenize / embeddings / rerank, accounting |
| [`e2e/oidc`](e2e/oidc) | end-to-end checks of llmflow-gateway's OpenID Connect paths against **real** identity providers, dex and go-authn/bridge (behind a test SAML IdP): JWT access tokens, refusals, key rotation, SSO with PKCE, cross-organisation isolation, SCIM |
| [`e2e/anthropic`](e2e/anthropic) | end-to-end checks of llmflow-gateway's opt-in Anthropic upstream: per-organisation keys, egress masking, USD accounting, retry-after; against the fake API, or the real one with a key file |
| [`e2e/fluids`](e2e/fluids) | end-to-end check of llmflow-gateway's measured-energy accounting (gateway #97): a real gateway process scrapes a **fake NVIDIA DCGM exporter** while two organisations send chat requests, and `/admin/fluids` must return the energy the counters moved by, to the millijoule, through a driver reload and an exporter that is down; needs `BENCH_GATEWAY_BIN` (gateway v0.19.0 or later) and skips without it |
| [`fakeupstream`](fakeupstream) | the fake vLLM as a Go package, for tests that embed it with `httptest`; also mimics Anthropic's API (429/529, retry-after, prompt-cache usage, workspace and rate-limit headers) |

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
| `POST /v1/messages/count_tokens` | `{"input_tokens": N}`, where N is the number of words in the prompt (`system` included), as Anthropic and vLLM answer it; it carries the same workspace headers and `--status` as `/v1/messages` (since v0.4.1) |
| any other `POST` | an OpenAI `chat.completion`; with `"stream": true`, `chat.completion.chunk` events (role, one chunk per word of `--reply`, finish), the usage chunk with empty `choices` when `stream_options.include_usage` is set, then `data: [DONE]` |

Usage is predictable: one prompt token per whitespace-separated word of the
request's message and system strings, one completion token per word of
`--reply`. `--delay` and `--token-delay` slow the answer and the stream.
`--generate N` (since v0.8.0) reports N completion tokens instead, capped by
the request's `max_tokens` / `max_completion_tokens` when it has one, as vLLM
does.

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

### Anthropic API mode (since v0.4.0)

To test a gateway in front of Anthropic's API without a key, fakevllm
reproduces the behaviour a gateway must handle there (references opened
2026-10-26: platform.claude.com/docs `api/errors`, `api/rate-limits`,
`build-with-claude/prompt-caching`):

| flag | effect |
|---|---|
| `--status 429` / `529` / … | every POST answers this status with an Anthropic error body (`rate_limit_error`, `overloaded_error`, `api_error`); `GET /v1/models` still answers |
| `--retry-after 30` | the `retry-after` header of those answers |
| `--cache-read N`, `--cache-write-5m N`, `--cache-write-1h N` | prompt-cache usage on `/v1/messages`: `cache_read_input_tokens`, `cache_creation_input_tokens` and its split `cache_creation.ephemeral_5m/1h_input_tokens`. `input_tokens` stays the uncached prompt |
| `--workspace ID` / `per-key` | `anthropic-workspace-id` on `/v1/messages` answers; `per-key` derives one workspace per `x-api-key`, as Anthropic resolves a key to its workspace |
| `--ratelimit-headers` | `anthropic-ratelimit-*` headers, so a test can check a gateway does not pass them on |

Streams now carry Anthropic's `ping` event after `message_start`. In Go,
`fakeupstream.Server.SetStatus` switches the status while serving.

## e2e/anthropic: llmflow-gateway in front of Anthropic

`e2e/anthropic` runs the `llmflow-gateway` binary (`BENCH_GATEWAY_BIN`,
v0.12.0 or later) with an Anthropic model for two organisations, each with
its own key. The fake API is `fakeupstream` in Anthropic mode. The test
checks:
- the provider receives the organisation's key, never the client's credential or the OAuth beta;
- PII is masked and `cache_salt` and `priority` are gone before egress;
- streams go through, and `/v1/chat/completions` is refused;
- the spend report is in USD and equals the price list applied to the provider's usage (reads, 5-minute and 1-hour writes, output);
- a 429 with `retry-after` makes the gateway answer 429 itself, without calling the provider, for that organisation only, until the wait is over.

`TestGatewayInFrontOfRealAnthropic` repeats a minimal exchange against
`https://api.anthropic.com` when `BENCH_ANTHROPIC_KEY_FILE` names a file
holding an API key (`BENCH_ANTHROPIC_MODEL`, default `claude-haiku-5-5`).
The variable names a file: a key never goes in the environment or a command
line. It is skipped otherwise; it was not run for v0.4.0, since no key was
available.

```sh
BENCH_GATEWAY_BIN=$(go env GOPATH)/bin/llmflow-gateway go test -v ./e2e/anthropic
```

On 2026-10-26, with llmflow-gateway v0.12.0, the fake-API test passes.

## e2e: llmflow-gateway against a real vLLM

The gateway's design rests on vLLM behaviours read in vLLM's docs and source.
`e2e/` checks them live: it starts `llmflow-gateway` with freshly generated
keys (nothing secret is read from the environment), creates one virtual key
per team, and drives it in front of a running vLLM. Without `BENCH_VLLM_URL`
every test skips, so CI runs none of them.

| variable | meaning |
|---|---|
| `BENCH_VLLM_URL` | the chat vLLM, e.g. `http://127.0.0.1:8000` (required) |
| `BENCH_VLLM_MODEL` | its model name (default: the first of `/v1/models`) |
| `BENCH_GATEWAY_BIN` | the `llmflow-gateway` binary (default: from `PATH`) |
| `BENCH_VLLM_MAX_NUM_SEQS` | the server's `--max-num-seqs` (the scenarios need 2) |
| `BENCH_VLLM_KV_SHORTAGE=1` | vLLM runs with a tiny KV cache: enables the preemption checks |
| `BENCH_VLLM_EMBED_URL`, `BENCH_VLLM_RERANK_URL` | optional embedding and score-model servers |

Gateway configuration used by every test: organisation A (teams `a1`, `a2`)
with share 0.5 and B (`b1`) with share 0.25 on a pool of capacity 2, so by
Hamilton apportionment A reserves 1 slot and B none: every request of B
borrows, the first request of A is within its share. `cache_isolation = "org"`.

### How vLLM was run (2026-10-26)

vLLM **0.32.1rc1.dev505+g00b6c4c56** (`vllm/vllm-openai-cpu:nightly-arm64`)
in Docker on an Apple M-series Mac (arm64 Linux VM, 8 GiB), model
**Qwen/Qwen2.5-0.5B-Instruct** in bfloat16; `BAAI/bge-small-en-v1.5`
(embeddings) and `BAAI/bge-reranker-base` (rerank). The released
`v0.32.0-arm64` CPU image fails every generation on this machine
(`compute_slot_mapping`: "Boolean value of Tensor with more than one value is
ambiguous"), which is why the nightly is used. llmflow-gateway **v0.8.0**.

```sh
# checks a-e except preemption
docker run -d -p 127.0.0.1:8000:8000 -e VLLM_CPU_KVCACHE_SPACE=1 \
  -e VLLM_CPU_OMP_THREADS_BIND=0-7 -e VLLM_ENABLE_RESPONSES_API_STORE=1 \
  vllm/vllm-openai-cpu:nightly-arm64 --model Qwen/Qwen2.5-0.5B-Instruct \
  --dtype bfloat16 --max-model-len 4096 --max-num-seqs 2 \
  --scheduling-policy priority --enable-prefix-caching
BENCH_VLLM_URL=http://127.0.0.1:8000 go test -v -timeout 60m ./e2e

# preemption: a 640-token KV cache
docker run ... --max-model-len 512 --max-num-seqs 2 --scheduling-policy priority \
  --enable-prefix-caching --kv-cache-memory-bytes 8M
BENCH_VLLM_URL=http://127.0.0.1:8000 BENCH_VLLM_KV_SHORTAGE=1 go test -v -run Preempt ./e2e
```

`VLLM_ENABLE_RESPONSES_API_STORE=1` is needed for the Responses control:
without it vLLM stores no response at all, whatever `store` says.

### Results (2026-10-26, gateway v0.8.0)

Every ordering check has a **control** straight to vLLM that must come out
the other way; the cache checks read vLLM's own counters
(`vllm:prefix_cache_hits_total`, `vllm:prefix_cache_queries_total`), never
timing.

| check | test | result | evidence |
|---|---|---|---|
| control: FCFS at equal priority | `TestControlFCFSWithoutPriorities` | pass | direct, all priority 0: B3 (sent first) first token 4.7 s before A1 |
| vLLM: `X-Vllm-Priority` overrides the body | `TestVLLMHeaderOverridesBodyPriority` | pass | body 100 + header −100: B3 before A1; body 100 alone: A1 before B3 |
| (a) within-share before borrowers | `TestWithinShareBeforeBorrowers` | pass | through the gateway A1 (sent after B3) got its first token 5.2 s before the borrower B3 |
| (a) control: victim is the latest arrival | `TestControlPreemptsLatestArrival` | pass | KV 640 tokens, 2 × 384 generated; direct: A1 (later) stalled 1 m 35 s, B1 finished first |
| (a) borrower preempted first | `TestBorrowerPreemptedFirst` | pass | through the gateway: the borrower B1 (started first) stalled 1 m 22 s, A1's longest gap 1.06 s, A1 finished first |
| (b) client priority ignored | `TestClientCannotRaiseItsPriority` | pass | B3 sent body `priority`, `vllm_xargs`, `extra_body`, `X-Vllm-Priority: -100`: A1 still first |
| (b)+(c) prefix cache | `TestPrefixCacheIsolation` | pass | control direct, unsalted repeat: 768/812 hit; a1 on that unsalted prompt: 0/812; a2 after a1 (same org): 768/814; b1 after a1: 0/814; b1 with a1's client `cache_salt`: 0/814; a2 with another client salt: 768/814 |
| (d) Responses, stateless | `TestResponsesStatelessThroughGateway` | pass | gateway response (`store: true` asked) is 404 at vLLM's `GET /v1/responses/{id}`; control: a direct `store: true` response is readable |
| (d) Messages | `TestMessagesThroughGateway` | pass | JSON `type=message`, SSE `message_start … message_stop` |
| (d) `/tokenize` | `TestTokenizeThroughGateway` | pass | count 6 through the gateway, 6 from vLLM |
| (d) embeddings | `TestEmbeddingsThroughGateway` | pass | 2 vectors of 384 |
| (d) rerank v1 and v2 | `TestRerankThroughGateway` | pass | Paris ranked first (0.998), `prompt_tokens` 34 |
| (e) accounting | `TestAccountingMatchesVLLM` | pass | chat JSON + chat stream (no client usage) + Messages + Responses: vLLM counted 132 prompt / 44 generated, the gateway charged 132 / 44 |
| vLLM beyond `--max-num-seqs` | `TestVLLMQueuesInsteadOfRefusing` | pass | 6 concurrent requests with 2 slots: six 200, no 429 or 503 |
| fallback on saturation (gateway v0.11.0, #58) | `TestFallbackOnSaturation` | pass | two borrowing streams of b1 fill pool `cpu` (C = 2); the next b1 request is served by `chat-spare` (`X-Llmflow-Model`), a1's within-share request stays on `chat`; `llmflow_fallbacks_total{reason="saturated"}` = 1. Run on 2026-10-26 with `--max-num-seqs 4` (both pools on one vLLM), gateway built from go-llmflow/gateway#66 |

### Hard budgets: usage ≤ W (gateway v0.17.0, #87)

`TestHardBudgetUsageWithinW` sends requests through a gateway whose team
has a budget. For each one, vLLM's usage must stay within the worst case W
the gateway held: `W = prompts · max((L−M)·p_in + n·M·p_out, L·p_in)`, with
`L` vLLM's `max_model_len` (its `/v1/models`) and `M` the `max_tokens`
sent (the client's, or the gateway's in `X-Llmflow-Max-Tokens`). The test
checks the tokens (prompt ≤ prompts·L, completion ≤ prompts·n·M) and the
cost. It also checks that the ledger never passes the limit, and that a
budget below the smallest W refuses with 429 before vLLM sees anything.

Run on 2026-10-28 with vLLM 0.32.1rc1 nightly CPU, Qwen2.5-0.5B-Instruct,
`--max-model-len 512 --max-num-seqs 2`, and gateway v0.17.0:

```sh
docker run -d --shm-size=1g -p 127.0.0.1:8000:8000 -e VLLM_CPU_KVCACHE_SPACE=1 \
  vllm/vllm-openai-cpu:nightly-arm64 --model Qwen/Qwen2.5-0.5B-Instruct \
  --dtype bfloat16 --max-model-len 512 --max-num-seqs 2
BENCH_VLLM_URL=http://127.0.0.1:8000 go test -v -run TestHardBudgetUsageWithinW ./e2e
```

(Without `--shm-size`, the nightly fails at start: "Insufficient space in /dev/shm".)

| request | M sent | usage (prompt / completion) | cost ≤ W |
|---|---|---|---|
| chat, no `max_tokens` | 512 (picked) | 35 / 149 | 0.780 ≤ 2.560 |
| chat, `n` = 3 | 16 | 34 / 42 | 0.244 ≤ 0.736 |
| chat, streamed | 32 | 34 / 32 | 0.194 ≤ 0.640 |
| chat, prompt 488 tokens of 512 | 64 | 488 / 10 | 0.538 ≤ 0.768 |
| same, no `max_tokens` | 512 (picked) | 488 / 24 | 0.608 ≤ 2.560 |
| completions, 2 prompts, `n` = 2 | 8 | 9 / 32 | 0.169 ≤ 1.168 |

The test prices are 1000 / 5000 / 100 per million tokens (input / output /
cached). On a prompt near L, vLLM cut the output at `L − input` as
`get_max_tokens` says, which is why W counts `L − M` input tokens beside `M`
output tokens.

`TestFallbackOnSaturation` needs `BENCH_VLLM_MAX_NUM_SEQS=4` (a vLLM started
with `--max-num-seqs 4`) and a gateway of v0.11.0 or later; it skips
otherwise.

Not covered: audio transcription (no Whisper model run on CPU), throughput
(deliberately not measured here).

## e2e: can LiteLLM exceed a `max_budget`? (since v0.8.0)

`TestLiteLLMBudgetOvershoot` (`e2e/litellm_budget_test.go`) measures LiteLLM
**v1.106.5**'s pre-call budget reservation
(`litellm/proxy/spend_tracking/budget_reservation.py`, read at that version):
- `_apply_over_budget_reservation_policy` (l.171-207) **resizes** a reservation
  that does not fit to the room left and admits the request, unless
  `general_settings.fail_closed_budget_enforcement` is true (l.196-201);
- the reservation is `min(client max_tokens or 16384, model max_output_tokens or
  16384)` output tokens (l.1651-1670), plus the input tokens counted by
  `litellm.token_counter`;
- no token price means no reservation (l.305-309).

Each scenario starts a fresh PostgreSQL 18 and Redis 7 (docker, removed at the
end), a fresh LiteLLM (`--num_workers 1`, Redis spend counters checked present
in the end, `proxy_batch_write_at: 5`), and a team with `max_budget` and one
key. The upstream is `fakeupstream` with `--generate`: like vLLM it stops at
the request's `max_tokens` when there is one and otherwise generates its whole
answer. Prices are declared in `model_info`: 1e-6 USD per input and per output
token. Ten requests are sent at once, then the team's spend is read from
`/team/info` once stable.

| | config | max_budget | final spend | overshoot | admitted / refused | `max_tokens` upstream |
|---|---|---|---|---|---|---|
| 1 | default, client `max_tokens` 4000, model uses 4000 | 0.01 | 0.012006 | +0.002006 USD (**+20.1 %**) | 3 / 7 | 4000 (the client's) |
| 2 | `fail_closed`, **no** client `max_tokens`, model emits 20000 | 0.05 | 0.060006 | +0.010006 USD (**+20.0 %**) | 3 / 7 | **none** (0 of 3 bodies) |
| 3 | `fail_closed`, client `max_tokens` 4000 (control) | 0.01 | 0.008004 | none (-20 %) | 2 / 8 | 4000 |

What this shows:
1. In default mode the third request is resized to the 0.002 left and admitted,
   although it really costs 0.004. The overshoot is bounded by one worst-case
   request per budget (further requests find 0 left and get 422).
2. Strict mode holds only when the model's output is bounded. Without a client
   `max_tokens`, LiteLLM reserves 16384 tokens (0.016394 per request) but
   **sends no bound upstream** (the fake recorded `{"model":…,"messages":…,
   "stream":false}`), so a vLLM bounded only by its context window spent 20000.
   Three requests fit the reservation (3 x 0.016394 <= 0.05) and cost 0.06.
3. With an explicit `max_tokens` the bound reaches the model and strict mode
   holds (control): the third request is refused, spend stays below the
   budget. Once spend is over (1, 2), a later request is refused (422).

Refusals are `HTTP 422 budget_exceeded`. Raw numbers, the configurations and
the upstream bodies: `BENCH_LITELLM_RESULTS=file.json`. Not run: the optional
fourth scenario (a prompt whose real token count exceeds LiteLLM's estimate),
which needs a real Qwen tokenizer; a single run per scenario, on one machine,
no timing involved. Run on 2026-10-28:

```sh
uv venv venv && uv pip install --python venv/bin/python 'litellm[proxy,extra_proxy]==1.106.5'
PATH=$PWD/venv/bin:$PATH venv/bin/prisma generate --schema venv/lib/python3.12/site-packages/litellm/proxy/schema.prisma
PATH=$PWD/venv/bin:$PATH BENCH_LITELLM_BIN=$PWD/venv/bin/litellm go test -v -run TestLiteLLMBudgetOvershoot ./e2e
```

`fakevllm --generate N` (and `fakeupstream.Options.Generate`) reports N
completion tokens in the usage, capped by the request's `max_tokens` or
`max_completion_tokens` when it carries one. The test skips without
`BENCH_LITELLM_BIN` or docker (`BENCH_LITELLM_PG_IMAGE`,
`BENCH_LITELLM_REDIS_IMAGE` override the images).

## e2e/oidc: llmflow-gateway against real identity providers

The gateway's JWT path (go-authn/oidc), SSO (go-authn/rp) and SCIM had met
only test keys. `e2e/oidc` runs the same checks against two real providers.
Each test starts its provider, a counting proxy in front of it (the proxy's
URL is the issuer, so every key-set fetch the gateway makes is counted), the
fake upstream and the gateway, all with generated secrets.

| variable | provider |
|---|---|
| `BENCH_DEX_IMAGE` | **dex**, as a container, e.g. `ghcr.io/dexidp/dex:v2.46.1`: static clients, static passwords with `groups`, the password grant for API tokens, `keysRotationPeriod` 4 min. Two dex instances share the issuer string but not the keys. |
| `BENCH_BRIDGE_BIN` | **go-authn/bridge** (`authn-bridge`) in front of a test SAML federation and IdP from [go-authn/saml/samltest](https://github.com/go-authn/saml#testing-a-program-built-on-it-samltest) (v0.5.0, pure Go). The metadata and the Responses are signed, and the assertions encrypted to the bridge. People log in through the IdP's form; `eppn` becomes `preferred_username` and `eduPersonEntitlement` becomes `groups`. API tokens come from the authorization-code flow with PKCE. Key rotation follows the bridge README's [procedure](https://github.com/go-authn/bridge#rotating-the-token-signing-key), with a restart at each step: the next key is published first in `retired_signing_key_files` (the JWKS lists both kids, tokens keep the old one), then it signs and the old key stays published. |
| `BENCH_GATEWAY_BIN` | llmflow-gateway (default: from `PATH`) |

Gateway configuration: organisations `inst-alpha` (team `lab-alpha`) and
`inst-beta` (team `lab-beta`). `jwt` maps group `grp-alpha` to `lab-alpha`
and `grp-beta` to `lab-beta`, with `clock_skew` 5 s and `min_refresh` 20 s.
`sso` binds `admins-alpha` to `org_admin` on `inst-alpha`. `scim` binds
group `Beta Leads` to `team_admin` on `lab-beta`. The people are alice
(`grp-alpha`, `admins-alpha`), bob (`grp-beta`) and carol (no group).

The upstream is the fake one (`fakeupstream`). The browser is a Go HTTP
client that follows redirects and submits the provider's forms. It keeps the
gateway's `__Host-` cookies on `http://127.0.0.1`, as browsers do for a
potentially trustworthy origin (W3C Secure Contexts §3.1).

```sh
go install github.com/go-authn/bridge/cmd/authn-bridge@v0.23.4
BENCH_DEX_IMAGE=ghcr.io/dexidp/dex:v2.46.1 BENCH_BRIDGE_BIN=$(go env GOPATH)/bin/authn-bridge \
  go test -v -timeout 20m ./e2e/oidc
```

### Results (2026-10-26, llmflow-gateway v0.8.0)

dex v2.46.1 (`ghcr.io/dexidp/dex`; the fleet's `claimward/dex` is an
unmodified fork 232 commits behind it). authn-bridge v0.23.4. The SAML side was signed by xmlsec1 1.3.12 in bench
v0.3.0. From v0.3.1 it is go-authn/saml/samltest v0.5.0, whose own tests
have xmlsec1 verify and decrypt its output. `TestBridge` passed again with
it, two-step rotation included, on 2026-10-26.

| check | dex | authn-bridge | evidence (both) |
|---|---|---|---|
| (a) token accepted, charged to the mapped org/team | pass | pass | alice → `inst-alpha/lab-alpha`, bob → `inst-beta/lab-beta` in `llmflow_requests_total` and `/admin/spend` |
| (b) wrong audience | pass | pass | 401, the same body as a garbage bearer |
| (b) signed by another key, same issuer | pass | pass | 401, same body |
| (b) `alg: none`, HS256 keyed with the public key (PEM, DER) | pass | pass | 401, same body |
| (b) expired | pass | pass | 401 after `exp` + 5 s skew |
| (b) unknown-`kid` flood | pass | pass | 40 tokens after a quiet `min_refresh`: exactly **1** key-set fetch at the provider |
| (c) key rotation | pass | pass | old token (not expired) and new token both 200, **1** fetch; an SSO login right after the rotation works |
| (d) SSO code + PKCE S256 + state + nonce | pass | pass | `__Host-llmflow-session` Secure, HttpOnly, SameSite=Lax, Path=/; `/admin/whoami` shows `org_admin` on `inst-alpha` |
| (d) replayed callback, callback in another browser | pass | pass | 400 both |
| (d) no role binding | pass | pass | bob: 403 "no administrative role for this account" |
| (d) logout | pass | pass | without CSRF token 403; with it 200; the old cookie then 401 |
| (e) org_admin of A against org B | pass | pass | 13 admin listings without `inst-beta`/`lab-beta`; 6 writes on B 403 (control: the same budget write on A is 200); 7 UI pages × 4 queries without B |
| (f) SCIM user + group → role | pass | pass | carol refused before; after SCIM `team_admin` on `lab-beta` in `/admin/users/…/roles` and in her session |

Findings filed: go-llmflow/gateway#60 (SSO's key set refreshes at most once
a minute, fixed, so a provider rotation right after start-up refuses logins:
observed with dex rotating every 30 s), go-llmflow/gateway#61 (README says
https only; http to a loopback issuer is accepted), go-authn/bridge#79 (no
documented signing-key rotation procedure; the one tested here works).

## e2e: HA mutual TLS between two replicas (since v0.6.0)

`TestHAMutualTLSLive` (`e2e/ha_mtls_test.go`) proves that two llmflow-gateway
replicas really complete a mutual TLS 1.3 handshake on the HA internal port,
with certificate **files** (multi-line PEM) loaded through the configuration:
the path that v0.16.0 broke (every real PEM file was refused: "holds more
than one line") and v0.16.1 fixed. It needs the gateway binary
(`BENCH_GATEWAY_BIN`, v0.16.1 or later) and docker for a throw-away
PostgreSQL (`BENCH_PG_IMAGE`, default `postgres:18`, trust authentication, a
random loopback port, removed at the end). It needs no vLLM: the upstream is
a fake with a gate. It skips, with the reason, when docker or the image is
unavailable.

1. A CA and two replica certificates (SAN `DNS:llmflow-ha`; ECDSA for A,
   Ed25519 for B) are generated in Go and written as PEM files, keys mode
   0600. `check-config` accepts them.
2. Replica A starts and takes the pool's lease; replica B starts.
3. Requests go through B, the non-owner: all are 200 at priority 0,
   `llmflow_ha_unowned_total` stays 0 on B, and while three requests are held
   at the fake upstream the **owner's** `llmflow_inflight` reads 3 (B's
   `llmflow_ha_local_grants` too): the grants crossed the mTLS channel.
4. **Control.** B restarts with a certificate another CA signed. It loads, but
   the owner does not answer: its requests are admitted without an owner
   (`llmflow_ha_unowned_total` 3, priority not 0). If this did not fail, step
   3 would prove nothing about the handshake.

```sh
go install github.com/go-llmflow/gateway/cmd/llmflow-gateway@v0.16.1
BENCH_GATEWAY_BIN=$(go env GOPATH)/bin/llmflow-gateway go test -v -run TestHAMutualTLSLive ./e2e
```

Run on 2026-10-27 with llmflow-gateway v0.16.1 and PostgreSQL 18 (docker): pass
in 7 s. With a v0.16.0 binary the same test fails at `check-config`
(`secret … holds more than one line`), as it should.

## Fake slurmrestd and e2e: Slurm discovery (since v0.5.0)

The package `fakeupstream` provides `Slurmrestd`, a fake slurmrestd.
- `GET /slurm/<version>/jobs/` answers the jobs given to `SetJobs`, in the shape of `OPENAPI_JOB_INFO_RESP`.
- The `job_state` field is a flag array, for example `["RUNNING"]` or `["RUNNING","COMPLETING"]`.
- A request must carry `X-SLURM-USER-TOKEN` set to `Token`.
- `SetDown(true)` simulates an outage: the server answers 502.

The fake tests the gateway's Slurm discovery (go-llmflow/gateway#75) without a cluster.

`e2e/slurm_test.go` (`TestSlurmDiscoveryE2E`) runs on a real Slurm node, as root. It does the following:
1. Starts the gateway with a `slurm {}` model.
2. Submits, with `sbatch --uid`, a job of user `alice` in account `physics` that claims the model. This job must never be trusted.
3. Submits a job of `svc_llm` named `llmflow:llama:18100:3`, then waits for the pool's C to reach 3.
4. Sends a chat request through the job.
5. Runs `scancel` on the job, then checks that C falls back to 0 and the model is no longer served.

| Variable | Meaning |
|---|---|
| `BENCH_SLURM_RESTD` | slurmrestd, e.g. `http://127.0.0.1:6820` |
| `BENCH_SLURM_API` | data_parser version (default `v0.0.42`) |
| `BENCH_SLURM_JWT_FILE` | output of `scontrol token username=svc_llm lifespan=7200` |
| `BENCH_SLURM_JOB` | the job's command; `{PORT}` and `{KEYFILE}` (the vLLM key) are replaced |
| `BENCH_SLURM_MODEL`, `BENCH_SLURM_MEM`, `BENCH_SLURM_WAIT` | model id served, job memory, wait for admission |

### How Slurm was run (2026-10-26, arm64)

The setup is one Debian trixie container with Slurm 24.11.5:
- packages `slurmctld`, `slurmd`, `slurmrestd`, `slurm-wlm-jwt-plugin` and `munge`;
- `AuthAltTypes=auth/jwt`;
- `slurmrestd -a rest_auth/jwt -s openapi/slurmctld`, run as an unprivileged user. Its data_parser versions are v0.0.40 to v0.0.42.

The container runs with `--privileged`, because slurmrestd needs to unshare namespaces. For the vLLM run, `/opt/uv` and `/opt/venv` are copied from `vllm/vllm-openai-cpu:nightly-arm64`. The job serves `Qwen/Qwen2.5-0.5B-Instruct` and reads its key into `VLLM_API_KEY`, never through `--api-key`.

The files are in `e2e/slurm-node/`. Run them like this:

```sh
docker run -d --privileged --shm-size=2g --name slurm --hostname c1 \
  -v $PWD/out:/e2e -v ~/.cache/huggingface:/hf:ro llmflow-slurm-vllm
docker exec slurm sh -c 'scontrol token username=svc_llm lifespan=7200 > /e2e/jwt.env'
docker exec -e BENCH_SLURM_RESTD=http://127.0.0.1:6820 -e BENCH_SLURM_JWT_FILE=/e2e/jwt.env \
  -e BENCH_SLURM_MODEL=Qwen/Qwen2.5-0.5B-Instruct -e BENCH_SLURM_MEM=4500M -e BENCH_SLURM_WAIT=8m \
  -e 'BENCH_SLURM_JOB=/e2e/vllm-job.sh {PORT} {KEYFILE}' -e BENCH_GATEWAY_BIN=/e2e/llmflow-gateway \
  -w /tmp slurm /e2e/e2e.test -test.run TestSlurmDiscoveryE2E -test.v
```

Here `e2e.test` is `GOOS=linux go test -c ./e2e`, and the gateway and `vllm-job.sh` are copied into `out/`.

### Results (2026-10-26, gateway #77 head, Slurm 24.11.5, data_parser v0.0.42)

| Job | Result |
|---|---|
| `fakevllm` | admitted 2 s after `sbatch`, removed 2 s after `scancel` |
| vLLM CPU (Qwen2.5-0.5B, `--enforce-eager`) | admitted 18 s after `sbatch` (once `/health` and `/v1/models` answered), chat request 200, removed 2 s after `scancel` |
| `alice`/`physics` job claiming the model | rejected once (`reason="account"`) and never contacted |

Two earlier vLLM runs failed inside vLLM: once from too little `/dev/shm`, once because torch inductor found no C++ compiler. Each time the job kept `RUNNING` with `/health` down, and the gateway correctly never admitted it.

## Development

## Development

```sh
go vet ./... && go test -race ./...
go test -run '^$' -fuzz '^FuzzServe$' -fuzztime 30s ./fakeupstream
```

CI runs gofmt, vet, the race tests on Linux amd64/arm64 and macOS, a fuzz
smoke, `govulncheck`, and a `CGO_ENABLED=0` cross-build for Linux
(amd64, arm64, riscv64, s390x, 386), macOS and Windows. The load gate reads
`/proc/loadavg` on Linux and `sysctl vm.loadavg` on macOS; elsewhere it
cannot read the load and refuses to measure.

## License

See [LICENSE](LICENSE).
