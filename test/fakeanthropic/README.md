# fakeanthropic

A scripted, recording stand-in for the Anthropic Messages API. Every offline
Luma test runs against it: Spike S, the gateway spike, the conductor's
integration tests, the evals' offline tests and the kind e2e suite. Most of
them run the real Claude Code CLI. It speaks the API's wire format, records
every request byte for byte, and checks thinking bindings the way preserved
thinking does. Replies are deterministic: no clock, no randomness.

## Running it

In Go tests, use the package directly:

```go
fake := fakeanthropic.New(fakeanthropic.Options{
    Scripts:     map[string][]fakeanthropic.Step{"weather": {{ToolUses: []fakeanthropic.ToolUse{{Name: "WebSearch", Input: map[string]any{"query": "rain"}}}}, {Text: "Rain."}}},
    DefaultText: "ok",
    BindingKey:  []byte("test-key-not-a-secret"),
})
srv := httptest.NewServer(fake) // ANTHROPIC_BASE_URL=srv.URL
// ... fake.Requests() returns every request received
```

Other tests run the binary (`go build ./test/fakeanthropic/cmd/fakeanthropic`):

```
fakeanthropic --listen 127.0.0.1:0 --scripts scripts.json --record-dir rec/ --binding-key-hex 00ff…
```

Once it accepts connections, it prints `fakeanthropic listening on <addr>` to
stdout. It logs one metadata line per request to stderr, and stops on SIGINT
or SIGTERM. The binary answers unmarked requests with
`fakeanthropic: this request carries no script marker.` and has no search
results.

| Route | Answer |
|---|---|
| `POST /v1/messages` (any query, e.g. `?beta=true`) | a scripted reply: SSE when `"stream": true`, otherwise one JSON message |
| `POST /v1/messages/count_tokens` | `{"input_tokens": n}`, n = `len(Canonical(body)) / 4`; runs validation and the binding check, uses no script step |
| anything else | 404 `not_found_error`, still recorded |

Every response carries `request-id: req_fake_<seq>`.

## Scripts

A request selects a script with the marker `[[fake:<name>]]` (name
`[A-Za-z0-9_.-]+`) in any text block of the **last user message**. A user
message that holds a `tool_result` continues the turn of the user message
before it, so every request of a tool loop carries the marker of the message
that started the turn. Messages with other roles, such as the CLI's trailing
`role: "system"` message, are skipped. The first marker found wins.

- Each request that carries a marker uses up that script's next step.
- A request without a marker gets `DefaultText` and `DefaultUsage`.
- A marker naming a missing script, or a script with no steps left, is a 400
  `invalid_request_error`: `fakeanthropic: script "x" has no step N (it has M)`.
- **A rejected request uses no step.** A request that fails parsing,
  validation (the tool_use rule) or the binding check is answered before any
  step is taken, so the CLI's strip-and-retry after a binding 400 gets the
  step.
- **Concurrent marked requests** on one script take its steps in lock order:
  the order they reach the step, after their bodies are read and checked.
  That need not be `Seq` order.

Any helper request that quotes the user's text carries the marker too, and
uses up a step.

`--scripts` takes a JSON object that maps each name to its steps. Unknown
fields are an error. Every field is optional:

```json
{
  "weather": [
    {"tool_uses": [{"name": "WebSearch", "input": {"query": "rain"}}]},
    {"text": "Rain.", "usage": {"input": 1000, "output": 50, "cache_read": 800,
                                "cache_creation": 300, "ephemeral_1h": 200, "web_search_requests": 3}}
  ],
  "busy": [{"status": 529}, {"status": 429, "retry_after": "3"}, {"text": "Back."}],
  "odd":  [{"no_thinking": true}, {"redacted_thinking": true},
           {"model": "claude-other", "stop_reason": "refusal"},
           {"input_transformations": [{"type": "thinking_dropped", "path": "messages.1.content.0", "reason": "prefix_binding_mismatch"}]},
           {"status": 400, "error_type": "invalid_request_error", "error_message": "workspace limit"}]
}
```

| Step field (Go) | Effect |
|---|---|
| `text` (`Text`) | the reply's text block. With tool uses and empty text, there is no text block. |
| `tool_uses` (`ToolUses`) | `tool_use` blocks after the text, with ids `toolu_fake_<seq>_<i>`. They make `stop_reason` `tool_use`. |
| `no_thinking` (`NoThinking`) | no thinking block. By default every reply starts with one. |
| `redacted_thinking` (`RedactedThinking`) | a `redacted_thinking` block, whose `data` is the signature, replaces the thinking block |
| `model` (`Model`) | the reply's `model` (default: the request's) |
| `stop_reason` (`StopReason`) | overrides `end_turn` / `tool_use` |
| `usage` (`Usage`) | replaces `DefaultUsage` |
| `input_transformations` | sent as the message's `input_transformations` |
| `status`, `error_type`, `error_message`, `retry_after` | any status but 0 or 200 makes the reply an error `{"type":"error","error":{type,message},"request_id"}`, with a `retry-after` header if given. Defaults: 400 `invalid_request_error`, 401 `authentication_error`, 402 `billing_error`, 403 `permission_error`, 404 `not_found_error`, 413 `request_too_large`, 429 `rate_limit_error` ("Rate limit exceeded"), 500 `api_error`, 529 `overloaded_error` ("Overloaded"). |

## Replies

- **Ids** follow the request's `Seq`, which counts every request from 1:
  `msg_fake_<seq>`, `req_fake_<seq>`.
- **Blocks:** `[thinking] [text] [tool_use…]`, in that order.
- **SSE:** `message_start`, `ping`, then `content_block_start` / deltas /
  `content_block_stop` for each block, then `message_delta`, then
  `message_stop`.
  - A thinking block starts with empty `thinking` and `signature`, and gets
    one `signature_delta`.
  - A text block gets one `text_delta`. A tool use gets one
    `input_json_delta` with the whole input.
  - A redacted thinking block and a web search result arrive whole in
    `content_block_start`.
  - `message_start` is flushed at once. `StepDelay` is the pause before each
    later event.
- **Usage:**
  - `Input`, `CacheRead` and `CacheCreation` become `input_tokens`,
    `cache_read_input_tokens` and `cache_creation_input_tokens`.
  - `cache_creation` splits the write into `ephemeral_1h_input_tokens`
    (`Ephemeral1h`) and `ephemeral_5m_input_tokens` (the rest).
  - `message_start` reports `output_tokens: 0`. `message_delta` carries the
    cumulative usage, with `output_tokens` = `Output`.
- **Web search:** a request whose `tools` include a `web_search_*` server
  tool is answered with:
  - a `server_tool_use` (id `srvtoolu_fake_<seq>_0`, `input.query` = the last
    user text);
  - a `web_search_tool_result` built from `SearchResults`. Each result's
    snippet goes where the API puts its opaque `encrypted_content`;
  - the text block.

  `usage.server_tool_use.web_search_requests` is `Usage.WebSearchRequests`,
  where 0 counts as 1. A step's `ToolUses` do not apply here.
- **`input_transformations`:** the step's list when it sets one. Otherwise
  `[]` when the request's `anthropic-beta` includes
  `thinking-binding-controls-2026-08-01`, and absent without that beta, as the
  API does. It is on `message_start.message`, or at the top level of a JSON
  reply.

## Thinking signatures and the binding check

A thinking block's signature binds it to the conversation that produced it.
Write `MAC(prefix)` for `HMAC-SHA256(BindingKey, Canonical(prefix))`, where
`prefix` is `{system, tools, messages}` up to and excluding the assistant
message being generated.

- **A block generated after no earlier thinking** is signed with
  `base64(MAC(prefix))`, 32 bytes.
- **A block generated when the request held earlier thinking** is signed
  with `base64(MAC(prefix) ‖ tag)`, 48 bytes. `tag` is the first 16 bytes of
  `HMAC-SHA256(BindingKey, "fakeanthropic chain\0" ‖ MAC(prefix) ‖ previous
  signature)`, and the previous signature is that of the last thinking block
  in the request.

On every `/v1/messages` and `count_tokens` request, every block whose
`type` is `thinking` or `redacted_thinking`, in an assistant message's
content array, is checked. The check reads the parse tree, so it fails
closed: malformed neighbours cannot hide a block.

1. **As minted.** A `thinking` block must carry `thinking: ""` (the fake
   mints no text, so any other value, or none, is an edit) and a string
   `signature`. A `redacted_thinking` block must carry a string `data`. The
   value must be base64 of 32 or 48 bytes.
2. Its MAC must match the canonical prefix of **its own message**.
3. **The chain rule:** the replayed blocks must be a contiguous window of
   the sequence they were generated in. Every block after the first one
   replayed must chain to the replayed block just before it.
   - Dropping blocks from the front, from the back, or from both is fine.
   - Removing a block from the middle, or reordering blocks, fails the block
     after the gap.
   - So does restoring a block that was stripped before a later block was
     generated.

The first failing block gets a 400 `invalid_request_error`:

- **Rule 1 (tampered):** ``messages.<i>.content.<j>: Invalid `signature` in
  `thinking` block.`` For a redacted block, ``Invalid `data` in
  `redacted_thinking` block.`` It never contains the binding substring, so
  S16 does not count it as a binding break.
- **Rules 2 and 3 (bound elsewhere):** `messages.<i>.content.<j>: ` +
  `BindingErrorText`. That is the error-mode form in the preserved-thinking
  docs, and the CLI reacts to it as it does to the real one: it strips
  thinking and retries.
  - `BindingErrorText` must always contain the one hand-written substring in
    `protocol/fixtures/binding-error.txt` (S9). Tests and the conductor
    match on that file and never re-type the text.
  - Deploy spike L5 replaces the constant with the live text.

A well-formed signature over some other prefix cannot be told apart from
random bytes of the right length, so both get the binding text.

The check is always on. With no key, the empty key signs.
`Recorded.BindingOK` is true when a request reached the check (it parsed
and passed validation) and every block passed.

## Canonicalisation

`Canonical` produces the bytes a prefix MAC covers. It is exported for tests
and Spike S. It ignores exactly what the API's binding check ignores
(Ruling S24, from the list in the preserved-thinking guide, §1.2). Each rule
has its own test in `fake_test.go`.

1. **`cache_control`**: every member is deleted, at any depth. The CLI moves
   its breakpoints every turn.
2. **A string, or a single plain text block**: in `system`,
   `messages[*].content` and `tool_result` content, an array holding one
   `{"type":"text","text":…}` block with nothing else in it (cache_control
   aside) is replaced by its string. The CLI sends its trailing
   `role: "system"` message as a block while that message is last, and as a
   string after that.
3. **Leading and trailing white space of a text block** (Go's
   `unicode.IsSpace`, raw or escaped) is trimmed. String content counts as a
   text block.
4. **Text blocks that are only white space** are dropped.
5. **Key order**: every object's keys are sorted by their decoded bytes, and
   no whitespace is left between tokens.
6. **Tools** are sorted by `name`: they are compared as a name-keyed set.
7. **Request parameters** other than `system`, `tools` and `messages` are
   left out (`model`, `max_tokens`, `thinking`, `output_config`, `stream`, …).
8. **`thinking` and `redacted_thinking` blocks** are left out of every
   prefix. The chain rule above checks them instead.

Everything else counts, byte for byte as sent: interior white space,
`tool_use.input`, tool-result text, and image and document bytes, as at the
API. Input that is not a JSON object gets rules 1 and 5 only. Invalid JSON
gives nil.

### Stricter than the API

The fake tells apart these forms, which the API treats as the same
(`prefix_diff.py` in the preserved-thinking guide).
`TestCanonicalIsStricterThanTheAPI` pins each one. None of them varies in
the CLI's traffic; if a spike shows one does, model it.

- **Numbers keep their spelling:** `1.0` is not `1`. The API collapses
  integral floats.
- **A null-valued key** is not an absent key.
- **Tool-entry defaults** spelled out: `type: "custom"`, `description: ""`,
  `strict: false`, `eager_input_streaming: false`,
  `allowed_callers: ["direct"]`.
- **`citations: []` or `null`** on a block. Such a text block also does not
  become its string (rule 2).
- **`tool_result.is_error: false`.**
- **URL-sourced images and documents:** the URL string counts. The API
  compares the bytes behind it, so a rotated URL to the same bytes matches
  there.
- **String escapes:** a `\uXXXX` escape and the raw character it stands for
  are different strings.
- **`defer_loading` tools** that nothing has named yet: the API leaves them
  out of the comparison; the fake compares them like any other tool.
- **Server-side compaction blocks:** the API restarts the comparison at the
  last one; the fake compares the whole history.

### Looser than the API, or not modelled

- **The model check.** A block is not bound to the model that produced it.
  The API drops a block the current model cannot read and answers 200, with
  a `model_binding_mismatch` entry in `input_transformations`. The fake keeps
  the block.
- **`prefix_mismatch_behavior: "drop_block"`.** Every mismatch is a 400,
  never a `thinking_dropped` entry.
- **Validation.** The fake checks only that the body is a JSON object with a
  `messages` array of objects, plus one structural rule: every `tool_use` of
  an assistant message is answered by a `tool_result` in the next message.
  The rule uses the API's wording: ``messages.<i>: `tool_use` ids were found
  without `tool_result` blocks immediately after: <ids>. Each `tool_use` block
  must have a corresponding `tool_result` block in the next message.`` It does
  not check:
  - `model` or `max_tokens` (presence, values, limits);
  - empty `messages`, role order and alternation, or where a
    `role: "system"` message may sit;
  - block shapes;
  - `anthropic-beta` values, or `block_binding` without its beta;
  - a `tool_result` with no matching `tool_use`;
  - media limits.
- **Wire behaviour not produced:**
  - a mid-stream `event: error` after a 200;
  - `pause_turn`, unless a step scripts it as its `stop_reason`;
  - `web_search_tool_result_error` and citations in search answers;
  - rate-limit headers (`anthropic-ratelimit-*`) and `x-should-retry`;
  - `thinking_delta` text and multi-delta blocks;
  - keep-alive pings during a long `StepDelay`;
  - usage derived from the prefix (usage is always scripted).

## Recording

`Requests()` returns every request in `Seq` order with:

- method;
- path with query;
- headers as net/http parsed them: names canonicalised (`Anthropic-Beta`),
  values verbatim, and `Host` moved out of the map into `Recorded.Host`;
- the body exactly as received;
- `BindingOK`.

With `RecordDir` (`--record-dir`), each request is also written as
`<seq>.body` (the raw bytes) and `<seq>.meta.json` (`seq`, `method`, `path`,
`header`, `binding_ok`, `host`), with `seq` zero-padded to six digits.

Recordings hold headers verbatim, credentials included, so use only fake
keys and never commit a recording.
