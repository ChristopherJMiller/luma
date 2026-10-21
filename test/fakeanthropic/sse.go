package fakeanthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// The wire shapes below follow the Messages API, field order included.

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type redactedThinkingBlock struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolUseBlock struct {
	Type  string `json:"type"` // tool_use or server_tool_use
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}

type webSearchResult struct {
	Type             string  `json:"type"`
	Title            string  `json:"title"`
	URL              string  `json:"url"`
	EncryptedContent string  `json:"encrypted_content"`
	PageAge          *string `json:"page_age"`
}

type webSearchToolResultBlock struct {
	Type      string            `json:"type"`
	ToolUseID string            `json:"tool_use_id"`
	Content   []webSearchResult `json:"content"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type signatureDelta struct {
	Type      string `json:"type"`
	Signature string `json:"signature"`
}

type inputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type cacheCreation struct {
	Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
}

type serverToolUsage struct {
	WebSearchRequests int `json:"web_search_requests"`
}

type usageJSON struct {
	InputTokens              int              `json:"input_tokens"`
	CacheCreationInputTokens int              `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int              `json:"cache_read_input_tokens"`
	CacheCreation            *cacheCreation   `json:"cache_creation,omitempty"`
	OutputTokens             int              `json:"output_tokens"`
	ServerToolUse            *serverToolUsage `json:"server_tool_use,omitempty"`
	ServiceTier              string           `json:"service_tier,omitempty"`
}

type messageJSON struct {
	ID                   string            `json:"id"`
	Type                 string            `json:"type"`
	Role                 string            `json:"role"`
	Model                string            `json:"model"`
	Content              []any             `json:"content"`
	StopReason           *string           `json:"stop_reason"`
	StopSequence         *string           `json:"stop_sequence"`
	Usage                usageJSON         `json:"usage"`
	InputTransformations *[]map[string]any `json:"input_transformations,omitempty"`
}

// block is one content block of a reply: what content_block_start carries,
// the deltas that complete it, and its final form.
type block struct {
	start  any
	deltas []any
	full   any
}

// reply is a complete scripted answer, rendered as SSE or as one JSON body.
type reply struct {
	id, model, stopReason string
	blocks                []block
	usage                 Usage
	webSearches           int // > 0 when a server tool ran
	transformations       *[]map[string]any
}

func thinking(sig string) block {
	return block{
		start:  thinkingBlock{Type: "thinking"},
		deltas: []any{signatureDelta{Type: "signature_delta", Signature: sig}},
		full:   thinkingBlock{Type: "thinking", Signature: sig},
	}
}

func redactedThinking(sig string) block {
	b := redactedThinkingBlock{Type: "redacted_thinking", Data: sig}
	return block{start: b, full: b}
}

func text(s string) block {
	return block{
		start:  textBlock{Type: "text"},
		deltas: []any{textDelta{Type: "text_delta", Text: s}},
		full:   textBlock{Type: "text", Text: s},
	}
}

func toolUse(typ, id, name string, input map[string]any) block {
	if input == nil {
		input = map[string]any{}
	}
	return block{
		start:  toolUseBlock{Type: typ, ID: id, Name: name, Input: map[string]any{}},
		deltas: []any{inputJSONDelta{Type: "input_json_delta", PartialJSON: string(marshal(input))}},
		full:   toolUseBlock{Type: typ, ID: id, Name: name, Input: input},
	}
}

func webSearchToolResult(toolUseID string, results []SearchResult) block {
	content := make([]webSearchResult, 0, len(results))
	for _, r := range results {
		content = append(content, webSearchResult{Type: "web_search_result", Title: r.Title, URL: r.URL, EncryptedContent: r.Snippet})
	}
	b := webSearchToolResultBlock{Type: "web_search_tool_result", ToolUseID: toolUseID, Content: content}
	return block{start: b, full: b}
}

// fullUsage is the usage of message_start and of a non-streaming body.
func (r *reply) fullUsage(output int) usageJSON {
	u := r.usage
	return usageJSON{
		InputTokens:              u.Input,
		CacheCreationInputTokens: u.CacheCreation,
		CacheReadInputTokens:     u.CacheRead,
		CacheCreation:            &cacheCreation{Ephemeral5m: max(u.CacheCreation-u.Ephemeral1h, 0), Ephemeral1h: u.Ephemeral1h},
		OutputTokens:             output,
		ServerToolUse:            r.serverToolUsage(),
		ServiceTier:              "standard",
	}
}

func (r *reply) serverToolUsage() *serverToolUsage {
	if r.webSearches == 0 {
		return nil
	}
	return &serverToolUsage{WebSearchRequests: r.webSearches}
}

func (r *reply) message(content []any, stopReason *string, output int) messageJSON {
	return messageJSON{
		ID: r.id, Type: "message", Role: "assistant", Model: r.model,
		Content: content, StopReason: stopReason, Usage: r.fullUsage(output),
		InputTransformations: r.transformations,
	}
}

// writeJSON writes the reply as a non-streaming body.
func (r *reply) writeJSON(w http.ResponseWriter) {
	content := make([]any, len(r.blocks))
	for i, b := range r.blocks {
		content[i] = b.full
	}
	stop := r.stopReason
	writeBody(w, http.StatusOK, r.message(content, &stop, r.usage.Output))
}

// writeSSE streams the reply. message_start is flushed at once; every later
// event waits delay first. It stops early if the client goes away.
func (r *reply) writeSSE(ctx context.Context, w http.ResponseWriter, delay time.Duration) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	s := &sseWriter{ctx: ctx, w: w, rc: http.NewResponseController(w), delay: delay}

	start := r.message([]any{}, nil, 0)
	start.Usage.ServerToolUse = nil
	if !s.event("message_start", struct {
		Type    string      `json:"type"`
		Message messageJSON `json:"message"`
	}{"message_start", start}) {
		return
	}
	if !s.event("ping", map[string]any{"type": "ping"}) {
		return
	}
	for i, b := range r.blocks {
		if !s.event("content_block_start", struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock any    `json:"content_block"`
		}{"content_block_start", i, b.start}) {
			return
		}
		for _, d := range b.deltas {
			if !s.event("content_block_delta", struct {
				Type  string `json:"type"`
				Index int    `json:"index"`
				Delta any    `json:"delta"`
			}{"content_block_delta", i, d}) {
				return
			}
		}
		if !s.event("content_block_stop", struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}{"content_block_stop", i}) {
			return
		}
	}
	u := r.usage
	if !s.event("message_delta", struct {
		Type  string `json:"type"`
		Delta struct {
			StopReason   string  `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		} `json:"delta"`
		Usage usageJSON `json:"usage"`
	}{
		Type: "message_delta",
		Delta: struct {
			StopReason   string  `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		}{StopReason: r.stopReason},
		// message_delta usage is cumulative and has no cache_creation split.
		Usage: usageJSON{
			InputTokens:              u.Input,
			CacheCreationInputTokens: u.CacheCreation,
			CacheReadInputTokens:     u.CacheRead,
			OutputTokens:             u.Output,
			ServerToolUse:            r.serverToolUsage(),
		},
	}) {
		return
	}
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

type sseWriter struct {
	ctx   context.Context
	w     http.ResponseWriter
	rc    *http.ResponseController
	delay time.Duration
	n     int
}

// event writes and flushes one event, after the delay unless it is the
// first. It reports whether the stream should go on.
func (s *sseWriter) event(name string, payload any) bool {
	if s.n > 0 && s.delay > 0 {
		t := time.NewTimer(s.delay)
		select {
		case <-s.ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
	s.n++
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, marshal(payload)); err != nil {
		return false
	}
	return s.rc.Flush() == nil
}

// marshal encodes v as compact JSON without HTML escaping, as the API does.
func marshal(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		panic(fmt.Sprintf("fakeanthropic: marshal %T: %v", v, err))
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

// writeBody writes v as an application/json body.
func writeBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(marshal(v))
}

// writeError writes the API's error envelope.
func writeError(w http.ResponseWriter, status int, requestID, typ, msg string) {
	writeBody(w, status, struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}{
		Type: "error",
		Error: struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}{typ, msg},
		RequestID: requestID,
	})
}
