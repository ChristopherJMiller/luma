// Package protocol holds Luma's wire contracts: the client API (C1), the
// conductor link (C2), the error codes (C3) and the internal endpoints (C10).
// The TypeScript mirror is packages/protocol; the hand-written golden frames
// both sides test against are protocol/fixtures. A change here must change
// C1-C10 of docs/plans/phase-1-lean-chat.md in the same commit.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// MessagePart is one part of a stored message: text, a tool use, or (user
// messages only) a bound attachment (S15). Type selects the fields.
type MessagePart struct {
	Type string `json:"type"` // "text" | "tool" | "attachment"

	Text string `json:"text,omitempty"` // text

	ToolUseID string `json:"tool_use_id,omitempty"` // tool
	Name      string `json:"name,omitempty"`        // tool
	Summary   string `json:"summary,omitempty"`     // tool
	IsError   bool   `json:"is_error,omitempty"`    // tool

	ID        string `json:"id,omitempty"`         // attachment
	MediaType string `json:"media_type,omitempty"` // attachment
	Filename  string `json:"filename,omitempty"`   // attachment
}

// MarshalJSON writes exactly the fields of the part's type, so a `false`
// is_error and an empty summary stay on the wire.
func (p MessagePart) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case "text":
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{p.Type, p.Text})
	case "tool":
		return json.Marshal(struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Name      string `json:"name"`
			Summary   string `json:"summary"`
			IsError   bool   `json:"is_error"`
		}{p.Type, p.ToolUseID, p.Name, p.Summary, p.IsError})
	case "attachment":
		return json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			MediaType string `json:"media_type"`
			Filename  string `json:"filename"`
		}{p.Type, p.ID, p.MediaType, p.Filename})
	}
	return nil, fmt.Errorf("protocol: unknown message part type %q", p.Type)
}

// UnmarshalJSON rejects an unknown part type.
func (p *MessagePart) UnmarshalJSON(data []byte) error {
	type plain MessagePart
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch v.Type {
	case "text", "tool", "attachment":
		*p = MessagePart(v)
		return nil
	}
	return fmt.Errorf("protocol: unknown message part type %q", v.Type)
}

// Message mirrors C4 `messages`.
type Message struct {
	ID              string        `json:"id"`
	SessionID       string        `json:"session_id"`
	ParentMessageID *string       `json:"parent_message_id"`
	TurnID          string        `json:"turn_id"`
	Role            string        `json:"role"` // "user" | "assistant"
	Content         []MessagePart `json:"content"`
	SDKUUID         *string       `json:"sdk_uuid"`
	Model           *string       `json:"model"`
	Status          string        `json:"status"` // "running" | "complete" | "error"
	ErrorCode       *ErrorCode    `json:"error_code"`
	CreatedAt       time.Time     `json:"created_at"`
}

// MemoryWrite mirrors C4 `memory_writes` without person_id.
type MemoryWrite struct {
	ID        string     `json:"id"`
	SessionID *string    `json:"session_id"`
	TurnID    string     `json:"turn_id"`
	Op        string     `json:"op"` // "remember" | "forget"
	Text      string     `json:"text"`
	Matched   []string   `json:"matched"`
	Untrusted bool       `json:"untrusted"`
	Status    string     `json:"status"` // "applied" | "pending" | "rejected"
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at"`
}

// ContentBlock is one Anthropic-shaped input block: text (also fenced text),
// or a base64 image or document. Client events carry only text blocks.
type ContentBlock struct {
	Type   string        `json:"type"` // "text" | "image" | "document"
	Text   string        `json:"text,omitempty"`
	Source *Base64Source `json:"source,omitempty"`
}

// Base64Source is the source of an image or document block.
type Base64Source struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// MarshalJSON keeps an empty text block's "text".
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "text":
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.Type, b.Text})
	case "image", "document":
		if b.Source == nil {
			return nil, fmt.Errorf("protocol: %s block without source", b.Type)
		}
		return json.Marshal(struct {
			Type   string       `json:"type"`
			Source Base64Source `json:"source"`
		}{b.Type, *b.Source})
	}
	return nil, fmt.Errorf("protocol: unknown content block type %q", b.Type)
}

// UnmarshalJSON rejects an unknown block type.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type plain ContentBlock
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch v.Type {
	case "text", "image", "document":
		*b = ContentBlock(v)
		return nil
	}
	return fmt.Errorf("protocol: unknown content block type %q", v.Type)
}

// mergeObjects joins two JSON objects into one (keys of b after a's).
func mergeObjects(a, b []byte) []byte {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("{}")) {
		return a
	}
	out := make([]byte, 0, len(a)+len(b))
	out = append(out, a[:len(a)-1]...)
	out = append(out, ',')
	return append(out, b[1:]...)
}

// marshalTyped encodes body with a leading "type" key.
func marshalTyped(typ string, body any) ([]byte, error) {
	head, err := json.Marshal(struct {
		Type string `json:"type"`
	}{typ})
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return mergeObjects(head, b), nil
}

// ClientEvent is what a client posts to /v1/sessions/{id}/events.
type ClientEvent interface{ ClientEventType() string }

// UserMessage is `user.message`. Content holds text blocks only.
type UserMessage struct {
	ClientID      string         `json:"client_id"`
	Content       []ContentBlock `json:"content"`
	AttachmentIDs []string       `json:"attachment_ids"`
}

// UserInterrupt is `user.interrupt`.
type UserInterrupt struct{}

func (UserMessage) ClientEventType() string   { return "user.message" }
func (UserInterrupt) ClientEventType() string { return "user.interrupt" }

// EncodeClientEvent encodes a client event with its type tag.
func EncodeClientEvent(e ClientEvent) ([]byte, error) {
	return marshalTyped(e.ClientEventType(), e)
}

// DecodeClientEvent decodes a client event; an unknown type is an error.
func DecodeClientEvent(data []byte) (ClientEvent, error) {
	typ, err := typeOf(data)
	if err != nil {
		return nil, err
	}
	var e ClientEvent
	switch typ {
	case "user.message":
		e = &UserMessage{}
	case "user.interrupt":
		e = &UserInterrupt{}
	default:
		return nil, fmt.Errorf("protocol: unknown client event type %q", typ)
	}
	if err := json.Unmarshal(data, e); err != nil {
		return nil, err
	}
	return deref(e).(ClientEvent), nil
}

func typeOf(data []byte) (string, error) {
	var h struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return "", err
	}
	if h.Type == "" {
		return "", errors.New("protocol: frame has no type")
	}
	return h.Type, nil
}

// EventBody is the type-specific part of an Event.
type EventBody interface{ EventType() string }

// Event is a server-to-client event, also the stored events.payload:
// {id, session_id, type, created_at, ...body}.
type Event struct {
	ID        int64
	SessionID string
	CreatedAt time.Time
	Body      EventBody
}

type eventHeader struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionStatus is `session.status`.
type SessionStatus struct {
	Status string `json:"status"` // running | idle | requires_action | error
}

// MessageUser is `message.user`.
type MessageUser struct {
	Message Message `json:"message"`
}

// MessageStarted is `message.started`: the turn's one assistant message (P23).
type MessageStarted struct {
	MessageID string `json:"message_id"`
	Model     string `json:"model"`
}

// MessageTextDelta is `message.text_delta`.
type MessageTextDelta struct {
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

// ToolStarted is `tool.started`.
type ToolStarted struct {
	MessageID string `json:"message_id"`
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	Summary   string `json:"summary"`
}

// ToolFinished is `tool.finished`.
type ToolFinished struct {
	MessageID string `json:"message_id"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Summary   string `json:"summary"`
}

// MessageCompleted is `message.completed`.
type MessageCompleted struct {
	Message Message `json:"message"`
}

// ModelSwitched is `model.switched`.
type ModelSwitched struct {
	MessageID string `json:"message_id"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// MemoryWriteEvent is `memory.write`, emitted on create and on each status change.
type MemoryWriteEvent struct {
	Write MemoryWrite `json:"write"`
}

// Usage is `usage`.
type Usage struct {
	TurnID  string  `json:"turn_id"`
	CostUSD float64 `json:"cost_usd"`
}

// ErrorEvent is `error`.
type ErrorEvent struct {
	Code      ErrorCode `json:"code"`
	Retryable bool      `json:"retryable"`
}

func (SessionStatus) EventType() string    { return "session.status" }
func (MessageUser) EventType() string      { return "message.user" }
func (MessageStarted) EventType() string   { return "message.started" }
func (MessageTextDelta) EventType() string { return "message.text_delta" }
func (ToolStarted) EventType() string      { return "tool.started" }
func (ToolFinished) EventType() string     { return "tool.finished" }
func (MessageCompleted) EventType() string { return "message.completed" }
func (ModelSwitched) EventType() string    { return "model.switched" }
func (MemoryWriteEvent) EventType() string { return "memory.write" }
func (Usage) EventType() string            { return "usage" }
func (ErrorEvent) EventType() string       { return "error" }

func newEventBody(typ string) EventBody {
	switch typ {
	case "session.status":
		return &SessionStatus{}
	case "message.user":
		return &MessageUser{}
	case "message.started":
		return &MessageStarted{}
	case "message.text_delta":
		return &MessageTextDelta{}
	case "tool.started":
		return &ToolStarted{}
	case "tool.finished":
		return &ToolFinished{}
	case "message.completed":
		return &MessageCompleted{}
	case "model.switched":
		return &ModelSwitched{}
	case "memory.write":
		return &MemoryWriteEvent{}
	case "usage":
		return &Usage{}
	case "error":
		return &ErrorEvent{}
	}
	return nil
}

// MarshalJSON flattens the header and the body into one object.
func (e Event) MarshalJSON() ([]byte, error) {
	if e.Body == nil {
		return nil, errors.New("protocol: event without body")
	}
	head, err := json.Marshal(eventHeader{e.ID, e.SessionID, e.Body.EventType(), e.CreatedAt})
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(e.Body)
	if err != nil {
		return nil, err
	}
	return mergeObjects(head, b), nil
}

// UnmarshalJSON decodes the header, then the body its type names.
func (e *Event) UnmarshalJSON(data []byte) error {
	var h eventHeader
	if err := json.Unmarshal(data, &h); err != nil {
		return err
	}
	body := newEventBody(h.Type)
	if body == nil {
		return fmt.Errorf("protocol: unknown event type %q", h.Type)
	}
	if err := json.Unmarshal(data, body); err != nil {
		return err
	}
	*e = Event{ID: h.ID, SessionID: h.SessionID, CreatedAt: h.CreatedAt, Body: deref(body).(EventBody)}
	return nil
}

// DecodeEvent decodes one Event frame; an unknown type is an error.
func DecodeEvent(data []byte) (Event, error) {
	var e Event
	err := json.Unmarshal(data, &e)
	return e, err
}

// deref turns a pointer to a struct into the struct, so decoded values are
// plain values like the ones callers construct.
func deref(p any) any {
	return reflect.ValueOf(p).Elem().Interface()
}
