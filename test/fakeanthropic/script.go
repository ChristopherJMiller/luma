package fakeanthropic

import (
	"encoding/json"
	"regexp"
)

// Step is one scripted reply. Its zero value is a plain text reply that
// starts with a signed thinking block. The JSON names are the --scripts
// file's (README "Scripts").
type Step struct {
	// Text is the reply's text block. Empty with ToolUses set: no text
	// block. Empty without: an empty text block.
	Text string `json:"text,omitempty"`
	// ToolUses follow the text, as tool_use blocks with ids
	// toolu_fake_<seq>_<i>. They make the default stop_reason "tool_use".
	ToolUses []ToolUse `json:"tool_uses,omitempty"`
	// NoThinking drops the leading thinking block. The zero value replies
	// with one.
	NoThinking bool `json:"no_thinking,omitempty"`
	// RedactedThinking replaces the thinking block with a
	// redacted_thinking block whose data is the same HMAC.
	RedactedThinking bool `json:"redacted_thinking,omitempty"`
	// Model overrides the reply's model (default: the request's).
	Model string `json:"model,omitempty"`
	// StopReason overrides "end_turn" / "tool_use".
	StopReason string `json:"stop_reason,omitempty"`
	// Usage replaces Options.DefaultUsage for this reply.
	Usage *Usage `json:"usage,omitempty"`
	// InputTransformations is sent as the message's input_transformations.
	InputTransformations []map[string]any `json:"input_transformations,omitempty"`
	// Status, when neither 0 nor 200, makes the reply an API error with
	// that status. ErrorType and ErrorMessage default from the status.
	// RetryAfter, if set, is sent as the retry-after header.
	Status       int    `json:"status,omitempty"`
	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	RetryAfter   string `json:"retry_after,omitempty"`
}

// ToolUse is one scripted client tool call.
type ToolUse struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// SearchResult is one web_search_result of an auto-answered server tool.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Usage is the token accounting a reply reports. CacheCreation is the
// total cache write; Ephemeral1h is its 1h part, and the 5m part is the
// rest. WebSearchRequests applies to auto-answered web searches only (0
// counts as 1).
type Usage struct {
	Input             int `json:"input,omitempty"`
	Output            int `json:"output,omitempty"`
	CacheRead         int `json:"cache_read,omitempty"`
	CacheCreation     int `json:"cache_creation,omitempty"`
	Ephemeral1h       int `json:"ephemeral_1h,omitempty"`
	WebSearchRequests int `json:"web_search_requests,omitempty"`
}

// markerRE matches the script marker [[fake:<name>]].
var markerRE = regexp.MustCompile(`\[\[fake:([A-Za-z0-9_.-]+)\]\]`)

// apiMessage is one entry of the request's messages, decoded only as far
// as the fake reads it.
type apiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// apiBlock is a content block, decoded only as far as the fake reads it.
type apiBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

// blocks decodes the message content. A string is one text block; any
// other shape that is not an array of objects has no blocks.
func (m apiMessage) blocks() []apiBlock {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []apiBlock{{Type: "text", Text: s}}
	}
	var bs []apiBlock
	if json.Unmarshal(m.Content, &bs) != nil {
		return nil
	}
	return bs
}

// scriptName finds the turn's marker. It reads the last user message; a
// user message holding a tool_result continues the turn of the user
// message before it, so every request of a tool loop carries the marker of
// the message that started the turn. The first marker found wins.
func scriptName(msgs []apiMessage) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		toolResult := false
		for _, b := range msgs[i].blocks() {
			switch b.Type {
			case "text":
				if m := markerRE.FindStringSubmatch(b.Text); m != nil {
					return m[1], true
				}
			case "tool_result":
				toolResult = true
			}
		}
		if !toolResult {
			return "", false
		}
	}
	return "", false
}

// lastUserText is the text of the last user message's last text block: the
// query of an auto-answered web search.
func lastUserText(msgs []apiMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		bs := msgs[i].blocks()
		for j := len(bs) - 1; j >= 0; j-- {
			if bs[j].Type == "text" {
				return bs[j].Text
			}
		}
		return ""
	}
	return ""
}

// errorDefaults are the error type and message for each status the API
// documents, used when a step names only its status.
var errorDefaults = map[int][2]string{
	400: {"invalid_request_error", "Invalid request"},
	401: {"authentication_error", "invalid x-api-key"},
	402: {"billing_error", "Billing error"},
	403: {"permission_error", "Permission denied"},
	404: {"not_found_error", "Not found"},
	413: {"request_too_large", "Request exceeds the maximum allowed number of bytes."},
	429: {"rate_limit_error", "Rate limit exceeded"},
	500: {"api_error", "Internal server error"},
	529: {"overloaded_error", "Overloaded"},
}

// errorFor is a step's error type and message.
func errorFor(st Step) (typ, msg string) {
	d, ok := errorDefaults[st.Status]
	if !ok {
		d = [2]string{"api_error", "Internal server error"}
	}
	typ, msg = st.ErrorType, st.ErrorMessage
	if typ == "" {
		typ = d[0]
	}
	if msg == "" {
		msg = d[1]
	}
	return typ, msg
}
