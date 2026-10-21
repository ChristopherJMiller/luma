package fakeanthropic_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherJMiller/luma/test/fakeanthropic"
)

// testKey is the fixed binding key of these tests. It is not a secret.
var testKey = []byte("fakeanthropic-test-key-not-a-secret")

// bindingFixture is the file that holds the one binding-error substring (S9).
const bindingFixture = "../../protocol/fixtures/binding-error.txt"

func start(t *testing.T, o fakeanthropic.Options) (*fakeanthropic.Server, string) {
	t.Helper()
	if o.BindingKey == nil {
		o.BindingKey = testKey
	}
	s := fakeanthropic.New(o)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts.URL
}

// response is the status and headers of a reply whose body post has read.
type response struct {
	StatusCode int
	Header     http.Header
}

// post sends body and returns the response with its body bytes. hdr is
// name/value pairs.
func post(t *testing.T, base, path, body string, hdr ...string) (response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{resp.StatusCode, resp.Header}, b
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, b)
	}
	return m
}

type sseEvent struct {
	Name string
	Data map[string]any
}

// parseSSE splits a complete event stream and checks that each event's
// `event:` name equals its data's "type".
func parseSSE(t *testing.T, b []byte) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, chunk := range strings.Split(strings.TrimSpace(string(b)), "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(chunk, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = decode(t, []byte(strings.TrimPrefix(line, "data: ")))
			default:
				t.Fatalf("unexpected SSE line %q", line)
			}
		}
		if ev.Data["type"] != ev.Name {
			t.Fatalf("event %q carries data type %v", ev.Name, ev.Data["type"])
		}
		out = append(out, ev)
	}
	return out
}

func names(evs []sseEvent) []string {
	var n []string
	for _, e := range evs {
		n = append(n, e.Name)
	}
	return n
}

// assemble rebuilds the reply's content blocks from the stream, the way the
// SDK accumulates them, so a test can echo the reply back.
func assemble(t *testing.T, evs []sseEvent) []any {
	t.Helper()
	var blocks []map[string]any
	partial := map[int]string{}
	for _, e := range evs {
		switch e.Name {
		case "content_block_start":
			blocks = append(blocks, e.Data["content_block"].(map[string]any))
		case "content_block_delta":
			i := int(e.Data["index"].(float64))
			d := e.Data["delta"].(map[string]any)
			switch d["type"] {
			case "text_delta":
				blocks[i]["text"] = blocks[i]["text"].(string) + d["text"].(string)
			case "signature_delta":
				blocks[i]["signature"] = d["signature"]
			case "input_json_delta":
				partial[i] += d["partial_json"].(string)
			default:
				t.Fatalf("unexpected delta %v", d["type"])
			}
		case "content_block_stop":
			i := int(e.Data["index"].(float64))
			if p, ok := partial[i]; ok {
				var in any
				if err := json.Unmarshal([]byte(p), &in); err != nil {
					t.Fatalf("tool input %q: %v", p, err)
				}
				blocks[i]["input"] = in
			}
		}
	}
	out := make([]any, len(blocks))
	for i, b := range blocks {
		out[i] = b
	}
	return out
}

func userText(text string) map[string]any {
	return map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}
}

func baseReq(msgs ...any) map[string]any {
	return map[string]any{
		"model":      "claude-haiku-5-5",
		"max_tokens": 1024,
		"stream":     true,
		"system":     "You are a test.",
		"messages":   msgs,
	}
}

// stream posts a streaming request and returns its events, failing on a
// non-200 status.
func stream(t *testing.T, base string, body any, hdr ...string) []sseEvent {
	t.Helper()
	resp, b := post(t, base, "/v1/messages?beta=true", mustJSON(t, body), hdr...)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("content-type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	return parseSSE(t, b)
}

func hmacB64(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func readFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(bindingFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf("%s must be one line with no trailing newline: %q", bindingFixture, b)
	}
	return string(b)
}

func TestStreamShape(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{
		DefaultText:  "Hello from the fake.",
		DefaultUsage: fakeanthropic.Usage{Input: 1000, Output: 50, CacheRead: 800, CacheCreation: 300, Ephemeral1h: 200},
	})
	body := `{"model":"claude-haiku-5-5","max_tokens":1024,"stream":true,` +
		`"system":[{"type":"text","text":"You are a test.","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"tools":[{"name":"get_time","description":"Time.","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	resp, b := post(t, base, "/v1/messages?beta=true", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	evs := parseSSE(t, b)

	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if got := names(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events\n got %v\nwant %v", got, want)
	}

	msg := evs[0].Data["message"].(map[string]any)
	if msg["id"] != "msg_fake_1" || msg["type"] != "message" || msg["role"] != "assistant" || msg["model"] != "claude-haiku-5-5" {
		t.Errorf("message_start message %v", msg)
	}
	if c := msg["content"].([]any); len(c) != 0 {
		t.Errorf("message_start content %v", c)
	}
	if msg["stop_reason"] != nil {
		t.Errorf("message_start stop_reason %v", msg["stop_reason"])
	}
	u := msg["usage"].(map[string]any)
	for k, v := range map[string]float64{"input_tokens": 1000, "cache_read_input_tokens": 800, "cache_creation_input_tokens": 300} {
		if u[k] != v {
			t.Errorf("message_start usage.%s = %v, want %v", k, u[k], v)
		}
	}
	cc := u["cache_creation"].(map[string]any)
	if cc["ephemeral_1h_input_tokens"] != 200.0 || cc["ephemeral_5m_input_tokens"] != 100.0 {
		t.Errorf("cache_creation %v", cc)
	}

	// The thinking block: empty text, then one signature over the canonical
	// prefix (typed here by hand: request parameters, cache_control and
	// whitespace gone, keys sorted, single text blocks as strings). The
	// first block of a conversation is signed with the prefix MAC alone.
	prefix := `{"messages":[{"content":"hi","role":"user"}],"system":"You are a test.",` +
		`"tools":[{"description":"Time.","input_schema":{"type":"object"},"name":"get_time"}]}`
	cb := evs[2].Data["content_block"].(map[string]any)
	if !reflect.DeepEqual(cb, map[string]any{"type": "thinking", "thinking": "", "signature": ""}) {
		t.Errorf("thinking start %v", cb)
	}
	d := evs[3].Data["delta"].(map[string]any)
	if d["type"] != "signature_delta" || d["signature"] != hmacB64(testKey, prefix) {
		t.Errorf("signature delta %v, want signature %s", d, hmacB64(testKey, prefix))
	}
	for i, idx := range []float64{0, 0, 0, 1, 1, 1} {
		if got := evs[2+i].Data["index"]; got != idx {
			t.Errorf("event %d index %v, want %v", 2+i, got, idx)
		}
	}

	if cb := evs[5].Data["content_block"].(map[string]any); !reflect.DeepEqual(cb, map[string]any{"type": "text", "text": ""}) {
		t.Errorf("text start %v", cb)
	}
	if d := evs[6].Data["delta"].(map[string]any); !reflect.DeepEqual(d, map[string]any{"type": "text_delta", "text": "Hello from the fake."}) {
		t.Errorf("text delta %v", d)
	}

	md := evs[8].Data
	if md["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Errorf("message_delta %v", md)
	}
	if md["usage"].(map[string]any)["output_tokens"] != 50.0 {
		t.Errorf("message_delta usage %v", md["usage"])
	}
}

func TestRecordsBodyBytesExactly(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "ok"})
	sent := "{ \"model\" : \"claude-haiku-5-5\",\n  \"max_tokens\": 1.0e3, \"stream\": true,\n" +
		"  \"messages\": [ {\"role\":\"user\", \"content\": \"caf\\u00e9 \u00e9 <&> \\\"q\\\"\"} ] }\n"
	beta := "claude-code-20250219,interleaved-thinking-2025-05-14, context-1m-2025-08-07"
	resp, b := post(t, base, "/v1/messages?beta=true", sent, "anthropic-beta", beta)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	recs := s.Requests()
	if len(recs) != 1 {
		t.Fatalf("%d recorded", len(recs))
	}
	rec := recs[0]
	if !bytes.Equal(rec.Body, []byte(sent)) {
		t.Errorf("body\n got %q\nwant %q", rec.Body, sent)
	}
	if got := rec.Header.Values("anthropic-beta"); !reflect.DeepEqual(got, []string{beta}) {
		t.Errorf("anthropic-beta %q", got)
	}
	if rec.Seq != 1 || rec.Method != http.MethodPost || rec.Path != "/v1/messages?beta=true" || !rec.BindingOK {
		t.Errorf("record %+v", rec)
	}
	// net/http moves Host out of the header map; the record keeps it.
	if want := strings.TrimPrefix(base, "http://"); rec.Host != want || rec.Header.Get("Host") != "" {
		t.Errorf("Host %q (header %q), want %q", rec.Host, rec.Header.Get("Host"), want)
	}
}

func TestMarkerInAnyTextBlock(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{
		DefaultText: "default",
		Scripts:     map[string][]fakeanthropic.Step{"m": {{Text: "scripted"}}},
	})
	req := baseReq(map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "first [[fake:m]]"},
		map[string]any{"type": "text", "text": "second"},
		map[string]any{"type": "text", "text": "third"},
	}})
	blocks := assemble(t, stream(t, base, req))
	if got := blocks[len(blocks)-1].(map[string]any)["text"]; got != "scripted" {
		t.Errorf("reply text %q, want the script's", got)
	}
	// A plain string content counts as one text block too, and a request
	// without a marker gets the default.
	plain := baseReq(map[string]any{"role": "user", "content": "no marker here"})
	blocks = assemble(t, stream(t, base, plain))
	if got := blocks[len(blocks)-1].(map[string]any)["text"]; got != "default" {
		t.Errorf("unmarked reply %q, want the default", got)
	}
}

func TestScriptAdvancesPerMarkedRequest(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{
		DefaultText: "default",
		Scripts: map[string][]fakeanthropic.Step{"s": {
			{ToolUses: []fakeanthropic.ToolUse{{Name: "WebSearch", Input: map[string]any{"query": "weather in Seattle"}}}},
			{Text: "It is raining."},
		}},
	})
	u1 := userText("[[fake:s]] what is the weather?")
	evs := stream(t, base, baseReq(u1))
	a1 := assemble(t, evs)
	if len(a1) != 2 {
		t.Fatalf("step 1 content %v", a1)
	}
	tu := a1[1].(map[string]any)
	want := map[string]any{"type": "tool_use", "id": "toolu_fake_1_0", "name": "WebSearch", "input": map[string]any{"query": "weather in Seattle"}}
	if !reflect.DeepEqual(tu, want) {
		t.Errorf("tool_use\n got %v\nwant %v", tu, want)
	}
	if sr := evs[len(evs)-2].Data["delta"].(map[string]any)["stop_reason"]; sr != "tool_use" {
		t.Errorf("stop_reason %v", sr)
	}

	// The tool loop's next request ends in a tool_result: it still carries
	// the turn's marker and gets step 2.
	tr := map[string]any{"role": "user", "content": []any{map[string]any{
		"type": "tool_result", "tool_use_id": "toolu_fake_1_0", "content": "Rain.",
	}}}
	a2 := assemble(t, stream(t, base, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, tr)))
	if got := a2[len(a2)-1].(map[string]any)["text"]; got != "It is raining." {
		t.Errorf("step 2 text %q", got)
	}

	// A new human turn without a marker gets the default text.
	a3 := assemble(t, stream(t, base, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, tr,
		map[string]any{"role": "assistant", "content": a2}, userText("thanks"))))
	if got := a3[len(a3)-1].(map[string]any)["text"]; got != "default" {
		t.Errorf("unmarked turn text %q", got)
	}
	for _, r := range s.Requests() {
		if !r.BindingOK {
			t.Errorf("request %d failed the binding check", r.Seq)
		}
	}
}

func TestScriptExhaustedIsAnError(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"one": {{Text: "only"}}}})
	stream(t, base, baseReq(userText("[[fake:one]]")))
	resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(userText("[[fake:one]] again"))))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	e := decode(t, b)["error"].(map[string]any)
	if e["type"] != "invalid_request_error" || e["message"] != `fakeanthropic: script "one" has no step 2 (it has 1)` {
		t.Errorf("error %v", e)
	}
	resp, b = post(t, base, "/v1/messages", mustJSON(t, baseReq(userText("[[fake:nope]]"))))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), `fakeanthropic: script \"nope\" has no step 1 (it has 0)`) {
		t.Errorf("unknown script: %d %s", resp.StatusCode, b)
	}
}

func TestErrorStatus(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"e": {
		{Status: 429, RetryAfter: "3"},
		{Status: 529},
		{Status: 400, ErrorMessage: "You have reached your specified workspace API usage limits."},
	}}})
	body := mustJSON(t, baseReq(userText("[[fake:e]]")))

	resp, b := post(t, base, "/v1/messages", body)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("retry-after") != "3" {
		t.Errorf("429: status %d retry-after %q", resp.StatusCode, resp.Header.Get("retry-after"))
	}
	if ct := resp.Header.Get("content-type"); ct != "application/json" {
		t.Errorf("content-type %q", ct)
	}
	want := map[string]any{
		"type":       "error",
		"error":      map[string]any{"type": "rate_limit_error", "message": "Rate limit exceeded"},
		"request_id": "req_fake_1",
	}
	if got := decode(t, b); !reflect.DeepEqual(got, want) {
		t.Errorf("429 body\n got %v\nwant %v", got, want)
	}
	if resp.Header.Get("request-id") != "req_fake_1" {
		t.Errorf("request-id %q", resp.Header.Get("request-id"))
	}

	resp, b = post(t, base, "/v1/messages", body)
	if resp.StatusCode != 529 || resp.Header.Get("retry-after") != "" {
		t.Errorf("529: status %d retry-after %q", resp.StatusCode, resp.Header.Get("retry-after"))
	}
	if e := decode(t, b)["error"].(map[string]any); e["type"] != "overloaded_error" || e["message"] != "Overloaded" {
		t.Errorf("529 error %v", e)
	}

	resp, b = post(t, base, "/v1/messages", body)
	if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest || e["type"] != "invalid_request_error" ||
		e["message"] != "You have reached your specified workspace API usage limits." {
		t.Errorf("400: %d %v", resp.StatusCode, e)
	}
}

// honestTurn runs one turn and returns its user message and the reply's
// content, as a client would echo them.
func honestTurn(t *testing.T, base string) (u1 map[string]any, a1 []any) {
	t.Helper()
	u1 = userText("Tell me about otters.")
	a1 = assemble(t, stream(t, base, baseReq(u1)))
	if a1[0].(map[string]any)["type"] != "thinking" {
		t.Fatalf("reply does not start with thinking: %v", a1)
	}
	return u1, a1
}

func TestBindingAcceptsHonestReplay(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "Otters hold hands."})
	u1, a1 := honestTurn(t, base)
	a2 := assemble(t, stream(t, base, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, userText("More?"))))
	// A third request replays two signed blocks, each against its own prefix.
	stream(t, base, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, userText("More?"),
		map[string]any{"role": "assistant", "content": a2}, userText("Thanks.")))
	recs := s.Requests()
	if len(recs) != 3 {
		t.Fatalf("%d recorded", len(recs))
	}
	for _, r := range recs {
		if !r.BindingOK {
			t.Errorf("request %d: BindingOK false", r.Seq)
		}
	}
}

func TestBindingRejectsEditedHistory(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "Otters hold hands."})
	u1, a1 := honestTurn(t, base)
	edited := userText("Tell me about otters!") // one character changed
	resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(edited, map[string]any{"role": "assistant", "content": a1}, userText("More?"))))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), readFixture(t)) {
		t.Errorf("body lacks the fixture text: %s", b)
	}
	e := decode(t, b)["error"].(map[string]any)
	if e["type"] != "invalid_request_error" || e["message"] != "messages.1.content.0: "+fakeanthropic.BindingErrorText {
		t.Errorf("error %v", e)
	}
	recs := s.Requests()
	if !recs[0].BindingOK || recs[1].BindingOK {
		t.Errorf("BindingOK: %v then %v", recs[0].BindingOK, recs[1].BindingOK)
	}

	// An edited system prompt or tool list breaks the binding too.
	sys := baseReq(u1, map[string]any{"role": "assistant", "content": a1}, userText("More?"))
	sys["system"] = "You are a test!"
	if resp, b := post(t, base, "/v1/messages", mustJSON(t, sys)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("edited system: status %d: %s", resp.StatusCode, b)
	}
	tools := baseReq(u1, map[string]any{"role": "assistant", "content": a1}, userText("More?"))
	tools["tools"] = []any{map[string]any{"name": "late_tool", "input_schema": map[string]any{"type": "object"}}}
	if resp, b := post(t, base, "/v1/messages", mustJSON(t, tools)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("added tool: status %d: %s", resp.StatusCode, b)
	}

	// count_tokens runs the same check.
	resp, b = post(t, base, "/v1/messages/count_tokens", mustJSON(t, baseReq(edited, map[string]any{"role": "assistant", "content": a1}, userText("More?"))))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), readFixture(t)) {
		t.Errorf("count_tokens: status %d: %s", resp.StatusCode, b)
	}
}

func TestBindingErrorTextContainsFixture(t *testing.T) {
	text, fixture := fakeanthropic.BindingErrorText, readFixture(t)
	if !strings.Contains(text, fixture) {
		t.Errorf("BindingErrorText %q lacks the fixture text", text)
	}
}

func TestNoThinkingStep(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"n": {{NoThinking: true, Text: "plain"}}}})
	evs := stream(t, base, baseReq(userText("[[fake:n]]")))
	want := []string{"message_start", "ping", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := names(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	if got := assemble(t, evs); !reflect.DeepEqual(got, []any{map[string]any{"type": "text", "text": "plain"}}) {
		t.Errorf("content %v", got)
	}
}

func TestBindingIgnoresCacheControlMoves(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "ok"})
	ephemeral := map[string]any{"type": "ephemeral", "ttl": "1h"}
	r1 := baseReq(map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "hello", "cache_control": ephemeral},
	}})
	r1["system"] = []any{map[string]any{"type": "text", "text": "You are a test.", "cache_control": ephemeral}}
	a1 := assemble(t, stream(t, base, r1))

	// The breakpoints move: off the system block and the first user turn,
	// onto the echoed reply's text block and the new user turn.
	a1[1].(map[string]any)["cache_control"] = ephemeral
	r2 := baseReq(userText("hello"), map[string]any{"role": "assistant", "content": a1}, map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "again", "cache_control": ephemeral},
	}})
	r2["system"] = []any{map[string]any{"type": "text", "text": "You are a test."}}
	stream(t, base, r2)
	if recs := s.Requests(); !recs[1].BindingOK {
		t.Errorf("moved cache_control failed the binding check")
	}
}

func TestRedactedThinkingRoundTrips(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "after", Scripts: map[string][]fakeanthropic.Step{"r": {{RedactedThinking: true, Text: "redacted"}}}})
	u1 := map[string]any{"role": "user", "content": "[[fake:r]]"}
	r1 := baseReq(u1)
	evs := stream(t, base, r1)
	want := []string{"message_start", "ping", "content_block_start", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := names(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	prefix := `{"messages":[{"content":"[[fake:r]]","role":"user"}],"system":"You are a test."}`
	wantBlock := map[string]any{"type": "redacted_thinking", "data": hmacB64(testKey, prefix)}
	if cb := evs[2].Data["content_block"].(map[string]any); !reflect.DeepEqual(cb, wantBlock) {
		t.Errorf("redacted block\n got %v\nwant %v", cb, wantBlock)
	}
	a1 := assemble(t, evs)

	stream(t, base, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, userText("next")))
	if !s.Requests()[1].BindingOK {
		t.Errorf("honest replay of redacted_thinking failed")
	}
	edited := map[string]any{"role": "user", "content": "[[fake:r]]!"}
	resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(edited, map[string]any{"role": "assistant", "content": a1}, userText("next"))))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), readFixture(t)) {
		t.Errorf("edited replay: status %d: %s", resp.StatusCode, b)
	}
}

func webSearchReq(msgs ...any) map[string]any {
	r := baseReq(msgs...)
	r["tools"] = []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}}
	return r
}

func TestServerToolAutoAnswered(t *testing.T) {
	results := []fakeanthropic.SearchResult{
		{Title: "Otter facts", URL: "https://example.test/otters", Snippet: "Otters hold hands while sleeping."},
		{Title: "Rivers", URL: "https://example.test/rivers", Snippet: "Rivers flow."},
	}
	_, base := start(t, fakeanthropic.Options{DefaultText: "Here is what I found.", SearchResults: results})
	evs := stream(t, base, webSearchReq(userText("Perform a web search for the query: otters")))
	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_stop", // thinking
		"content_block_start", "content_block_delta", "content_block_stop", // server_tool_use
		"content_block_start", "content_block_stop", // web_search_tool_result
		"content_block_start", "content_block_delta", "content_block_stop", // text
		"message_delta", "message_stop",
	}
	if got := names(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events\n got %v\nwant %v", got, want)
	}
	c := assemble(t, evs)
	wantUse := map[string]any{"type": "server_tool_use", "id": "srvtoolu_fake_1_0", "name": "web_search",
		"input": map[string]any{"query": "Perform a web search for the query: otters"}}
	if !reflect.DeepEqual(c[1], wantUse) {
		t.Errorf("server_tool_use\n got %v\nwant %v", c[1], wantUse)
	}
	wantResult := map[string]any{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_fake_1_0", "content": []any{
		map[string]any{"type": "web_search_result", "title": "Otter facts", "url": "https://example.test/otters", "encrypted_content": "Otters hold hands while sleeping.", "page_age": nil},
		map[string]any{"type": "web_search_result", "title": "Rivers", "url": "https://example.test/rivers", "encrypted_content": "Rivers flow.", "page_age": nil},
	}}
	if !reflect.DeepEqual(c[2], wantResult) {
		t.Errorf("web_search_tool_result\n got %v\nwant %v", c[2], wantResult)
	}
	if c[3].(map[string]any)["text"] != "Here is what I found." {
		t.Errorf("text %v", c[3])
	}
	md := evs[len(evs)-2].Data
	if md["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason %v", md["delta"])
	}
	stu := md["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if stu["web_search_requests"] != 1.0 {
		t.Errorf("default web_search_requests %v, want 1", stu["web_search_requests"])
	}

	// Configured: the step's usage says 3.
	_, base = start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"ws": {{Usage: &fakeanthropic.Usage{WebSearchRequests: 3}}}}})
	evs = stream(t, base, webSearchReq(userText("Perform a web search for the query: [[fake:ws]] otters")))
	stu = evs[len(evs)-2].Data["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if stu["web_search_requests"] != 3.0 {
		t.Errorf("web_search_requests %v, want 3", stu["web_search_requests"])
	}

	// And through DefaultUsage, with no marker.
	_, base = start(t, fakeanthropic.Options{DefaultUsage: fakeanthropic.Usage{WebSearchRequests: 3}})
	evs = stream(t, base, webSearchReq(userText("otters")))
	stu = evs[len(evs)-2].Data["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if stu["web_search_requests"] != 3.0 {
		t.Errorf("default-usage web_search_requests %v, want 3", stu["web_search_requests"])
	}
}

// TestServerToolOnePairPerSearch: a reply that reports N web searches holds
// N server_tool_use / web_search_tool_result pairs, as the API's does (the
// CLI counts searches from those blocks; Spike S17).
func TestServerToolOnePairPerSearch(t *testing.T) {
	results := []fakeanthropic.SearchResult{{Title: "Otter facts", URL: "https://example.test/otters", Snippet: "Otters hold hands while sleeping."}}
	_, base := start(t, fakeanthropic.Options{
		SearchResults: results,
		Scripts:       map[string][]fakeanthropic.Step{"ws": {{Text: "Found it.", Usage: &fakeanthropic.Usage{WebSearchRequests: 3}}}},
	})
	evs := stream(t, base, webSearchReq(userText("[[fake:ws]] otters")))
	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_stop", // thinking
		"content_block_start", "content_block_delta", "content_block_stop", // server_tool_use 0
		"content_block_start", "content_block_stop", // web_search_tool_result 0
		"content_block_start", "content_block_delta", "content_block_stop", // server_tool_use 1
		"content_block_start", "content_block_stop", // web_search_tool_result 1
		"content_block_start", "content_block_delta", "content_block_stop", // server_tool_use 2
		"content_block_start", "content_block_stop", // web_search_tool_result 2
		"content_block_start", "content_block_delta", "content_block_stop", // text
		"message_delta", "message_stop",
	}
	if got := names(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events\n got %v\nwant %v", got, want)
	}
	c := assemble(t, evs)
	if len(c) != 8 {
		t.Fatalf("%d content blocks, want 8: %v", len(c), c)
	}
	result := []any{map[string]any{"type": "web_search_result", "title": "Otter facts", "url": "https://example.test/otters", "encrypted_content": "Otters hold hands while sleeping.", "page_age": nil}}
	for i, id := range []string{"srvtoolu_fake_1_0", "srvtoolu_fake_1_1", "srvtoolu_fake_1_2"} {
		wantUse := map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": "[[fake:ws]] otters"}}
		if !reflect.DeepEqual(c[1+2*i], wantUse) {
			t.Errorf("block %d\n got %v\nwant %v", 1+2*i, c[1+2*i], wantUse)
		}
		wantResult := map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": result}
		if !reflect.DeepEqual(c[2+2*i], wantResult) {
			t.Errorf("block %d\n got %v\nwant %v", 2+2*i, c[2+2*i], wantResult)
		}
	}
	if c[7].(map[string]any)["text"] != "Found it." {
		t.Errorf("text %v", c[7])
	}
	stu := evs[len(evs)-2].Data["usage"].(map[string]any)["server_tool_use"].(map[string]any)
	if stu["web_search_requests"] != 3.0 {
		t.Errorf("web_search_requests %v, want 3", stu["web_search_requests"])
	}
}

func TestRefusalStep(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"no": {{
		StopReason: "refusal", Model: "claude-fallback-test", Text: "I can't help with that.",
	}}}})
	evs := stream(t, base, baseReq(userText("[[fake:no]]")))
	if m := evs[0].Data["message"].(map[string]any)["model"]; m != "claude-fallback-test" {
		t.Errorf("message_start model %v", m)
	}
	if sr := evs[len(evs)-2].Data["delta"].(map[string]any)["stop_reason"]; sr != "refusal" {
		t.Errorf("message_delta stop_reason %v", sr)
	}
}

func TestInputTransformationsEmitted(t *testing.T) {
	tf := []map[string]any{{"type": "thinking_dropped", "path": "messages.1.content.0", "reason": "prefix_binding_mismatch"}}
	_, base := start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"t": {{InputTransformations: tf}, {InputTransformations: tf}}}})
	evs := stream(t, base, baseReq(userText("[[fake:t]]")))
	want := []any{map[string]any{"type": "thinking_dropped", "path": "messages.1.content.0", "reason": "prefix_binding_mismatch"}}
	if got := evs[0].Data["message"].(map[string]any)["input_transformations"]; !reflect.DeepEqual(got, want) {
		t.Errorf("message_start input_transformations %v", got)
	}
	nonStream := baseReq(userText("[[fake:t]]"))
	delete(nonStream, "stream")
	_, b := post(t, base, "/v1/messages", mustJSON(t, nonStream))
	if got := decode(t, b)["input_transformations"]; !reflect.DeepEqual(got, want) {
		t.Errorf("non-streaming input_transformations %v", got)
	}

	// With the thinking-binding beta and nothing to report, the field is
	// an empty array; without the beta it is absent.
	evs = stream(t, base, baseReq(userText("x")), "anthropic-beta", "claude-code-20250219,thinking-binding-controls-2026-08-01")
	if got, ok := evs[0].Data["message"].(map[string]any)["input_transformations"]; !ok || !reflect.DeepEqual(got, []any{}) {
		t.Errorf("with the beta: %v (present %v), want []", got, ok)
	}
	evs = stream(t, base, baseReq(userText("x")))
	if got, ok := evs[0].Data["message"].(map[string]any)["input_transformations"]; ok {
		t.Errorf("without the beta: %v, want absent", got)
	}
}

func TestCountTokens(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"c": {{Text: "unused"}}}})
	body := `{ "model": "claude-haiku-5-5",
	  "messages": [ { "role": "user", "content": "hello there [[fake:c]]", "cache_control": {"type": "ephemeral"} } ] }`
	resp, b := post(t, base, "/v1/messages/count_tokens?beta=true", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	canonical := `{"messages":[{"content":"hello there [[fake:c]]","role":"user"}]}`
	want := map[string]any{"input_tokens": float64(len(canonical) / 4)}
	if got := decode(t, b); !reflect.DeepEqual(got, want) {
		t.Errorf("count_tokens %v, want %v", got, want)
	}
	if !s.Requests()[0].BindingOK {
		t.Errorf("count_tokens BindingOK false")
	}
	// count_tokens consumes no script step.
	a := assemble(t, stream(t, base, baseReq(userText("[[fake:c]]"))))
	if a[len(a)-1].(map[string]any)["text"] != "unused" {
		t.Errorf("script step was consumed by count_tokens: %v", a)
	}
}

func TestNonStreamingJSON(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{
		DefaultText:  "Plain JSON.",
		DefaultUsage: fakeanthropic.Usage{Input: 10, Output: 5, CacheRead: 7, CacheCreation: 3, Ephemeral1h: 3},
	})
	body := `{"model":"claude-haiku-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, b := post(t, base, "/v1/messages", body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("content-type") != "application/json" {
		t.Fatalf("status %d content-type %q: %s", resp.StatusCode, resp.Header.Get("content-type"), b)
	}
	sig := hmacB64(testKey, `{"messages":[{"content":"hi","role":"user"}]}`)
	want := map[string]any{
		"id":    "msg_fake_1",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-haiku-5-5",
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "", "signature": sig},
			map[string]any{"type": "text", "text": "Plain JSON."},
		},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                10.0,
			"cache_creation_input_tokens": 3.0,
			"cache_read_input_tokens":     7.0,
			"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": 0.0, "ephemeral_1h_input_tokens": 3.0},
			"output_tokens":               5.0,
			"service_tier":                "standard",
		},
	}
	if got := decode(t, b); !reflect.DeepEqual(got, want) {
		t.Errorf("body\n got %v\nwant %v", got, want)
	}
}

func TestMessageIDsFollowRequestSeq(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{})
	for _, want := range []string{"msg_fake_1", "msg_fake_2"} {
		evs := stream(t, base, baseReq(userText("x")))
		if id := evs[0].Data["message"].(map[string]any)["id"]; id != want {
			t.Errorf("id %v, want %s", id, want)
		}
	}
}

func TestUnknownPathIsRecordedAnd404(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{})
	resp, b := post(t, base, "/v1/models", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if e := decode(t, b)["error"].(map[string]any); e["type"] != "not_found_error" {
		t.Errorf("error %v", e)
	}
	recs := s.Requests()
	if len(recs) != 1 || recs[0].Path != "/v1/models" || recs[0].BindingOK {
		t.Errorf("records %+v", recs)
	}
}

func TestRecordDir(t *testing.T) {
	dir := t.TempDir()
	_, base := start(t, fakeanthropic.Options{RecordDir: dir})
	sent := `{"model":"m", "stream":true, "messages":[{"role":"user","content":"x"}]}`
	post(t, base, "/v1/messages?beta=true", sent, "x-luma-agent", "luma")
	got, err := os.ReadFile(filepath.Join(dir, "000001.body"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sent {
		t.Errorf("body file %q", got)
	}
	meta, err := os.ReadFile(filepath.Join(dir, "000001.meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, meta)
	if m["seq"] != 1.0 || m["method"] != "POST" || m["path"] != "/v1/messages?beta=true" || m["binding_ok"] != true ||
		m["host"] != strings.TrimPrefix(base, "http://") {
		t.Errorf("meta %v", m)
	}
	if h := m["header"].(map[string]any)["X-Luma-Agent"]; !reflect.DeepEqual(h, []any{"luma"}) {
		t.Errorf("meta header %v", m["header"])
	}
}

// canonicalCase checks Canonical on each input against one hand-typed
// output.
func canonicalCase(t *testing.T, want string, inputs ...string) {
	t.Helper()
	for _, in := range inputs {
		if got := string(fakeanthropic.Canonical(json.RawMessage(in))); got != want {
			t.Errorf("Canonical(%s)\n got %s\nwant %s", in, got, want)
		}
	}
}

// Canonicalisation, one test per rule of Ruling S24 (README
// "Canonicalisation").

func TestCanonicalRule1CacheControl(t *testing.T) {
	canonicalCase(t,
		`{"messages":[{"content":[{"text":"a","type":"text"},{"source":{"data":"AAAA","media_type":"image/png","type":"base64"},"type":"image"}],"role":"user"}],`+
			`"system":[{"text":"S","type":"text"},{"text":"T","type":"text"}],"tools":[{"input_schema":{"type":"object"},"name":"t"}]}`,
		`{"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}},{"type":"text","text":"T"}],`+
			`"messages":[{"role":"user","cache_control":{"type":"ephemeral"},"content":[`+
			`{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}},`+
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"},"cache_control":{"type":"ephemeral"}}]}],`+
			`"tools":[{"name":"t","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]}`,
	)
}

func TestCanonicalRule2StringEqualsSingleTextBlock(t *testing.T) {
	canonicalCase(t,
		`{"messages":[{"content":"hi","role":"user"},{"content":[{"content":"out","tool_use_id":"t1","type":"tool_result"}],"role":"user"},`+
			`{"content":"env","role":"system"}],"system":"S"}`,
		`{"system":"S","messages":[{"role":"user","content":"hi"},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]},{"role":"system","content":"env"}]}`,
		`{"system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out"}]}]},`+
			`{"role":"system","content":[{"type":"text","text":"env","cache_control":{"type":"ephemeral"}}]}]}`,
	)
	// Two text blocks stay blocks. (A text block carrying `citations: []`
	// also stays a block, which is stricter than the API:
	// TestCanonicalIsStricterThanTheAPI.)
	canonicalCase(t,
		`{"messages":[{"content":[{"text":"a","type":"text"},{"text":"b","type":"text"}],"role":"user"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
	)
}

func TestCanonicalRule3TrimsTextBlockEdges(t *testing.T) {
	// Raw and escaped white space at both edges goes; the interior, its
	// escapes included, stays byte for byte. The JSON escapes are built
	// from parts, so this source shows exactly the bytes sent.
	esc := func(hex string) string { return `\` + "u" + hex }
	emSpace := string(rune(0x2003)) // a raw U+2003 EM SPACE, white space
	canonicalCase(t,
		`{"messages":[{"content":[{"text":"in  side\n `+esc("00e9")+`","type":"text"},{"text":"b","type":"text"}],"role":"user"}],"system":"S"}`,
		`{"system":"  S\n","messages":[{"role":"user","content":[`+
			`{"type":"text","text":" \n\t`+esc("00a0")+` in  side\n `+esc("00e9")+` \r\n"},`+
			`{"type":"text","text":"b`+emSpace+`"}]}]}`,
	)
	canonicalCase(t, `{"messages":[{"content":"hi","role":"user"}]}`, `{"messages":[{"role":"user","content":"  hi \n"}]}`)
}

func TestCanonicalRule4DropsWhitespaceOnlyTextBlocks(t *testing.T) {
	canonicalCase(t,
		`{"messages":[{"content":"a","role":"user"},{"content":[],"role":"user"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"  "},{"type":"text","text":"a"},{"type":"text","text":"\n"}]},{"role":"user","content":" "}]}`,
	)
}

func TestCanonicalRule5KeyOrder(t *testing.T) {
	// Keys sort by their bytes, so "B" precedes "a"; whitespace goes.
	canonicalCase(t,
		`{"messages":[{"content":[{"id":"t","input":{"B":2,"a":{"y":2,"z":1},"b":1},"name":"n","type":"tool_use"}],"role":"assistant"}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{"b":1,"B":2,"a":{"z":1,"y":2}}}]}]}`,
		"{ \"messages\" : [ { \"content\" : [ { \"input\" : { \"a\" : { \"y\" : 2 , \"z\" : 1 } , \"B\" : 2 , \"b\" : 1 } ,\n"+
			" \"type\" : \"tool_use\" , \"name\" : \"n\" , \"id\" : \"t\" } ] , \"role\" : \"assistant\" } ] }",
	)
}

func TestCanonicalRule6ToolsAreANameKeyedSet(t *testing.T) {
	canonicalCase(t,
		`{"messages":[],"tools":[{"description":"A.","name":"alpha"},{"description":"B.","name":"beta"},{"max_uses":8,"name":"web_search","type":"web_search_20250305"}]}`,
		`{"messages":[],"tools":[{"name":"web_search","type":"web_search_20250305","max_uses":8},{"name":"beta","description":"B."},{"name":"alpha","description":"A."}]}`,
		`{"messages":[],"tools":[{"name":"alpha","description":"A."},{"name":"web_search","type":"web_search_20250305","max_uses":8},{"name":"beta","description":"B."}]}`,
	)
}

func TestCanonicalRule7OnlySystemToolsAndMessages(t *testing.T) {
	canonicalCase(t,
		`{"messages":[{"content":"hi","role":"user"}]}`,
		`{"model":"claude-haiku-5-5","max_tokens":5,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},`+
			`"metadata":{"user_id":"u"},"messages":[{"role":"user","content":"hi"}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"model":"claude-opus-5-5","tool_choice":{"type":"none"}}`,
	)
}

func TestCanonicalRule8ExcludesThinkingBlocks(t *testing.T) {
	canonicalCase(t,
		`{"messages":[{"content":"q","role":"user"},{"content":[{"text":"answer","type":"text"},{"id":"t","input":{"k":1.0},"name":"n","type":"tool_use"}],"role":"assistant"},`+
			`{"content":"done","role":"assistant"},{"content":[],"role":"assistant"}]}`,
		`{"messages":[{"role":"user","content":"q"},`+
			`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"c2ln"},{"type":"redacted_thinking","data":"ZGF0YQ=="},`+
			`{"type":"text","text":"answer"},{"type":"tool_use","id":"t","name":"n","input":{"k":1.0}}]},`+
			`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"c2ln"},{"type":"text","text":"done"}]},`+
			`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"c2ln"}]}]}`,
	)
}

func TestCanonicalCountsEverythingElse(t *testing.T) {
	base := `{"messages":[{"role":"user","content":"a b"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{"k":1.0}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"out"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	want := string(fakeanthropic.Canonical(json.RawMessage(base)))
	for name, edit := range map[string][2]string{
		"interior whitespace": {`"a b"`, `"a  b"`},
		"tool_use.input":      {`{"k":1.0}`, `{"k":2.0}`},
		"tool-result text":    {`"out"`, `"out!"`},
		"image bytes":         {`"AAAA"`, `"AAAB"`},
	} {
		edited := strings.Replace(base, edit[0], edit[1], 1)
		if got := string(fakeanthropic.Canonical(json.RawMessage(edited))); got == want {
			t.Errorf("%s: an edit did not change the canonical form", name)
		}
	}
}

// TestCanonicalIsStricterThanTheAPI pins the documented places where the
// fake tells apart two forms the API treats as the same (README
// "Canonicalisation", "Stricter than the API"). Each pair is equal at the
// API and different here.
func TestCanonicalIsStricterThanTheAPI(t *testing.T) {
	esc := func(hex string) string { return `\` + "u" + hex } // a JSON escape, built from parts
	msg := func(content string) string { return `{"messages":[{"role":"user","content":[` + content + `]}]}` }
	tool := func(extra string) string {
		return `{"messages":[],"tools":[{"name":"t","input_schema":{"type":"object"}` + extra + `}]}`
	}
	for name, pair := range map[string][2]string{
		"integral float": {
			`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{"k":1.0}}]}]}`,
			`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{"k":1}}]}]}`,
		},
		"null-valued key":            {msg(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"},"title":null}`), msg(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}}`)},
		"citations []":               {msg(`{"type":"text","text":"x","citations":[]}`), msg(`{"type":"text","text":"x"}`)},
		"citations null":             {msg(`{"type":"text","text":"x","citations":null}`), msg(`{"type":"text","text":"x"}`)},
		"tool_result is_error false": {msg(`{"type":"tool_result","tool_use_id":"t","content":"out","is_error":false}`), msg(`{"type":"tool_result","tool_use_id":"t","content":"out"}`)},
		"tool type custom":           {tool(`,"type":"custom"`), tool(``)},
		"tool description empty":     {tool(`,"description":""`), tool(``)},
		"tool strict false":          {tool(`,"strict":false`), tool(``)},
		"tool eager_input_streaming": {tool(`,"eager_input_streaming":false`), tool(``)},
		"tool allowed_callers":       {tool(`,"allowed_callers":["direct"]`), tool(``)},
		"rotated media URL": {
			msg(`{"type":"image","source":{"type":"url","url":"https://media.test/a.png?sig=1"}}`),
			msg(`{"type":"image","source":{"type":"url","url":"https://media.test/a.png?sig=2"}}`),
		},
		"escape vs raw character": {msg(`{"type":"text","text":"caf` + esc("00e9") + `"}`), msg(`{"type":"text","text":"caf` + string(rune(0xe9)) + `"}`)},
	} {
		a, b := fakeanthropic.Canonical(json.RawMessage(pair[0])), fakeanthropic.Canonical(json.RawMessage(pair[1]))
		if bytes.Equal(a, b) {
			t.Errorf("%s: the two forms are equal, but the README documents them as different here", name)
		}
	}
}

func TestCanonicalNonRequestAndInvalidJSON(t *testing.T) {
	// A value that is not an object gets rules 1 and 5 only.
	canonicalCase(t, `[{"a":{},"b":[1.0,1e3,true,null," x "]}]`, ` [ { "b" : [1.0, 1e3, true, null, " x "], "a" : {"cache_control": 1} } ] `)
	if got := fakeanthropic.Canonical(json.RawMessage(`{"a":`)); got != nil {
		t.Errorf("invalid JSON gave %q, want nil", got)
	}
}

// The same rules at the binding check: each re-render the API ignores
// keeps an honest replay valid.

func TestBindingIgnoresTheAPIsRerenders(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "Otters hold hands."})
	sys := []any{map[string]any{"type": "text", "text": "You are a test."}}
	r1 := baseReq(userText("Tell me about otters."), map[string]any{"role": "system", "content": []any{
		map[string]any{"type": "text", "text": "# Environment", "cache_control": map[string]any{"type": "ephemeral"}},
	}})
	r1["system"] = sys
	r1["tools"] = []any{
		map[string]any{"name": "alpha", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "beta", "input_schema": map[string]any{"type": "object"}},
	}
	a1 := assemble(t, stream(t, base, r1))

	// The CLI's own pattern (rule 2): the trailing system message goes out
	// as a string once it is no longer last. Also: the first user turn as a
	// string with edge whitespace and a whitespace-only block (rules 2–4),
	// the system as a string, the tools reordered (6), and other request
	// parameters changed (7).
	r2 := baseReq(
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": " Tell me about otters.\n"},
			map[string]any{"type": "text", "text": "  "},
		}},
		map[string]any{"role": "system", "content": "# Environment"},
		map[string]any{"role": "assistant", "content": a1},
		userText("More?"),
	)
	r2["system"] = "You are a test."
	r2["tools"] = []any{
		map[string]any{"name": "beta", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "alpha", "input_schema": map[string]any{"type": "object"}},
	}
	r2["model"] = "claude-opus-5-5"
	r2["max_tokens"] = 16
	r2["thinking"] = map[string]any{"type": "adaptive"}
	r2["output_config"] = map[string]any{"effort": "high"}
	stream(t, base, r2)
	if !s.Requests()[1].BindingOK {
		t.Errorf("an ignored re-render failed the binding check")
	}
}

func TestBindingStillCountsTheRest(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"t": {
		{ToolUses: []fakeanthropic.ToolUse{{Name: "Read", Input: map[string]any{"path": "/a"}}}},
		{Text: "read it"},
	}}})
	u1 := userText("[[fake:t]] read a")
	a1 := assemble(t, stream(t, base, baseReq(u1)))
	tr := func(out string) map[string]any {
		return map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_fake_1_0", "content": out}}}
	}
	a2 := assemble(t, stream(t, base, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, tr("file a"))))
	honest := func() []any {
		return []any{u1, map[string]any{"role": "assistant", "content": a1}, tr("file a"), map[string]any{"role": "assistant", "content": a2}, userText("thanks")}
	}
	if resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(honest()...))); resp.StatusCode != http.StatusOK {
		t.Fatalf("honest replay: %d %s", resp.StatusCode, b)
	}
	for name, edit := range map[string]func([]any){
		"tool-result text": func(m []any) { m[2] = tr("file a!") },
		"tool_use.input": func(m []any) {
			c := slices.Clone(a1)
			tu := maps.Clone(c[1].(map[string]any))
			tu["input"] = map[string]any{"path": "/b"}
			c[1] = tu
			m[1] = map[string]any{"role": "assistant", "content": c}
		},
		"interior whitespace": func(m []any) { m[0] = userText("[[fake:t]]  read a") },
	} {
		m := honest()
		edit(m)
		resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(m...)))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), readFixture(t)) {
			t.Errorf("%s: status %d: %s", name, resp.StatusCode, b)
		}
	}
}

// chainConversation runs three honest turns and returns the messages of a
// fourth request, whose assistant messages 1, 3 and 5 carry thinking
// blocks A, B and C, minted in that order.
func chainConversation(t *testing.T, base string) []any {
	t.Helper()
	msgs := []any{userText("one")}
	for _, next := range []string{"two", "three", "four"} {
		a := assemble(t, stream(t, base, baseReq(msgs...)))
		msgs = append(msgs, map[string]any{"role": "assistant", "content": a}, userText(next))
	}
	return msgs
}

// stripThinking returns msgs with the thinking block of the assistant
// message at index i removed.
func stripThinking(msgs []any, i ...int) []any {
	out := slices.Clone(msgs)
	for _, j := range i {
		m := out[j].(map[string]any)
		out[j] = map[string]any{"role": "assistant", "content": m["content"].([]any)[1:]}
	}
	return out
}

func TestBindingChainSignatures(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{DefaultText: "ok"})
	msgs := chainConversation(t, base)
	sig := func(i int) []byte {
		s := msgs[i].(map[string]any)["content"].([]any)[0].(map[string]any)["signature"].(string)
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// A had no thinking before it: the brief's signature, the prefix MAC.
	if got, want := base64.StdEncoding.EncodeToString(sig(1)),
		hmacB64(testKey, `{"messages":[{"content":"one","role":"user"}],"system":"You are a test."}`); got != want {
		t.Errorf("A = %s, want %s", got, want)
	}
	// B covers its prefix with A left out (rule 8), plus a 16-byte chain
	// tag binding it to A.
	b := sig(3)
	wantMAC, _ := base64.StdEncoding.DecodeString(hmacB64(testKey,
		`{"messages":[{"content":"one","role":"user"},{"content":"ok","role":"assistant"},{"content":"two","role":"user"}],"system":"You are a test."}`))
	if len(b) != 48 || !bytes.Equal(b[:32], wantMAC) {
		t.Errorf("B = %x (%d bytes), want its prefix MAC %x then a chain tag", b, len(b), wantMAC)
	}
}

func TestBindingChainAllowsAContiguousWindow(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "ok"})
	msgs := chainConversation(t, base)
	for name, m := range map[string][]any{
		"all kept":            msgs,
		"front dropped (A)":   stripThinking(msgs, 1),
		"front dropped (A,B)": stripThinking(msgs, 1, 3),
		"back dropped (C)":    stripThinking(msgs, 5),
		"both ends (A,C)":     stripThinking(msgs, 1, 5),
		"all dropped":         stripThinking(msgs, 1, 3, 5),
	} {
		if resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(m...))); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d: %s", name, resp.StatusCode, b)
		}
	}
	for _, r := range s.Requests() {
		if !r.BindingOK {
			t.Errorf("request %d: BindingOK false", r.Seq)
		}
	}
}

func TestBindingChainRejectsAGapOrAPutBack(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{DefaultText: "ok"})
	msgs := chainConversation(t, base)

	// B dropped from the middle: C, the block after the gap, fails.
	resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(stripThinking(msgs, 3)...)))
	if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest ||
		e["message"] != "messages.5.content.0: "+fakeanthropic.BindingErrorText {
		t.Errorf("middle gap: status %d: %v", resp.StatusCode, e)
	}

	// A reorder: C's block swapped into B's message and back fails on its
	// prefix, at the first moved block.
	swapped := slices.Clone(msgs)
	a3, a5 := msgs[3].(map[string]any)["content"].([]any), msgs[5].(map[string]any)["content"].([]any)
	swapped[3] = map[string]any{"role": "assistant", "content": []any{a5[0], a3[1]}}
	swapped[5] = map[string]any{"role": "assistant", "content": []any{a3[0], a5[1]}}
	resp, b = post(t, base, "/v1/messages", mustJSON(t, baseReq(swapped...)))
	if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest ||
		e["message"] != "messages.3.content.0: "+fakeanthropic.BindingErrorText {
		t.Errorf("reorder: status %d: %v", resp.StatusCode, e)
	}

	// A put back: a turn answered without A (stripped), then A restored.
	// The block generated while A was gone fails.
	_, base = start(t, fakeanthropic.Options{DefaultText: "ok"})
	u0 := userText("one")
	a1 := assemble(t, stream(t, base, baseReq(u0)))
	withA := map[string]any{"role": "assistant", "content": a1}
	withoutA := map[string]any{"role": "assistant", "content": a1[1:]}
	a3b := assemble(t, stream(t, base, baseReq(u0, withoutA, userText("two"))))
	resp, b = post(t, base, "/v1/messages", mustJSON(t, baseReq(u0, withA, userText("two"), map[string]any{"role": "assistant", "content": a3b}, userText("three"))))
	if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest ||
		e["message"] != "messages.3.content.0: "+fakeanthropic.BindingErrorText {
		t.Errorf("put back: status %d: %v", resp.StatusCode, e)
	}
}

func TestScriptsFileFormat(t *testing.T) {
	file := `{
	  "weather": [
	    {"tool_uses": [{"name": "WebSearch", "input": {"query": "weather"}}]},
	    {"text": "Sunny.", "no_thinking": true,
	     "usage": {"input": 10, "output": 2, "cache_read": 5, "cache_creation": 3, "ephemeral_1h": 1, "web_search_requests": 2}}
	  ],
	  "err": [{"status": 429, "error_type": "rate_limit_error", "error_message": "slow down", "retry_after": "3"}],
	  "refuse": [{"model": "claude-x", "stop_reason": "refusal", "redacted_thinking": true,
	              "input_transformations": [{"type": "thinking_dropped"}]}]
	}`
	var got map[string][]fakeanthropic.Step
	dec := json.NewDecoder(strings.NewReader(file))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string][]fakeanthropic.Step{
		"weather": {
			{ToolUses: []fakeanthropic.ToolUse{{Name: "WebSearch", Input: map[string]any{"query": "weather"}}}},
			{Text: "Sunny.", NoThinking: true, Usage: &fakeanthropic.Usage{Input: 10, Output: 2, CacheRead: 5, CacheCreation: 3, Ephemeral1h: 1, WebSearchRequests: 2}},
		},
		"err":    {{Status: 429, ErrorType: "rate_limit_error", ErrorMessage: "slow down", RetryAfter: "3"}},
		"refuse": {{Model: "claude-x", StopReason: "refusal", RedactedThinking: true, InputTransformations: []map[string]any{{"type": "thinking_dropped"}}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded\n got %+v\nwant %+v", got, want)
	}
}

// withBlock returns msgs with the assistant message at index i's first
// content block replaced by edit's result.
func withBlock(msgs []any, i int, edit func(map[string]any) any) []any {
	out := slices.Clone(msgs)
	c := slices.Clone(out[i].(map[string]any)["content"].([]any))
	c[0] = edit(maps.Clone(c[0].(map[string]any)))
	out[i] = map[string]any{"role": "assistant", "content": c}
	return out
}

func TestBindingRejectsTamperedBlocks(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "ok", Scripts: map[string][]fakeanthropic.Step{"r": {{RedactedThinking: true}}}})
	u1, a1 := honestTurn(t, base)
	honest := []any{u1, map[string]any{"role": "assistant", "content": a1}, userText("More?")}
	sig := a1[0].(map[string]any)["signature"].(string)
	raw, _ := base64.StdEncoding.DecodeString(sig)
	tamper := "messages.1.content.0: Invalid `signature` in `thinking` block."
	set := func(k string, v any) func(map[string]any) any {
		return func(b map[string]any) any { b[k] = v; return b }
	}
	del := func(k string) func(map[string]any) any {
		return func(b map[string]any) any { delete(b, k); return b }
	}
	for name, tc := range map[string]struct {
		edit func(map[string]any) any
		want string
	}{
		"thinking text edited":   {set("thinking", "x"), tamper},
		"thinking key missing":   {del("thinking"), tamper},
		"thinking not a string":  {set("thinking", 1), tamper},
		"signature missing":      {del("signature"), tamper},
		"signature not a string": {set("signature", 5), tamper},
		"signature not base64":   {set("signature", "%%%"), tamper},
		"signature too short":    {set("signature", base64.StdEncoding.EncodeToString(raw[:31])), tamper},
		// Well formed but over another prefix: the binding text.
		"signature of another prefix": {
			set("signature", base64.StdEncoding.EncodeToString(append(slices.Clone(raw[:31]), raw[31]^1))),
			"messages.1.content.0: " + fakeanthropic.BindingErrorText,
		},
	} {
		resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(withBlock(honest, 1, tc.edit)...)))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d: %s", name, resp.StatusCode, b)
			continue
		}
		e := decode(t, b)["error"].(map[string]any)
		if e["type"] != "invalid_request_error" || e["message"] != tc.want {
			t.Errorf("%s: error %v, want %q", name, e, tc.want)
		}
		if tc.want == tamper && strings.Contains(string(b), readFixture(t)) {
			t.Errorf("%s: a tampered block must not read as a binding break: %s", name, b)
		}
	}

	// The check fails closed: a malformed sibling (here a bare string in
	// the content array) does not hide the thinking block next to it.
	odd := withBlock(honest, 1, set("thinking", "x"))
	am := odd[1].(map[string]any)
	am["content"] = append([]any{"not a block"}, am["content"].([]any)...)
	resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(odd...)))
	if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest ||
		e["message"] != "messages.1.content.1: Invalid `signature` in `thinking` block." {
		t.Errorf("malformed sibling: status %d: %v", resp.StatusCode, e)
	}

	// redacted_thinking: an edited data field is tampering too.
	ur := map[string]any{"role": "user", "content": "[[fake:r]]"}
	ar := assemble(t, stream(t, base, baseReq(ur)))
	red := []any{ur, map[string]any{"role": "assistant", "content": ar}, userText("next")}
	resp, b = post(t, base, "/v1/messages", mustJSON(t, baseReq(withBlock(red, 1, set("data", "%%%"))...)))
	if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest ||
		e["message"] != "messages.1.content.0: Invalid `data` in `redacted_thinking` block." {
		t.Errorf("redacted data: status %d: %v", resp.StatusCode, e)
	}
	for _, r := range s.Requests() {
		if r.BindingOK != (r.Seq == 1 || r.Seq == 11) { // the two honest mints
			t.Errorf("request %d: BindingOK %v", r.Seq, r.BindingOK)
		}
	}
}

func TestToolUseNeedsAToolResult(t *testing.T) {
	_, base := start(t, fakeanthropic.Options{Scripts: map[string][]fakeanthropic.Step{"t": {
		{ToolUses: []fakeanthropic.ToolUse{{Name: "Read", Input: map[string]any{"path": "/a"}}, {Name: "Read", Input: map[string]any{"path": "/b"}}}},
	}}})
	u1 := userText("[[fake:t]] read two files")
	a1 := assemble(t, stream(t, base, baseReq(u1)))
	result := func(ids ...string) map[string]any {
		var c []any
		for _, id := range ids {
			c = append(c, map[string]any{"type": "tool_result", "tool_use_id": id, "content": "data"})
		}
		return map[string]any{"role": "user", "content": c}
	}
	want := "messages.1: `tool_use` ids were found without `tool_result` blocks immediately after: %s. " +
		"Each `tool_use` block must have a corresponding `tool_result` block in the next message."
	for name, tc := range map[string]struct {
		next    any
		missing string
	}{
		"no tool_result at all":   {userText("never mind"), "toolu_fake_1_0, toolu_fake_1_1"},
		"one of two answered":     {result("toolu_fake_1_1"), "toolu_fake_1_0"},
		"answered for another id": {result("toolu_other", "toolu_fake_1_1"), "toolu_fake_1_0"},
	} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			resp, b := post(t, base, path, mustJSON(t, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, tc.next)))
			if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest ||
				e["type"] != "invalid_request_error" || e["message"] != fmt.Sprintf(want, tc.missing) {
				t.Errorf("%s %s: status %d: %v", name, path, resp.StatusCode, e)
			}
		}
	}
	// A trailing tool_use with no next message is unanswered too.
	resp, b := post(t, base, "/v1/messages", mustJSON(t, baseReq(u1, map[string]any{"role": "assistant", "content": a1})))
	if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest ||
		e["message"] != fmt.Sprintf(want, "toolu_fake_1_0, toolu_fake_1_1") {
		t.Errorf("trailing tool_use: status %d: %v", resp.StatusCode, e)
	}
	// Both answered: accepted (the marker's script has no step 2, so the
	// reply is that error, after validation and the binding check pass).
	resp, b = post(t, base, "/v1/messages", mustJSON(t, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, result("toolu_fake_1_0", "toolu_fake_1_1"))))
	if e := decode(t, b)["error"].(map[string]any); resp.StatusCode != http.StatusBadRequest ||
		e["message"] != `fakeanthropic: script "t" has no step 2 (it has 1)` {
		t.Errorf("answered: status %d: %v", resp.StatusCode, e)
	}
}

func TestRejectedRequestsUseNoStep(t *testing.T) {
	s, base := start(t, fakeanthropic.Options{DefaultText: "ok", Scripts: map[string][]fakeanthropic.Step{"s": {{Text: "first"}, {Text: "second"}}}})
	u1, a1 := honestTurn(t, base)
	edited := userText("[[fake:s]] edited history")
	// A binding failure and a validation failure, both marked.
	if resp, _ := post(t, base, "/v1/messages", mustJSON(t, baseReq(userText("Tell me about otters!"), map[string]any{"role": "assistant", "content": a1}, edited))); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("binding failure: status %d", resp.StatusCode)
	}
	if resp, _ := post(t, base, "/v1/messages", mustJSON(t, baseReq(edited, map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": "toolu_x", "name": "Read", "input": map[string]any{}},
	}}))); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("validation failure: status %d", resp.StatusCode)
	}
	// The script's first step is still there.
	a := assemble(t, stream(t, base, baseReq(u1, map[string]any{"role": "assistant", "content": a1}, userText("[[fake:s]] go"))))
	if got := a[len(a)-1].(map[string]any)["text"]; got != "first" {
		t.Errorf("reply %q, want the script's first step", got)
	}
	if recs := s.Requests(); recs[1].BindingOK || recs[2].BindingOK || !recs[3].BindingOK {
		t.Errorf("BindingOK: %v %v %v", recs[1].BindingOK, recs[2].BindingOK, recs[3].BindingOK)
	}
}

func TestConcurrentRequests(t *testing.T) {
	const n = 32
	steps := make([]fakeanthropic.Step, n/2)
	for i := range steps {
		steps[i] = fakeanthropic.Step{Text: fmt.Sprintf("step %d", i+1)}
	}
	s, base := start(t, fakeanthropic.Options{DefaultText: "ok", Scripts: map[string][]fakeanthropic.Step{"c": steps}})
	type result struct {
		status int
		body   []byte
		err    error
	}
	bodies := make([]string, n)
	for i := range bodies {
		marker := ""
		if i%2 == 0 {
			marker = "[[fake:c]] "
		}
		req := baseReq(userText(marker + "hello"))
		delete(req, "stream")
		bodies[i] = mustJSON(t, req)
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for _, body := range bodies {
		wg.Go(func() {
			resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			results <- result{resp.StatusCode, b, err}
		})
	}
	wg.Wait()
	close(results)

	got := map[string]int{}
	for r := range results {
		if r.err != nil || r.status != http.StatusOK {
			t.Errorf("status %d, err %v: %s", r.status, r.err, r.body)
			continue
		}
		c := decode(t, r.body)["content"].([]any)
		got[c[len(c)-1].(map[string]any)["text"].(string)]++
	}

	recs := s.Requests()
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	for i, r := range recs {
		if r.Seq != i+1 {
			t.Errorf("record %d has Seq %d: Seqs must be distinct, 1..%d", i, r.Seq, n)
		}
	}
	// Each step went to exactly one marked request (in lock order, not Seq
	// order); the unmarked half got the default.
	want := map[string]int{"ok": n / 2}
	for i := range n / 2 {
		want[fmt.Sprintf("step %d", i+1)] = 1
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replies %v, want %v", got, want)
	}
}
