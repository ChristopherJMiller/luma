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
are. The `request_class` set was provisional (`^[a-z_]{1,32}\z`) here; Task
4 Step 5 narrowed it to the classes Spike S saw (section S, "Contract and
gateway amendments").

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

## S. Agent SDK behaviour against the fake (Task 4; spec section 17, rows 2–4, offline)

**Question.** How do `@anthropic-ai/claude-agent-sdk` 0.3.303 and Claude Code
CLI 2.1.303 actually behave on the paths the conductor is built on: resume
and fork from a `SessionStore`, thinking bindings across them, cache TTLs,
compaction, SessionStart context, totals, settings, MCP readiness, errors,
model switches, WebFetch egress, idle behaviour, effort changes,
`input_transformations` and web-search counts?

**Answer.** Resume and fork are byte-exact and keep thinking valid, so the
idle eviction stays at 10 minutes and forks use `resumeSessionAt`.
PostToolUse carries a WebSearch call's search count
(`WEB_SEARCH_COUNT_SOURCE = 'postToolUse'`), and the CLI reads
`precomputeCompactionEnabled` (`PRECOMPUTE_SETTING_EXISTS = true`), as the
plan assumed. Three results take a branch:

- **`CONTEXT_DELIVERY = 'first-user-turn'`.** An SDK SessionStart callback
  never runs for `startup`, `resume` or `fork`. It does run for `compact`.
- **`MODEL_SWITCH_SIGNAL = 'assistantModelField'`.** The refusal fixture
  switched no model. `PostModelSwitch` fired for `setModel()` and not for a
  reply from another model; whether it fires for a flag-routed switch could
  not be tested offline.
- **`INPUT_TRANSFORMATIONS_VISIBLE = true`.** Task 16 adds the beta and Task
  19a counts dropped thinking.

Eight more facts change later tasks. They are listed under "Consequences"
below.

*Correction (Task 4 review).* The first version of this section had
`resultDelta` and `PRECOMPUTE_SETTING_EXISTS = false`, both artefacts:

- The fake answered every web search with one `server_tool_use` /
  `web_search_tool_result` pair, whatever the search count. The CLI counts a
  call's searches from those pairs, so PostToolUse said 1. The fake now sends
  one pair per search, as the API does.
- A server flag gates precompute, and it is off offline, so no request could
  show the setting working.

### What ran

- **SDK:** `@anthropic-ai/claude-agent-sdk` 0.3.303, whose `claudeCodeVersion`
  is 2.1.303. Its optional `@anthropic-ai/claude-agent-sdk-<platform>`
  packages carry the same CLI binary.
- **CLI:** the direct `@anthropic-ai/claude-code` 2.1.303 (`2.1.303 (Claude
  Code)`, Bun 1.4.4), its native `@anthropic-ai/claude-code-linux-x64` binary,
  passed as `pathToClaudeCodeExecutable` (S19). The package's `bin/claude.exe`
  is a stub until its postinstall copies the binary over it, and pnpm 10 does
  not run that script, so the conductor must point at the platform package's
  `claude` too (Task 25).
- **Harness:** `conductor/spike/` drives one long-lived `query()` per host
  over an input queue held open (#348), with Task 16's options and
  environment, the SDK's `InMemorySessionStore` with every `append` recorded,
  inline `settings`, and a fresh data dir per tree.
  - The CLI writes its cwd into the conversation, so the hosts of one tree
    share the data dir path, as the conductor's do, and "an empty local dir"
    is that path wiped.
  - Each case runs on its own (`-t` selects any one). After each case the
    harness stops its CLIs, fakes and servers and removes its temp dirs
    (transcripts, recordings, the test CA's keys); the clock shim is removed
    at the end of the run.
  - At the end, every request any case made is checked against S9's paths,
    classes and headers.
- **Fake:** the fakeanthropic binary at main, binding check on, with a fixed
  test key. Every request is recorded with its verdict, so each resume and
  fork case is also a preserved-thinking check. Since the review fix it
  answers a web search with one `server_tool_use` / `web_search_tool_result`
  pair per search.
- **Command:** `nix develop -c make sdk-spike`, 2026-10-21, on rowlett (Linux
  6.18, Node 24.21, vitest 5.0.5). Output:

  ```
  ✓ runs the pinned CLI 357ms
  ✓ S29: CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1 stops the CLI at start when bubblewrap is not on PATH 121ms
  ✓ S1: request 3 after a store resume equals the unbroken run outside metadata, thinking accepted 1457ms
  ✓ S1b: a resume a day later keeps the date line in the prefix and notes the new date at the tail 1258ms
  ✓ S2: every session of a tree writes under projectKey = the root 819ms
  ✓ S3: main requests cache for 1h; subagent requests for the default 5m; helper requests not at all 469ms
  ✓ S14: the CLI compacts at COMPACTION_TRIGGER_TOKENS of context, not one token before 913ms
  ✓ S4: compact_boundary, SessionStart(compact) context in messages, and one binding retry 512ms
  ✓ S5: a SessionStart callback never runs for startup, resume or fork, so its context reaches no request 1101ms
  ✓ S6: totals run within a query, are restored from cost-state entries, and give non-negative deltas 2349ms
  ✓ S7: inline settings reach the CLI as its --settings flag (the flag settings layer) 852ms
  ✓ S7: the CLI reads precomputeCompactionEnabled (default on) behind a server flag, so offline neither value changes a request 14161ms
  ✓ S8: a fork at a message, from a second host reading only the store, replays the kept turn byte for byte 806ms
  ✓ S9: a resume after an idle longer than the cache TTL looks exactly like a warm one 1597ms
  ✓ S9: one conversation sends every request class, on one path, with our headers and system blocks 613ms
  ✓ S10: polling mcpServerStatus() until connected means the first request lists the memory tools 1441ms
  ✓ S11: what the SDK reports for a 429, a 529 and a workspace-limit 400 2229ms
  ✓ S11: a binding 400 is invisible to the SDK: the CLI strips the thinking and retries 871ms
  ✓ S11: the subagent tool is Agent, and its subagent runs in the background unless CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1 1074ms
  ✓ S11: the refusal fixture switches no model; a reply from another model fires no PostModelSwitch; setModel() does 581ms
  ✓ S12: the page and a domain check go through the proxy; the extraction request goes to the fake on the helper model 647ms
  ✓ S12: a denied domain check fails the WebFetch, unless skipWebFetchPreflight 928ms
  ✓ S13: an idle hour after six 4 KB tool results changes no earlier message, with or without idleCompaction: false 11509ms
  ✓ S15: applyFlagSettings({effortLevel: "medium"}) changes output_config and the new turn tail only 438ms
  ✓ S16: with the thinking-binding beta, input_transformations reach the SDK messages 447ms
  ✓ S17: a WebSearch call whose nested request runs 3 searches reports 3 in PostToolUse 489ms
  Tests  26 passed (26)    Duration  48.71s
  ```

The values are committed in `conductor/src/spike-results.ts`, each typed by
hand from these observations. Every case asserts that its observation equals
its constant, so these are change detectors (S20).

### Clock control

The plan named libfaketime (`LD_PRELOAD=$LUMA_FAKETIME_LIB`, `FAKETIME=…`).
The flake's libfaketime 0.9.11 **hangs the CLI at start**, with every
`FAKETIME` value and with `libfaketimeMT` too. One thread waits in
`futex_wait` before `main`. The cause: libfaketime's lazy init calls `dlsym`.
That allocates through the CLI's own allocator (Bun's), which reads the
clock, which re-enters libfaketime's init under `pthread_once`. Its source
says as much ("If ftpl_init ends up recursing, pthread_once will deadlock").

The harness builds its own shim instead (`CLOCK_SHIM_C` in `harness.ts`).
The dev shell's `cc` compiles it once per run.

- It adds an offset to `CLOCK_REALTIME`, `gettimeofday` and `time`. With
  `monotonic`, it also moves the monotonic clocks, as `FAKETIME` does.
- The offset is read from a file mapped shared, so a running CLI's clock can
  be moved. The shim maps it in a constructor, before the CLI's own
  initialisers run and before any thread exists.
- It makes raw syscalls only, so it never allocates. It uses no glibc symbol
  newer than `syscall`, so it loads into the CLI's own glibc, which is not
  the flake's.

`LUMA_FAKETIME_LIB` stays in the flake, unused.

### Results

**S1, resume (`IDLE_EVICT_MS = 600_000`).**
- Run A: three turns. Run B: two turns, `close()`, the local JSONL deleted, a
  store `resume`, turn three.
- B's third main request equals A's **byte for byte, outside
  `metadata.user_id`**. Every request passed the binding check.
- `metadata.user_id` is `{device_id, account_uuid: "", session_id}`.
  `session_id` is the session's; `device_id` lives in the config dir's
  `.claude.json`. So A and B differ there because they are two sessions, and
  B's resumed request has the same `metadata` as B's second.
- The SDK does not resume in the host's `CLAUDE_CONFIG_DIR` (S34). It loads
  the store into a temp dir, `<SDK process tmpdir>/claude-resume-<uuid>`,
  under `projects/<projectKey>/<sid>.jsonl`. It runs the CLI with
  `CLAUDE_CONFIG_DIR` pointing at it, and deletes it after `close()`; the
  case asserts each of these.
- The SDK's resume code (sdk.mjs) also copies the config dir's
  `.credentials.json`, `.claude.json` and `.config.json` there when present.
  Here only `.claude.json` exists (auth is `ANTHROPIC_AUTH_TOKEN`), and the
  case pins the dir's listing to `.claude.json`, `projects` and `sessions`.
  A `.credentials.json` in a person's config dir would be copied into
  `TMPDIR` too.
- `system[0]` is `x-anthropic-billing-header: cc_version=2.1.303.<3 hex>;
  cc_entrypoint=sdk-ts;`. The 3 hex digits follow the conversation's first
  user message: they are stable within a conversation, and change when a
  compaction replaces that message.

**S1b, a day later.**
- The first turn's environment block, a `role: "system"` message right after
  U1, ends `Today's date is <day 0>.`. A resume under a clock one day ahead
  restores that line unchanged.
- The CLI adds `\n\nThe date has changed. Today's date is now <day 1>. No
  need to announce the new date — the user's own clock shows it.` to the
  trailing system message. That message comes after the previous request's
  cached prefix.
- Every other byte equals A's, and thinking passes. The case runs the CLI at
  06:00 UTC by clock offset, so it never crosses midnight.

**S2, project key.** With `CLAUDE_CODE_PROJECT_DIR_NAME=<root>` and cwd
`conv/<root>`, a session and its fork append only under
`{projectKey: <root>, sessionId}`, with no subpath. The fresh session's
local copy is `config/<ref>/projects/<root>/<sid>.jsonl`. There are exactly
two requests, one per turn, and both pass the binding check.

**S3, cache TTLs.** The beta header on main requests includes
`extended-cache-ttl-2025-04-11`.

| Request (`x-claude-code-request-class`) | Model | `cache_control` |
|---|---|---|
| `main` | session | every marker `{"type":"ephemeral","ttl":"1h"}` (three) |
| `subagent` (deep-think) | the agent's (`claude-opus-5-5`) | `{"type":"ephemeral"}`: the 5m default |
| `compaction` | session | `{"type":"ephemeral"}` |
| `auxiliary` (WebSearch's nested request, the WebFetch extraction) | session / helper | none |

**S4 and S14, compaction (`COMPACTION_TRIGGER_TOKENS = 66_965`).**
- A binary search over the fourth reply's `input_tokens`, with window 100000
  and pct 85, found the trigger at 66965. The case checks 66964 (no
  compaction) and 66965 (compaction).
- The count is the last response's `input + cache_read + cache_creation +
  output`. Splitting 66965 across those fields compacts the same.
- The next turn's own size counts toward it: a 40000-character fifth message
  compacted at 60000.
- With only one earlier turn, the compaction fails (`compact_result:
  "failed", compact_error: "too_few_groups"`), so the probe runs four turns
  first.

After the compaction:

- The compaction request is class `compaction` on the session model, with
  header `x-claude-code-compaction: auto` and 5m cache markers.
- The SDK emits `system/compact_boundary` (`trigger: "auto"`, with
  `preserved_messages`).
- `SessionStart` fires once, with `source: "compact"`. Its
  `additionalContext` lands **in `messages`**, in the trailing system message
  of the first post-compaction turn, exactly once, and stays there.
- The main requests of that turn carry `x-claude-code-context-compacted:
  auto`; later turns do not. `SLO_SIGNALS.postCompaction =
  'compact_boundary'`.
- **The first main request after a compaction fails the binding check.**
  Compaction keeps the last turn verbatim after the summary, its signed
  thinking included. That thinking was bound to the pre-compaction prefix.
  The fake answers 400, as the API would, and the CLI strips the thinking and
  retries; the retry and later requests pass. The plan expected that request
  to pass.

**S5, SessionStart (`CONTEXT_DELIVERY = 'first-user-turn'`,
`SESSION_START_CONTEXT_SOURCES = ['compact']`).**
- A callback hook with matcher `startup|compact`, plus a recorder on every
  source, never ran for a fresh session, a store resume or a fork. Its context
  reached no request.
- With `--await-initialize` forced (`extraArgs`, as a diagnostic only), it
  still did not run. The CLI's debug log shows `session.start` raised before
  the SDK's hooks are in place.
- A settings-file `command` hook does run for `startup`. It needs a shell in
  the image, so it is not an option for the conductor.

**S6, totals (`TOTALS_ENTRY_TYPE = 'cost-state'`, `COST_BASIS_FIELD =
'modelUsage[model].costBasis'`).** Turn k reported 1000·k input and 100·k
output tokens.

| Case | First result's tokens (in/out) | Store's latest `cost-state` at open | Delta = result − that |
|---|---|---|---|
| turns 1, 2 in one query | 1000/100, then 3000/300 (running) | none | — |
| clean-close resume, turn 3 | 6000/600 | 3000/300 | 3000/300 |
| SIGKILL after turn 4 (10000/1000), resume, turn 5 | 8000/800 | 3000/300 | 5000/500 |
| fork at turn 1 from an empty dir, turn 6 | 14000/1400 | 8000/800 (the source's latest) | 6000/600 |
| killed before any clean close, resume, turn 2 | 2000/200 | none | 2000/200 |

- A clean close appends `{type: "cost-state", sessionId, totalCostUSD,
  totalAPIDuration, …, modelUsage: {<model>: {inputTokens, outputTokens,
  thinkingTokens, cacheReadInputTokens, cacheCreationInputTokens,
  webSearchRequests, costUSD}}, hasUnknownModelCost}`. Results are running
  totals within a query.
- A resume or a fork restores the latest `cost-state` of the transcript it
  loads. A SIGKILL writes none, so `TOTALS_RESTORED = {resume: true,
  resumeAfterKill: false, fork: true}`.
- **The baseline rule that gives exact, never-negative deltas:** the latest
  `cost-state` entry of the loaded transcript, or zero when there is none.
- Core's last persisted totals would be wrong as a fallback. After the kill
  it gives −2000/−200, and for the session killed before any clean close it
  undercounts by 1000/100.
- `modelUsage[<model>].costBasis` is `"list"` on every result. It is per
  model, and absent until the process has priced a request for that model.
- The eight turns made exactly eight requests, all passing the binding
  check: no resume or fork here cost a strip-and-retry.

**S7, settings (`PRECOMPUTE_SETTING_EXISTS = true`).**
- The SDK passes inline `settings` as the CLI's `--settings
  {"switchModelsOnFlag":true,"idleCompaction":false,"precomputeCompactionEnabled":false}`,
  the flag settings layer. The case reads it from `/proc/<pid>/cmdline`, next
  to `--setting-sources=`, `--strict-mcp-config` and
  `--permission-mode=default`.
- **`precomputeCompactionEnabled` exists and defaults to true.** It is
  declared in the SDK's `Settings` type, and the CLI reads it as
  `bs("precomputeCompactionEnabled", !0)`.
- **Precompute is gated by the server flag `tengu_sepia_moth`** (default off):
  `if(!k("tengu_sepia_moth",!1))return!1`. The flag also gates the idle
  compaction mode `compact_on_return`, whose check returns
  `"precompute_compaction_off"` when the setting is false. The case asserts
  the binary holds `"precomputeCompactionEnabled",!0)`, `"idleCompaction",!0)`
  and `"tengu_sepia_moth",!1)`.
- Offline the flag is off, so with `true` or `false` the requests below the
  threshold (63965) and at it are the same classes, with a 3 s pause before
  the fifth turn, and no precompute request appears. This shows the gate, not
  the setting's effect: Task 16 keeps `precomputeCompactionEnabled: false`.
- Neither setting's effect is shown offline. S11's refusal switched nothing,
  so `switchModelsOnFlag` went unexercised. S13 cannot observe
  `idleCompaction` (below).

**S8, fork (`FORK_STRATEGY = 'resumeSessionAt'`).**
- The source turn: U1, then A1 (thinking + a `WebSearch` tool use, its
  result, thinking + text), then U2 and A2.
- A second host, after its data dir was wiped, forked with `resume` +
  `forkSession` + `resumeSessionAt` = the uuid of turn 1's last main-loop
  assistant message, and sent U2'.
- The fork's request is `[U1, env, A1(thinking, tool_use), tool_result,
  system, A1(thinking, text), U2', system]`. Every message before U2' is byte
  for byte the original's, and the binding check passes.
- The fork gets a new session id, and its store key holds the whole kept
  history.

**S9, headers, paths and system blocks.** One case runs a conversation that
makes every kind of request (a subagent call, a web search, four turns, a
compaction) and asserts the exact sets. A check after the run then holds
every request of every case (more than 100 in a full run) to the same rules:

- **Paths:** `CLI_PATHS = ['/v1/messages']`; every request was `POST
  /v1/messages?beta=true`. No `count_tokens` request occurred.
- **Classes:** `REQUEST_CLASSES = ['auxiliary', 'compaction', 'main',
  'subagent']`.
- **Auth and identity:** `Authorization: Bearer <ANTHROPIC_AUTH_TOKEN>`, no
  `x-api-key`. `x-luma-person|agent|tier|source` from
  `ANTHROPIC_CUSTOM_HEADERS` reached every request. `User-Agent: claude-cli/2.1.303
  (external, sdk-ts, agent-sdk/0.3.303, client-app/luma)`, and `x-client-app: luma`.
- **System blocks:** `system[0]` is the billing header on every request. On
  main requests, `system[1]` is "You are a Claude agent, built on Anthropic's
  Claude Agent SDK." and `system[2]` is the agent body, byte for byte.

**Cold resume (`SLO_SIGNALS.coldResume = null`).**
- A resume under a clock 70 minutes ahead sends a body equal to a warm
  resume's outside `metadata`, with the same header names.
- No SDK message carries anything "expired". The SessionStart input that
  would carry `prompt_cache_likely_expired` never runs (S5).
- The conductor derives a cold resume from the idle gap (Task 19b).

**S10, MCP.**
- The local HTTP server held `initialize` for 1 s. Polling
  `mcpServerStatus()` every 100 ms took 12 polls to see `connected`.
- The first request then listed `mcp__luma__forget` and
  `mcp__luma__remember`, without `defer_loading`. In an exploratory run
  without the poll, the CLI also waited for this server.
- The server's `instructions` travel in the first turn's environment block,
  in `messages`, not in `system`. The CLI also sends `server/discover` first;
  a -32601 answer is fine.

**S11, errors.** With `CLAUDE_CODE_MAX_RETRIES=1` to keep the case short:

| Upstream | `system/api_retry` | assistant `error` | assistant text | result |
|---|---|---|---|---|
| 429 | `rate_limit`, 429 | `rate_limit` | `API Error: Request rejected (429) · Rate limit exceeded` | `success`, `is_error: true`, `api_error_status: 429`, `terminal_reason: api_error` |
| 529 | `overloaded`, 529 | `server_error` | `API Error: 529 Overloaded. This is a server-side issue, usually temporary — …` | the same, 529 |
| 400 workspace limit | none | `unknown` | `API Error: 400 You have reached your specified workspace API usage limits. …` | the same, 400 |
| binding 400 (a tampered stored U1) | none | none | — | `success`, `is_error: false` |

- The result subtype is `success` in every case, so the conductor maps on
  `is_error`, `api_error_status` and the text (Task 19a).
- A 400 other than a binding 400 is sent a second time without the
  `thinking-display-updates-2026-08-18` beta and without `thinking.display`.
- The binding 400 never reaches the SDK: the CLI strips every thinking block
  and retries, and only the gateway sees the 400.
- With the default retries, an exploratory 429 run made 11 requests over
  180 s before the error surfaced, ignoring `retry-after: 0`.

**S11, subagent (`SUBAGENT_TOOL = 'Agent'`).**
- The API tool is `Agent`. Its input schema has `subagent_type`,
  `description`, `prompt`, `model` (enum `sonnet|opus|haiku|fable`),
  `effort`, `run_in_background` and `isolation`.
- The subagent request is class `subagent` on the agent's model and effort,
  with `cc_is_subagent=true;` in its billing header.
- **By default the subagent runs in the background.** The tool result is
  "Async agent launched successfully…", the turn ends, and the task
  notification later starts a turn of its own that no user sent.
- `background: false` in the agent definition does not change that.
  `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1` does: the subagent runs in the
  foreground, the tool result is its report ("[Subagent hand-back] …"), and
  there is one result.

**S11, refusal (`MODEL_SWITCH_SIGNAL = 'assistantModelField'`).**
- With `switchModelsOnFlag: true`, a `stop_reason: "refusal"` reply switches
  nothing. The CLI emits `system/informational` "Sonnet 5.5's safeguards
  stopped the response above · continuing once with that noted", adds a
  synthetic user message (`isSynthetic`, `refused_message_id`), and retries
  once on the same model.
- No `model_refusal_fallback` message appears, and no `Pre`/`PostModelSwitch`
  hook fires. The fake cannot send the refusal category a fallback is routed
  on.
- A reply from another model shows only as the assistant message's
  `message.model`, and fires no `PostModelSwitch`. `modelUsage` stays keyed
  by the requested model.
- `PostModelSwitch` fires for `setModel()` (`source: "sdk"`), which Luma
  never calls.
- **Untested:** whether `PostModelSwitch` fires for a switch the CLI makes on
  a flagged reply. The fake cannot trigger one, so the reply's model is the
  signal.
- **For the live spike:** the API may answer an alias with a dated snapshot
  id. The `message.model` comparison must not count that as a switch.

**S12, WebFetch through a CONNECT proxy.** With `HTTPS_PROXY` set,
`NO_PROXY=127.0.0.1,localhost` and `NODE_EXTRA_CA_CERTS` = the test CA:

- **CONNECTs:** `api.anthropic.com:443`, then `site.test:443`, and none to
  the fake.
- **The first CONNECT is a domain check:** `GET
  /api/web/domain_info?domain=site.test`, once per session.
- **The extraction request** reached the fake as class `auxiliary` on the
  helper model, with `effort: medium` and no cache markers.
- **A proxy that refuses `api.anthropic.com` fails the WebFetch:** "Unable
  to verify if domain site.test is safe to fetch. This may be due to network
  restrictions or enterprise security policies blocking claude.ai." Settings
  `skipWebFetchPreflight: true` skips the check.

**S13, idle.**
- Six WebFetch results of 5 KB each, then the CLI's clocks, monotonic
  included, jumped 65 minutes ahead while it idled for 5 s, then another
  turn.
- With `idleCompaction: false`, and with the key absent, the CLI made no
  request while idle. The next request's earlier messages equal the previous
  request's (apart from cache markers and the old trailing system message
  becoming a string), and every other field is unchanged.
- **This is not a regression guard for `idleCompaction: false`.** The CLI
  decides idle compaction on a server-side mode first, returning
  `"disabled"` while that mode is `"off"`. Only after that does it check
  `idleCompaction` (`"idle_compaction_off"`) and, for the `compact_on_return`
  mode, `precomputeCompactionEnabled`. Offline the mode stays off (neither
  run compacted), so this case cannot tell the two settings apart.
- The case records only that nothing else rewrites the history across an
  idle hour. The settings stay as the plan has them.

**S15, `applyFlagSettings({effortLevel: 'medium'})` mid-session.** The next
request changes in two places only:

- top-level `output_config` goes from `{"effort":"low"}` to
  `{"effort":"medium"}`;
- the new turn's trailing `role: "system"` message carries
  `output_config: {"effort":"medium"}`. The first turn's environment message
  carries the initial `{"effort":"low"}` the same way.

Earlier messages, the beta header and the thinking bindings are unchanged.
Whether the API keeps the cache across that is live spike L4's question.

**S16 (`INPUT_TRANSFORMATIONS_VISIBLE = true`).**
- With `ANTHROPIC_BETAS=thinking-binding-controls-2026-08-01`, the beta joins
  every request's `anthropic-beta`.
- A reply's `input_transformations` show on every assistant message's
  `message.input_transformations` and on the `stream_event` `message_start`.

**S17 (`WEB_SEARCH_COUNT_SOURCE = 'postToolUse'`).**
- WebSearch runs a nested request: class `auxiliary`, the session model,
  `tools: [{type: "web_search_20250305", name: "web_search", max_uses: 8}]`.
- The nested reply held three `server_tool_use` / `web_search_tool_result`
  pairs, as the API sends one per search, and reported
  `web_search_requests: 3`.
- PostToolUse's `tool_response` is `{query, results, durationSeconds,
  searchCount}`, with `searchCount: 3`. The CLI counts it as the larger of the
  two block counts, per call, so the hook can count searches as they run. The
  result's `modelUsage[model].webSearchRequests` delta agrees.

### Environment

- **`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`** makes the CLI refuse to start
  when bubblewrap is not on `PATH`: "bubblewrap is required for subprocess env
  scrubbing and isolation". The S29 case pins this with an empty `PATH`; the
  harness leaves the variable out.
- **A host must end its input and wait for the stream to end before it
  reports closed.** In an exploratory run, `query.close()` called while the
  input was still open returned before the CLI's graceful exit had written
  its `cost-state` to the store. A resume opened at once loaded the
  transcript without it, and the entry landed afterwards. The harness's
  `close()` ends the input, waits for the stream to end, then calls
  `close()`; S6 resumes straight after it and found the entry every time.

### Contract and gateway amendments (Step 5)

Applied to `docs/plans/phase-1-lean-chat.md`:

- **Global Constraints, web tools:** S17 found `postToolUse`, so the bound
  is the plan's own: one call of up to `max_uses: 8` searches after two
  counted ones, **10 searches a turn ($0.10)** and **22 a session ($0.22)**.
  The first version of this section restated it for `resultDelta`; the
  review fix reverted that.
- **C2:** `HostSpec.resume.baseline_totals` stays optional, because
  `TOTALS_ENTRY_TYPE` is not null. A note records that it is never the delta
  baseline (S6). `turn_result.cost_basis` stays, because `COST_BASIS_FIELD`
  is not null. `MODEL_SWITCH_SIGNAL` and `CONTEXT_DELIVERY` change nothing in
  C2 or Core.
- **C4:** unchanged.
- **C7:** S1 passed, so `luma_conductor_resume_prefix_changed_total` is not
  added, and the idle default stays 10. `luma_conductor_thinking_dropped_total`
  is no longer conditional, because S16 shows `input_transformations`.

The gateway config (`test/gateway/agentgateway.yaml`):

- **Routes unchanged.** `CLI_PATHS` is one of the two documented paths.
  `count_tokens` stays routed for `getContextUsage({detail: 'full'})`.
- **`request_class`'s provisional `^[a-z_]{1,32}\z` became the exact set**
  `v in ["main", "auxiliary", "compaction", "subagent"]`.
- `TestUnknownLabelValuesBecomeOther` checks the four kept and `title`,
  `Main` mapped to `other`.
- `nix develop -c make gateway-spike` then passed every test again
  (217.7 s, 2026-10-21).

### Consequences for later tasks

Not applied here; they need the plan's owner:

1. **Task 16 `buildEnv`.**
   - Drop `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`, or install bubblewrap in the
     image. With no shell or file tools there is no subprocess to scrub.
   - Add `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1`, or a background deep-think
     ends the turn early and starts turns Core never sent.
   - Add `ANTHROPIC_BETAS=thinking-binding-controls-2026-08-01` (S16).
2. **Task 16 options.** Keep `precomputeCompactionEnabled: false` (S7: the
   key exists, and defaults to true).
3. **Task 17/19b baseline.** The baseline is the latest `cost-state` of the
   loaded transcript, else zero. It is never `spec.resume.baseline_totals`:
   that gives negative deltas after a SIGKILL, and undercounts when a session
   never closed cleanly.
4. **Task 18.**
   - The SessionStart hook is off (`first-user-turn`). Its `compact` source
     does work, if Task 19b prefers it for re-injection.
   - The subagent lock must also refuse `run_in_background: true`, unless
     item 1's env var is set.
   - Search counting from PostToolUse's `tool_response.searchCount`
     (`postToolUse`), so Task 18's 8-search test runs.
5. **Task 19a errors.** Every API failure is a `success` result with
   `is_error: true`. Map on `api_error_status` and the text (429 →
   `rate_limited`, 529 → `overloaded` via status, not via the assistant's
   `server_error`). The binding 400 never reaches the SDK.
6. **Task 19b paths.**
   - A resumed CLI's local transcript lives in the SDK's
     `claude-resume-<uuid>` temp dir under the conductor's `TMPDIR`, not in
     `config/<ref>/projects/<root>`. The SDK deletes it on `close()`. P18's
     cleanup of `<sdk_session_id>.jsonl` applies to fresh sessions only.
   - `TMPDIR` (an in-memory emptyDir) must have room for the largest
     transcript.
7. **Deploy plan and Task 26b, alerts.** Each auto-compaction costs one
   main-class binding 400 (S4), and every other 400 is sent twice (S11). The
   "main-class 400" thinking alert will fire on compactions unless it allows
   for them. The 400 carries `x-claude-code-context-compacted: auto`, which
   the metrics do not label today.
8. **Task 26b smokescreen.**
   - WebFetch needs `api.anthropic.com:443` through the egress proxy for its
     domain check, or `skipWebFetchPreflight: true` (S12). Otherwise every
     WebFetch fails.
   - The check sends each fetched domain to Anthropic, which already sees the
     URL.

## R. Usage reconcile: the Admin API (Task 14b, P22, S12)

Read from the Usage and Cost Admin API documentation (platform.claude.com, manage-claude/usage-cost-api) on 2026-10-22. Offline: the tests use a fake with hand-written fixtures (`core/internal/reconcile/testdata/`); no real Admin key was used. The first live run (deploy plan) is the check that the shapes below hold.

- **Auth:** an Admin API key (`sk-ant-admin01-...`) as `x-api-key`, plus `anthropic-version: 2023-06-01`. Workspace keys do not work. The key comes from `LUMA_ANTHROPIC_ADMIN_KEY_FILE`, is read per run, and appears in no log, error or request to Core.
- **Workspaces:** `GET /v1/organizations/workspaces?limit=1000[&after_id=<last_id>]` returns `{data:[{id, name, archived_at, ...}], has_more, first_id, last_id}`. Names match exactly (`luma-interactive`, `luma-ci`); a missing name fails the run. Archived workspaces are not listed by default. The docs page names the endpoint but not its cursor; `after_id`/`last_id` is the Admin API list convention (verify on the first live run).
- **Cost:** `GET /v1/organizations/cost_report?starting_at=<day>T00:00:00Z&ending_at=<day+1>T00:00:00Z&bucket_width=1d&group_by[]=workspace_id[&page=<next_page>]` returns `{data:[{starting_at, ending_at, results:[{currency:"USD", amount:"<decimal string>", workspace_id, description, ...}]}], has_more, next_page}`. Daily buckets are the only granularity.
- **Unit:** `amount` is a decimal string **in cents** (docs: "USD, reported as decimal strings in lowest units (cents)"); the client sums a workspace's results and divides by 100. Any currency but USD fails the run. `workspace_id` is `null` for the default workspace (ignored). Priority Tier cost is not in this report; Luma does not use Priority Tier.
- **Grouping:** by `workspace_id`, not the spec's `api_key_id` (S23): the cost report segments only by workspace.
- **Freshness:** data appears within about 5 minutes; the CronJob runs at 07:30 UTC for the previous UTC day.
- **Core side:** `POST /internal/v1/usage-reconciliations` (C10). `drift_usd = ledger_usd - api_usd`, `drift_ratio = drift_usd / max(api_usd, 0.01)`. Gauges `luma_usage_reconcile_drift_usd{workspace}` and `luma_usage_reconcile_drift_ratio{workspace}` follow the newest day stored per workspace; `luma_usage_reconcile_last_success_timestamp_seconds` is set on each accepted post. All three are restored from the DB at start (S7). The alert (ratio > 0.10 **and** |drift_usd| > 0.50 per workspace-day, Chris) lives in the deploy plan's Task 9.
- **Exit and retries:** non-zero on any failure. Core's 503 (S43) is retried at 1, 2, 4, 8, 16 s (or a longer `Retry-After`), five retries; any other non-2xx fails at once.
