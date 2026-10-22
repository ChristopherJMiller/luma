package protocol

import (
	"encoding/json"
	"fmt"
)

// LinkFrame is one JSON frame of the conductor link (C2): {type, ...}.
type LinkFrame interface{ FrameType() string }

// ---- Core -> conductor ----

// HostOpen is `host.open`.
type HostOpen struct {
	SessionID string   `json:"session_id"`
	Spec      HostSpec `json:"spec"`
}

// TurnSend is `turn.send`. Attachments are inlined as base64 blocks or fenced
// text and the <luma-now> block is already appended. Untrusted is the
// session's taint.
type TurnSend struct {
	SessionID string         `json:"session_id"`
	TurnID    string         `json:"turn_id"`
	Content   []ContentBlock `json:"content"`
	Untrusted bool           `json:"untrusted"`
}

// TurnInterrupt is `turn.interrupt`.
type TurnInterrupt struct {
	SessionID string `json:"session_id"`
}

// HostClose is `host.close`.
type HostClose struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"` // deleted | shutdown | maintenance | error (Core gave up on a stuck turn, S48)
}

// TreePurge is `tree.purge`: remove the tree's project dirs after its hosts
// close, even when none is open (S14).
type TreePurge struct {
	RootID    string `json:"root_id"`
	PersonRef string `json:"person_ref"`
}

// MemoryResult is the result of a memory rpc.
type MemoryResult struct {
	Status  string    `json:"status"` // applied | pending | disabled | full | rejected
	WriteID string    `json:"write_id,omitempty"`
	Removed *int      `json:"removed,omitempty"`
	Code    ErrorCode `json:"code,omitempty"`
}

// RPCResult is `rpc.result`.
type RPCResult struct {
	RPCID  string        `json:"rpc_id"`
	OK     bool          `json:"ok"`
	Result *MemoryResult `json:"result,omitempty"`
	Error  *ErrorInfo    `json:"error,omitempty"`
}

// Shutdown is `shutdown`.
type Shutdown struct {
	Reason string `json:"reason"`
}

// ---- conductor -> Core ----

// Hello is `hello`.
type Hello struct {
	ConductorVersion string `json:"conductor_version"`
	SDKVersion       string `json:"sdk_version"`
	CLIVersion       string `json:"cli_version"`
	MaxHosts         int    `json:"max_hosts"`
}

// HostState is `host.state`. A host sends `ready` when it opens, and `ready`
// again with SDKSessionID as soon as the SDK reports it (S2).
type HostState struct {
	SessionID    string `json:"session_id"`
	State        string `json:"state"` // ready | closed
	SDKSessionID string `json:"sdk_session_id,omitempty"`
	Reason       string `json:"reason,omitempty"` // idle | evicted | capacity | error | shutdown | deleted | maintenance
}

// EventFrame is `event`: a LinkEvent for a session's running turn.
type EventFrame struct {
	SessionID string
	TurnID    string
	Event     LinkEvent
}

// RPCParams are the params of a memory rpc.
type RPCParams struct {
	Text      string `json:"text"`
	Untrusted bool   `json:"untrusted"`
}

// RPC is `rpc`.
type RPC struct {
	RPCID     string    `json:"rpc_id"`
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`
	Method    string    `json:"method"` // memory.remember | memory.forget
	Params    RPCParams `json:"params"`
}

// TreePurged is `tree.purged`, the ack of tree.purge, sent once the tree's
// hosts are closed and its project dirs removed (or the purge failed and was
// logged). Core deletes the tree's rows after it (S39).
type TreePurged struct {
	RootID string `json:"root_id"`
}

func (HostOpen) FrameType() string      { return "host.open" }
func (TurnSend) FrameType() string      { return "turn.send" }
func (TurnInterrupt) FrameType() string { return "turn.interrupt" }
func (HostClose) FrameType() string     { return "host.close" }
func (TreePurge) FrameType() string     { return "tree.purge" }
func (RPCResult) FrameType() string     { return "rpc.result" }
func (Shutdown) FrameType() string      { return "shutdown" }
func (Hello) FrameType() string         { return "hello" }
func (HostState) FrameType() string     { return "host.state" }
func (EventFrame) FrameType() string    { return "event" }
func (RPC) FrameType() string           { return "rpc" }
func (TreePurged) FrameType() string    { return "tree.purged" }

type eventFrameJSON struct {
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	Event     json.RawMessage `json:"event"`
}

// MarshalJSON encodes the nested LinkEvent with its own type tag.
func (f EventFrame) MarshalJSON() ([]byte, error) {
	if f.Event == nil {
		return nil, fmt.Errorf("protocol: event frame without event")
	}
	ev, err := marshalTyped(f.Event.LinkEventType(), f.Event)
	if err != nil {
		return nil, err
	}
	return marshalTyped(f.FrameType(), eventFrameJSON{f.SessionID, f.TurnID, ev})
}

// UnmarshalJSON decodes the nested LinkEvent by its type; unknown is an error.
func (f *EventFrame) UnmarshalJSON(data []byte) error {
	var raw eventFrameJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	ev, err := DecodeLinkEvent(raw.Event)
	if err != nil {
		return err
	}
	*f = EventFrame{SessionID: raw.SessionID, TurnID: raw.TurnID, Event: ev}
	return nil
}

func newFrame(typ string) LinkFrame {
	switch typ {
	case "host.open":
		return &HostOpen{}
	case "turn.send":
		return &TurnSend{}
	case "turn.interrupt":
		return &TurnInterrupt{}
	case "host.close":
		return &HostClose{}
	case "tree.purge":
		return &TreePurge{}
	case "rpc.result":
		return &RPCResult{}
	case "shutdown":
		return &Shutdown{}
	case "hello":
		return &Hello{}
	case "host.state":
		return &HostState{}
	case "event":
		return &EventFrame{}
	case "rpc":
		return &RPC{}
	case "tree.purged":
		return &TreePurged{}
	}
	return nil
}

// EncodeLinkFrame encodes a frame with its type tag.
func EncodeLinkFrame(f LinkFrame) ([]byte, error) {
	return json.Marshal(f)
}

// DecodeLinkFrame decodes one link frame; an unknown type is an error.
func DecodeLinkFrame(data []byte) (LinkFrame, error) {
	typ, err := typeOf(data)
	if err != nil {
		return nil, err
	}
	f := newFrame(typ)
	if f == nil {
		return nil, fmt.Errorf("protocol: unknown link frame type %q", typ)
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, err
	}
	return deref(f).(LinkFrame), nil
}

// ---- LinkEvent ----

// LinkEvent is the payload of an `event` frame; the turn is implied by the
// envelope.
type LinkEvent interface{ LinkEventType() string }

// LinkMessageStarted is `message_started`.
type LinkMessageStarted struct {
	Model string `json:"model"`
}

// LinkTextDelta is `text_delta`, coalesced for about 100 ms (P24).
type LinkTextDelta struct {
	Text string `json:"text"`
}

// LinkToolStarted is `tool_started`.
type LinkToolStarted struct {
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	Summary   string `json:"summary"`
}

// LinkToolFinished is `tool_finished`.
type LinkToolFinished struct {
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Summary   string `json:"summary,omitempty"`
}

// MainRequestUsage is `main_request_usage`: the API message's own usage
// fields, so Input is the uncached input_tokens (S28).
type MainRequestUsage struct {
	APIMessageID string `json:"api_message_id"`
	Model        string `json:"model"`
	SLOExclusion string `json:"slo_exclusion"` // none | first | cold_resume | post_compaction
	Input        int64  `json:"input"`
	CacheRead    int64  `json:"cache_read"`
	CacheCreate  int64  `json:"cache_creation"`
}

// LinkModelSwitched is `model_switched`.
type LinkModelSwitched struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ModelUsage is one model's usage inside TurnUsage and Totals.
type ModelUsage struct {
	Input             int64   `json:"input"`
	Output            int64   `json:"output"`
	CacheRead         int64   `json:"cache_read"`
	CacheCreation     int64   `json:"cache_creation"`
	WebSearchRequests int64   `json:"web_search_requests"`
	CostUSD           float64 `json:"cost_usd"`
}

// TurnUsage is a per-turn delta.
type TurnUsage struct {
	Models  map[string]ModelUsage `json:"models"`
	CostUSD float64               `json:"cost_usd"`
}

// Totals has TurnUsage's shape, as running totals.
type Totals TurnUsage

// TurnResult is `turn_result`. Core persists Totals as sessions.last_totals.
type TurnResult struct {
	SDKUUID   string        `json:"sdk_uuid"`
	Content   []MessagePart `json:"content"`
	Usage     TurnUsage     `json:"usage"`
	Totals    Totals        `json:"totals"`
	CostBasis *string       `json:"cost_basis"` // list | managed | unknown, or null
}

// LinkError is `error`. Detail "thinking_binding" is counted by Core (S16).
type LinkError struct {
	Code      ErrorCode `json:"code"`
	Retryable bool      `json:"retryable"`
	Detail    string    `json:"detail,omitempty"`
}

func (LinkMessageStarted) LinkEventType() string { return "message_started" }
func (LinkTextDelta) LinkEventType() string      { return "text_delta" }
func (LinkToolStarted) LinkEventType() string    { return "tool_started" }
func (LinkToolFinished) LinkEventType() string   { return "tool_finished" }
func (MainRequestUsage) LinkEventType() string   { return "main_request_usage" }
func (LinkModelSwitched) LinkEventType() string  { return "model_switched" }
func (TurnResult) LinkEventType() string         { return "turn_result" }
func (LinkError) LinkEventType() string          { return "error" }

func newLinkEvent(typ string) LinkEvent {
	switch typ {
	case "message_started":
		return &LinkMessageStarted{}
	case "text_delta":
		return &LinkTextDelta{}
	case "tool_started":
		return &LinkToolStarted{}
	case "tool_finished":
		return &LinkToolFinished{}
	case "main_request_usage":
		return &MainRequestUsage{}
	case "model_switched":
		return &LinkModelSwitched{}
	case "turn_result":
		return &TurnResult{}
	case "error":
		return &LinkError{}
	}
	return nil
}

// DecodeLinkEvent decodes the event of an `event` frame; unknown is an error.
func DecodeLinkEvent(data []byte) (LinkEvent, error) {
	typ, err := typeOf(data)
	if err != nil {
		return nil, err
	}
	e := newLinkEvent(typ)
	if e == nil {
		return nil, fmt.Errorf("protocol: unknown link event type %q", typ)
	}
	if err := json.Unmarshal(data, e); err != nil {
		return nil, err
	}
	return deref(e).(LinkEvent), nil
}

// ---- HostSpec and AgentSpec ----

// ResumeFork names the fork point of a fork's first open.
type ResumeFork struct {
	AtSDKUUID string `json:"at_sdk_uuid"`
}

// Resume is set on every host.open of a session with an sdk_session_id (S2).
// BaselineTotals is never the host's delta baseline (Spike S6).
type Resume struct {
	SDKSessionID   string      `json:"sdk_session_id"`
	Fork           *ResumeFork `json:"fork,omitempty"`
	BaselineTotals *Totals     `json:"baseline_totals,omitempty"`
}

// HostPerson is the person block of a HostSpec: the opaque ref only (P21).
type HostPerson struct {
	Ref string `json:"ref"`
}

// HostSpec is what Core tells the conductor to open.
type HostSpec struct {
	SessionID           string     `json:"session_id"`
	RootID              string     `json:"root_id"`
	Person              HostPerson `json:"person"`
	Agent               AgentSpec  `json:"agent"`
	Model               string     `json:"model"`
	HelperModel         string     `json:"helper_model"`
	Effort              string     `json:"effort"`
	CacheTTL            string     `json:"cache_ttl"` // 5m | 1h
	Workspace           string     `json:"workspace"` // the api_workspace: interactive | ci
	Source              string     `json:"source"`    // web | ci
	Timezone            string     `json:"timezone"`
	ProfileContext      string     `json:"profile_context"`
	WebfetchDenyDomains []string   `json:"webfetch_deny_domains"`
	Resume              *Resume    `json:"resume"`
}

// SubagentSpec is one compiled subagent.
type SubagentSpec struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Model       string   `json:"model"`
	Effort      string   `json:"effort"`
	Tools       []string `json:"tools"`
}

// AgentLimits are the AGENT.md limits.
type AgentLimits struct {
	MaxTurns            int `json:"max_turns"`
	MaxToolCalls        int `json:"max_tool_calls"`
	WebSearchPerTurn    int `json:"web_search_per_turn"`
	WebSearchPerSession int `json:"web_search_per_session"`
}

// TierBinding is the models.yaml tier an agent is bound to.
type TierBinding struct {
	Model             string `json:"model"`
	Effort            string `json:"effort"`
	CacheTTL          string `json:"cache_ttl"`
	AutoCompactWindow int    `json:"auto_compact_window,omitempty"`
	AutoCompactPct    int    `json:"auto_compact_pct,omitempty"`
	PDFMaxPages       int    `json:"pdf_max_pages"`
}

// AgentSpec is the compiled output of agents.Compile.
type AgentSpec struct {
	ID              string                  `json:"id"`
	Version         string                  `json:"version"`
	SystemPrompt    string                  `json:"system_prompt"`
	Tools           []string                `json:"tools"` // sorted
	BuiltinTools    []string                `json:"builtin_tools"`
	MCP             []string                `json:"mcp"`
	UntrustedInputs []string                `json:"untrusted_inputs"`
	Subagents       map[string]SubagentSpec `json:"subagents"`
	Limits          AgentLimits             `json:"limits"`
	Memory          string                  `json:"memory"` // person | none
	TierBinding     TierBinding             `json:"tier_binding"`
}
