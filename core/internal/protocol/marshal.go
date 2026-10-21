package protocol

import "encoding/json"

// Go encodes a nil slice or map as null, but the TypeScript side declares
// these fields non-nullable. Every type that carries a container below turns
// nil into [] or {} at encode time, so a zero value never puts null on the
// wire. Fields that are nullable by contract (MemoryWrite.Matched, ids and
// timestamps) are pointers or documented null, and are not touched.
//
// Frames and client events also write their own `type` tag, so json.Marshal
// of any of them equals EncodeLinkFrame / EncodeClientEvent.

func orEmpty[S ~[]E, E any](s S) S {
	if s == nil {
		return S{}
	}
	return s
}

func orEmptyMap[M ~map[K]V, K comparable, V any](m M) M {
	if m == nil {
		return M{}
	}
	return m
}

// ---- C1 ----

func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	m.Content = orEmpty(m.Content)
	return json.Marshal(plain(m))
}

func (t Tree) MarshalJSON() ([]byte, error) {
	type plain Tree
	t.Messages = orEmpty(t.Messages)
	t.LastEventIDs = orEmptyMap(t.LastEventIDs)
	t.PendingWrites = orEmpty(t.PendingWrites)
	return json.Marshal(plain(t))
}

func (p Profile) MarshalJSON() ([]byte, error) {
	type plain Profile
	p.Pending = orEmpty(p.Pending)
	return json.Marshal(plain(p))
}

func (l SessionList) MarshalJSON() ([]byte, error) {
	type plain SessionList
	l.Sessions = orEmpty(l.Sessions)
	return json.Marshal(plain(l))
}

func (l AgentList) MarshalJSON() ([]byte, error) {
	type plain AgentList
	l.Agents = orEmpty(l.Agents)
	return json.Marshal(plain(l))
}

func (a AgentSummary) MarshalJSON() ([]byte, error) {
	type plain AgentSummary
	a.Tiers = orEmpty(a.Tiers)
	return json.Marshal(plain(a))
}

func (m UserMessage) MarshalJSON() ([]byte, error) {
	type plain UserMessage
	m.Content = orEmpty(m.Content)
	m.AttachmentIDs = orEmpty(m.AttachmentIDs)
	return marshalTyped(m.ClientEventType(), plain(m))
}

func (i UserInterrupt) MarshalJSON() ([]byte, error) {
	return marshalTyped(i.ClientEventType(), struct{}{})
}

// ---- C2 ----

func (s HostSpec) MarshalJSON() ([]byte, error) {
	type plain HostSpec
	s.WebfetchDenyDomains = orEmpty(s.WebfetchDenyDomains)
	return json.Marshal(plain(s))
}

func (s AgentSpec) MarshalJSON() ([]byte, error) {
	type plain AgentSpec
	s.Tools = orEmpty(s.Tools)
	s.BuiltinTools = orEmpty(s.BuiltinTools)
	s.MCP = orEmpty(s.MCP)
	s.UntrustedInputs = orEmpty(s.UntrustedInputs)
	s.Subagents = orEmptyMap(s.Subagents)
	return json.Marshal(plain(s))
}

func (s SubagentSpec) MarshalJSON() ([]byte, error) {
	type plain SubagentSpec
	s.Tools = orEmpty(s.Tools)
	return json.Marshal(plain(s))
}

func (u TurnUsage) MarshalJSON() ([]byte, error) {
	type plain TurnUsage
	u.Models = orEmptyMap(u.Models)
	return json.Marshal(plain(u))
}

func (t Totals) MarshalJSON() ([]byte, error) {
	type plain Totals
	t.Models = orEmptyMap(t.Models)
	return json.Marshal(plain(t))
}

func (r TurnResult) MarshalJSON() ([]byte, error) {
	type plain TurnResult
	r.Content = orEmpty(r.Content)
	return json.Marshal(plain(r))
}

func (f HostOpen) MarshalJSON() ([]byte, error) {
	type plain HostOpen
	return marshalTyped(f.FrameType(), plain(f))
}

func (f TurnSend) MarshalJSON() ([]byte, error) {
	type plain TurnSend
	f.Content = orEmpty(f.Content)
	return marshalTyped(f.FrameType(), plain(f))
}

func (f TurnInterrupt) MarshalJSON() ([]byte, error) {
	type plain TurnInterrupt
	return marshalTyped(f.FrameType(), plain(f))
}

func (f HostClose) MarshalJSON() ([]byte, error) {
	type plain HostClose
	return marshalTyped(f.FrameType(), plain(f))
}

func (f TreePurge) MarshalJSON() ([]byte, error) {
	type plain TreePurge
	return marshalTyped(f.FrameType(), plain(f))
}

func (f RPCResult) MarshalJSON() ([]byte, error) {
	type plain RPCResult
	return marshalTyped(f.FrameType(), plain(f))
}

func (f Shutdown) MarshalJSON() ([]byte, error) {
	type plain Shutdown
	return marshalTyped(f.FrameType(), plain(f))
}

func (f Hello) MarshalJSON() ([]byte, error) {
	type plain Hello
	return marshalTyped(f.FrameType(), plain(f))
}

func (f HostState) MarshalJSON() ([]byte, error) {
	type plain HostState
	return marshalTyped(f.FrameType(), plain(f))
}

func (f RPC) MarshalJSON() ([]byte, error) {
	type plain RPC
	return marshalTyped(f.FrameType(), plain(f))
}

// ---- C10 ----

func (s EvalSpec) MarshalJSON() ([]byte, error) {
	type plain EvalSpec
	s.WebfetchDenyDomains = orEmpty(s.WebfetchDenyDomains)
	return json.Marshal(plain(s))
}

func (r EvalResult) MarshalJSON() ([]byte, error) {
	type plain EvalResult
	r.Failures = orEmpty(r.Failures)
	return json.Marshal(plain(r))
}

func (r EvalRun) MarshalJSON() ([]byte, error) {
	type plain EvalRun
	r.Results = orEmpty(r.Results)
	return json.Marshal(plain(r))
}

func (u UsageReconciliation) MarshalJSON() ([]byte, error) {
	type plain UsageReconciliation
	u.Workspaces = orEmpty(u.Workspaces)
	return json.Marshal(plain(u))
}

func (r UsageReconciliationResult) MarshalJSON() ([]byte, error) {
	type plain UsageReconciliationResult
	r.Workspaces = orEmpty(r.Workspaces)
	return json.Marshal(plain(r))
}
