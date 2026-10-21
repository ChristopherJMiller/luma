# Phase 1 spikes

The phase-1 plan's spikes, each with the run it was decided on. A spike's
recorded values are change detectors: the spike's tests assert them, so a
pinned dependency that starts behaving differently turns the suite red. The
sections are G (Task 3), S (Task 4), R (Task 14b) and L (the deploy plan's
live spikes).

## G. agentgateway passthrough (Task 3; spec section 17, row 1, offline half)

**Question.** Does agentgateway v1.6.0, run standalone, pass Claude Code CLI
↔ Anthropic Messages traffic faithfully: per-route bearers, exact-path
routes, the workspace key injected, no OpenAI `completions` fallback, no
prompt-caching rewrite, bodies semantically exact, SSE unbuffered, errors
untouched, and usage metrics with Luma's labels?

**Answer.** Yes, with route type `detect`. The §17 fallback (Task 3b, a Luma
gateway) is not needed.

### What ran

- **Image:** `ghcr.io/agentgateway/agentgateway:v1.6.0@sha256:9d3e6044ddcdc0878b1787f77bd401252b95e22684203fb5e874c4c42d2ed90c`
  (the multi-arch index; `docker buildx imagetools inspect`, 2026-10-21).
  linux/amd64 is `sha256:482921556876a503ad3675b29223b1897b63a316983e46797ff99ebf83a2a6a2`,
  linux/arm64 `sha256:3a07601c7dace14b2b4a8bfb9b73d4ba332d44a393a2b51f8c58404f1658a314`.
  Built 2026-10-02 (Chainguard glibc-dynamic base, user 65532).
- **Version** (`agentgateway --version`): `1.6.0`, git
  `ea5608642b9d5c94c5baf5fd9f2f9849b808e963`, Rust 1.99.0, release,
  x86_64-unknown-linux-gnu, aws-lc-rs, not FIPS.
- **Config:** `test/gateway/agentgateway.yaml`, the canonical config, with the
  upstream tokens filled with the fake (`localhost`, its port, `null`).
- **Against:** fakeanthropic at main `42e2c02`, `StepDelay` 2 s.
- **Command:** `nix develop -c make gateway-spike`, 2026-10-21, on rowlett
  (Linux 6.18, Docker 29.8.1).
- **Output:** every test passed in 217 s:

  ```
  --- PASS: TestCanonicalConfigHasNoUpstream (1.26s)
  --- PASS: TestBodyExact (0.25s)
      --- PASS: TestBodyExact/interactive (0.01s)
      --- PASS: TestBodyExact/ci (0.01s)
  --- PASS: TestHeadersForwarded (0.00s)
      --- PASS: TestHeadersForwarded/interactive (0.00s)
      --- PASS: TestHeadersForwarded/ci (0.00s)
  --- PASS: TestRouteBearersAreDistinct (0.00s)
  --- PASS: TestBearerRequired (0.00s)
  --- PASS: TestUnlistedPathRejected (0.00s)
  --- PASS: TestUpstreamPathRewritten (0.01s)
  --- PASS: TestLargeBody (14.32s)
  --- PASS: TestSSEUnbufferedPingKept (18.02s)
  --- PASS: TestLongStream (90.09s)
  --- PASS: TestErrorPassthrough (0.01s)
      --- PASS: TestErrorPassthrough/429 (0.00s)
      --- PASS: TestErrorPassthrough/529 (0.00s)
      --- PASS: TestErrorPassthrough/binding_400 (0.00s)
  --- PASS: TestUsageMetrics (18.08s)
  --- PASS: TestRequestsMetricHasStatusAndRequestClass (0.04s)
  --- PASS: TestUnknownLabelValuesBecomeOther (0.08s)
  --- PASS: TestUpstreamTLSSwitch (0.92s)
  --- PASS: TestRouteTypes (72.91s)
      --- PASS: TestRouteTypes/detect (36.49s)
      --- PASS: TestRouteTypes/messages (36.41s)
  ok  	github.com/ChristopherJMiller/luma/test/gateway	217.368s
  ```

  The cost, TTFT, cache-write and missing-`anthropic-version` figures below
  come from manual runs of the same config against the fakeanthropic binary.

### Decision: route type `detect`

`RouteType = "detect"` in `test/gateway/result.go`. `messages` fails the
semantic gate, so the spec's `detect` stays.

| | `detect` | `messages` |
|---|---|---|
| Body semantically exact (parsed JSON equal, keys in order, strings byte-equal) | yes | **no**: the top-level keys come out in its struct order (`messages` first) |
| Body `bytes.Equal` | no (below) | no |
| SSE: first event ≤ 500 ms, client bytes = upstream bytes, ping kept | yes | yes |
| Usage when `message_delta` repeats the counts (the API's shape) | input 1800, cache read 800, output 50 | the same |
| Usage when only `message_start` has input and cache counts | output 50 only | input 1800, cache read 800, output 50 |
| A lone surrogate escape (`"\ud83d"`) in the body | 200, forwarded byte for byte | **400** `failed to parse Messages request: unexpected end of hex escape` |
| TTFT and time-per-output-token histograms | none | yes |

The lone surrogate matters: `JSON.stringify` writes one for a string cut
inside a surrogate pair, as a truncated tool result can be, and under
`messages` every later request of that conversation would fail. Missing TTFT
under `detect` is accepted, because Core is the SLO source (P10). The API's
`message_delta` carries cumulative usage, so `detect` sees every count today;
if the API stopped repeating the input counts there, `detect` would record
output tokens only.

### Bodies under `detect`

`detect` parses a JSON body (by its source, one whose content type is
exactly `application/json`, as the CLI's is) and writes it back out with
serde_json, so the upstream body is not byte-equal
(`BodyByteEqual = false`), but it is semantically exact: every object keeps
its key order and every string decodes to the same bytes. What changes:

- numbers go through float64: `1e3` → `1000.0`, `2.50` → `2.5`, `-0` →
  `-0.0`, `1E-7` → `1e-7`, and an integer beyond 64 bits loses precision
  (`123456789012345678901234` → `1.2345678901234567e+23`); `1.0` and
  integers within 64 bits are kept;
- escapes that need none are written raw: `é` → `é`, `\/` → `/`,
  ` ` → U+2028; `\"`, `\\`, `\t` and `\u0001` stay escaped;
- a body that does not parse (a lone surrogate) is forwarded as sent.

The rewrite is deterministic, so a replayed prefix is rewritten the same way
every turn: thinking signatures bind to, and the prompt cache sees, the same
bytes each time. The CLI-shaped body's two signed thinking blocks passed
fakeanthropic's binding check after the gateway, and a 28 MiB body arrived
semantically exact.

### Routing, auth and headers

- **Bearers:** each route takes only its own bearer; none, a wrong one, the
  other route's, or the workspace key itself is a 401, and nothing reaches
  the upstream.
- **Paths:** `/<route>/v1/messages` and `/<route>/v1/messages/count_tokens`
  only. `GET /interactive/v1/models`, `POST /interactive/v1/chat/completions`,
  `POST /interactive/v1/messages/batches` and `POST /v1/messages` are 404 and
  reach nothing. So are request lines sent unnormalised with a dot segment
  (`/interactive/v1/messages/../models`), percent-encoding
  (`/interactive/v1/%6dessages`, `/%69nteractive/v1/messages`) or a doubled
  slash (`//interactive/v1/messages`): none is decoded or cleaned into a
  route match. `/ci/v1/messages?beta=true`
  arrives as `/v1/messages?beta=true`.
- **Headers:** on both routes and both paths, `anthropic-beta`,
  `x-claude-code-request-class` and the `x-luma-*` headers arrive unchanged;
  `anthropic-version` arrives as sent (`2023-06-01`), and the gateway adds
  that value when a client sends none; `authorization` is removed;
  `x-api-key` is the route's workspace key and nothing else. A client's own
  `x-api-key` sent with a valid bearer is replaced, not duplicated: the
  upstream sees one value, the route's.
- **Host:** for a hostname upstream the gateway sends `Host` as that name
  (here `localhost`, live `api.anthropic.com`), whatever the client sent; for
  an IP upstream it passes the client's `Host` through.
- **TLS:** `__UPSTREAM_TLS__` = `{}` makes the gateway speak TLS upstream
  and verify the certificate; `null` is plain http. Both fills pass
  `--validate-only`. With `{}`:
  - against the plain-http fake: 503 `upstream call failed: Connect:
    received corrupt message of type InvalidContentType`, and nothing
    reaches the fake;
  - against a TLS server whose valid leaf certificate for `localhost` signs
    itself: 503 `upstream call failed: Connect: invalid peer certificate:
    UnknownIssuer`, and no request reaches it. The image's system roots
    decide trust.
  - **SNI is the `hostOverride` host:** the gateway's ClientHello names
    `localhost` (the `__UPSTREAM_HOST__` fill), so live it names
    `api.anthropic.com`. rustls checks the certificate against that same
    server name; a name mismatch with a trusted certificate cannot be shown
    offline without adding a `backendTLS.root` to the config.

  TLS to `api.anthropic.com` itself is the live half's to show.
- **Errors:** 429 (with `retry-after: 7`), 529 and the fake's binding 400
  reach the client with the upstream's status, body and headers, after one
  upstream attempt (no gateway retry).
- **Streams:** the first SSE event (`message_start`) arrives within 500 ms
  while the next is 2 s away; the client's bytes equal the upstream's,
  `ping` included; a 90 s stream with a ping every 10 s completes under
  `responseIdleTimeout: 300s`.

### Metrics

The request counter is **`agentgateway_requests_total`**. Its labels, in the
order the gateway writes them:

```
backend, protocol, method, status, reason, bind, gateway, listener, route,
route_rule, person, agent, tier, source, request_class
```

The last five are Luma's (`config.metrics.fields.add`). A binding 400 on a
main-class request is:

```
agentgateway_requests_total{backend="/gateway/default/default/interactive/backend0",protocol="llm",method="POST",status="400",reason="Upstream",bind="bind/8080",gateway="default/default",listener="default",route="default/interactive",route_rule="unknown",person="u_…",agent="luma",tier="fast",source="web",request_class="main"} 1
```

so the deploy plan's thinking alert selects
`agentgateway_requests_total{status="400",request_class="main"}`. `route` is
`default/interactive` or `default/ci`.

Token usage is the histogram **`agentgateway_gen_ai_client_token_usage`**,
with `gen_ai_token_type` = `input`, `input_cache_read`, `input_cache_write`
and `output`, and the same Luma labels. **Its `input` is the whole prompt:**
`input_tokens + cache_read_input_tokens + cache_creation_input_tokens`. A
reply with 1000 input, 800 cache-read and 50 output tokens records `input`
1800, `input_cache_read` 800, `output` 50; with a 300-token cache write
added, `input` is 2100 and `input_cache_write` 300. A cache-read share is
`input_cache_read / input`.

Labels outside their fixed sets are `other`: `x-luma-person: chris`, a
missing `x-luma-tier`, `x-luma-source: <script>` and a repeated header all
are. The `request_class` set is provisional (`^[a-z_]{1,32}\z`) until Task 4
Step 5.

### Prices

The overlay prices `claude-haiku-5-5` only (`rates` 0.10 / 0.50 / 0.01 /
0.20 per MTok input / output / cache read / 1h cache write; above 100000
context tokens 0.50 / 2.50 / 0.05 / 1.00). The catalog lookup is `Exact`,
and the 1000 / 800 / 300 / 50 reply above costs
`agentgateway_gen_ai_client_cost_usd_total` 0.000193, the base rates.
`claude-sonnet-5-5` and `claude-opus-5-5` are not listed: only their
cacheWrite rates are known, and a partial entry would misprice. These
dollars are informational; the ledger is Core's (§12.4).

### The config, against the plan's sketch

`test/gateway/README.md` has the details. Four changes were needed:

1. **The prefix rewrite.** `urlRewrite: {path: {prefix: /}}` needs a prefix
   match; with the exact matches, v1.6.0 answers every request 500
   `prefix redirect requires prefix match`. Splitting each route into one
   rule per path with a full rewrite fails too: entries that share a `name`
   share a route key, and the last one wins. Each route strips its prefix
   with a CEL transformation instead,
   `":path": 'request.pathAndQuery.stripPrefix("/<route>")'`, which keeps
   the query.
2. **`__UPSTREAM_TLS__` is `{}` or `null`**, the `backendTLS` policy itself,
   not `true` or `false`. v1.6.0 has no boolean TLS switch, and
   `hostOverride` turns the provider's default TLS off. Task 26b maps
   `gateway.upstream.tls: true` to `{}`, and the deploy plan fills `{}`.
3. **`maxBufferSize: 33554432`.** The field is a byte count; `32Mi` does not
   parse.
4. **The `ai` backend has `name: anthropic`**, which v1.6.0 requires.

The CEL label patterns end in `\z` rather than `$`: the whole file, comments
included, goes through shellexpand (a comment naming a `$` variable failed
the load during the spike).

### For the live half (deploy plan Task 3)

- Fill `api.anthropic.com`, `443`, `{}`.
- Show TLS with the system roots, and that `Host` arrives as
  `api.anthropic.com`.
- The live `message_delta` usage shape decides whether `detect` keeps its
  input and cache counts (above).
