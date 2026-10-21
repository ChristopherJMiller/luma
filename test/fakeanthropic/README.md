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
| `POST /v1/messages/count_tokens` | `{"input_tokens": n}`, n = `len(Canonical(body)) / 4`; runs the binding check, uses no script step |
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

On every `/v1/messages` and `count_tokens` request, each `thinking` and
`redacted_thinking` block in an assistant message is checked:

1. Its MAC must match the canonical prefix of **its own message**.
2. **The chain rule:** the replayed blocks must be a contiguous window of
   the sequence they were generated in. Every block after the first one
   replayed must chain to the replayed block just before it.
   - Dropping blocks from the front, from the back, or from both is fine.
   - Removing a block from the middle, or reordering blocks, fails the block
     after the gap.
   - So does restoring a block that was stripped before a later block was
     generated.

A failure is a 400 `invalid_request_error` whose message is
`messages.<i>.content.<j>: ` + `BindingErrorText`. That is the error-mode
form in the preserved-thinking docs, and the CLI reacts to it as it does to
the real one: it strips thinking and retries. `BindingErrorText` must always
contain the one hand-written substring in
`protocol/fixtures/binding-error.txt` (S9). Tests and the conductor match on
that file and never re-type the text. Deploy spike L5 replaces the constant
with the live text.

The check is always on. With no key, the empty key signs.
`Recorded.BindingOK` is true when a request reached the check and every
block passed.

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
`tool_use.input` (numbers keep their spelling: `1.0` is not `1`),
tool-result text, image and document bytes, and string escapes. A
`\uXXXX` escape and the raw character it stands for are different strings
here; the CLI never varies them. Input that is not a JSON object gets
rules 1 and 5 only. Invalid JSON gives nil.

**Deliberately not modelled: the fake is stricter than the API here.**

- `defer_loading` tools that nothing has named yet. The API leaves them out
  of the comparison; the fake compares them like any other tool.
- Server-side compaction blocks. The API restarts the comparison at the last
  one; the fake compares the whole history.
- The model check: a block is not bound to the model that produced it.
- `prefix_mismatch_behavior: "drop_block"`: every mismatch is a 400, never a
  `thinking_dropped` entry.

## Recording

`Requests()` returns every request in `Seq` order: method, path with query,
headers and body exactly as received, and `BindingOK`. With `RecordDir`
(`--record-dir`), each request is also written as `<seq>.body`, the raw
bytes, and `<seq>.meta.json`, the rest, with `seq` zero-padded to six
digits.

Recordings hold headers verbatim, credentials included, so use only fake
keys and never commit a recording.
