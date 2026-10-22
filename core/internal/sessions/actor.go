package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// mailboxSize bounds a session's queued work; a full mailbox makes the
// sender (the link's reader, a request) wait.
const mailboxSize = 256

// actor is one session's goroutine. It is the only writer of the session's
// events, messages, status, taint and resume state, so everything a turn does
// happens in order. Its fields below mbox are owned by the goroutine.
type actor struct {
	s       *Service
	id      string
	mbox    chan func(*actor)
	pending int // queued work, guarded by s.mu

	sess     *store.Session     // the row, re-read on every Send
	spec     protocol.AgentSpec // the compiled snapshot of sess
	lastMsg  *string            // the session's newest message id, once known
	hostOpen bool               // a host is open on the current link
	turn     *turn
}

// turn is the running turn. It has at most one assistant message (P23),
// created on message_started.
type turn struct {
	id      string
	user    protocol.Message
	asst    *protocol.Message
	started time.Time
}

func newActor(s *Service, id string) *actor {
	return &actor{s: s, id: id, mbox: make(chan func(*actor), mailboxSize)}
}

func (a *actor) run() {
	for f := range a.mbox {
		f(a)
		a.s.mu.Lock()
		a.pending--
		if a.pending == 0 && !a.hostOpen && a.turn == nil {
			delete(a.s.actors, a.id)
			a.s.mu.Unlock()
			return
		}
		a.s.mu.Unlock()
	}
}

// opContext is the context of one piece of actor work: never cancelled by
// the caller, and marked as this actor's.
func (a *actor) opContext(parent context.Context) context.Context {
	return context.WithValue(context.WithoutCancel(parent), actorKey{}, a)
}

// load reads the session row once per actor lifetime.
func (a *actor) load(ctx context.Context) error {
	if a.sess != nil {
		return nil
	}
	return a.reload(ctx)
}

// reload re-reads the row, which a fork, a rename or an archive may change.
func (a *actor) reload(ctx context.Context) error {
	row, err := a.s.st.Session(ctx, a.id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var spec protocol.AgentSpec
	if err := json.Unmarshal(row.AgentSnapshot, &spec); err != nil {
		return fmt.Errorf("sessions: agent snapshot of %s: %w", a.id, err)
	}
	a.sess, a.spec = &row, spec
	return nil
}

// ---- events ----

// emit appends an event, then fans it out.
func (a *actor) emit(ctx context.Context, body protocol.EventBody, d store.Durability) (int64, error) {
	id, err := a.s.st.AppendEvent(ctx, a.id, body.EventType(), body, d)
	if err != nil {
		return 0, err
	}
	a.s.hub.publish(protocol.Event{ID: id, SessionID: a.id, CreatedAt: a.s.clock(), Body: body})
	return id, nil
}

// note emits an event on a path that cannot return the error (a link event).
func (a *actor) note(ctx context.Context, body protocol.EventBody, d store.Durability) {
	if _, err := a.emit(ctx, body, d); err != nil {
		a.warn("event append failed", err, "type", body.EventType())
	}
}

func (a *actor) warn(what string, err error, kv ...any) {
	args := []any{"session", a.id}
	if a.turn != nil {
		args = append(args, "turn", a.turn.id)
	}
	slog.Warn("sessions: "+what, append(append(args, kv...), "err", err)...)
}

func (a *actor) setStatus(ctx context.Context, status string) error {
	if err := a.s.st.SetSessionStatus(ctx, a.id, status); err != nil {
		return err
	}
	a.sess.Status = status
	_, err := a.emit(ctx, protocol.SessionStatus{Status: status}, store.Durable)
	return err
}

// taint stamps tainted_at once (P9); it is never cleared.
func (a *actor) taint(ctx context.Context) error {
	if a.sess.TaintedAt != nil {
		return nil
	}
	if err := a.s.st.TaintSession(ctx, a.id); err != nil {
		return err
	}
	now := a.s.clock()
	a.sess.TaintedAt = &now
	slog.Info("sessions: tainted", "session", a.id)
	return nil
}

// ---- Send ----

// send runs a user.message: persist, fan out, then the conductor.
func (a *actor) send(ctx context.Context, p auth.Person, um protocol.UserMessage) (protocol.Message, error) {
	if err := a.reload(ctx); err != nil {
		return protocol.Message{}, err
	}
	if a.sess.PersonID != p.ID {
		return protocol.Message{}, ErrNotFound
	}
	msgID := newID("msg_", a.s.clock())
	if um.ClientID != "" {
		stored, inserted, err := a.s.st.RecordClientEvent(ctx, a.id, um.ClientID, msgID)
		if err != nil {
			return protocol.Message{}, err
		}
		if !inserted {
			m, err := a.s.st.Message(ctx, stored)
			if err == nil {
				return m, nil // a repeat: the first send's message, and no new turn
			}
			if !errors.Is(err, store.ErrNotFound) {
				return protocol.Message{}, err
			}
			// An earlier send with this id was refused before its message
			// existed (busy, gate, attachments): this one takes its id.
			msgID = stored
		}
	}
	if refusal := a.s.gate.CheckNewTurn(ctx); refusal != nil {
		return protocol.Message{}, &RefusedError{Info: *refusal}
	}
	if a.turn != nil {
		return protocol.Message{}, ErrBusy
	}

	parts := make([]protocol.MessagePart, 0, len(um.Content)+len(um.AttachmentIDs))
	for _, b := range um.Content {
		if b.Text != "" {
			parts = append(parts, protocol.MessagePart{Type: "text", Text: b.Text})
		}
	}
	var blocks []protocol.ContentBlock
	if len(um.AttachmentIDs) > 0 {
		var err error
		if blocks, err = a.s.att.Bind(ctx, p, a.sess.RootID, um.AttachmentIDs, a.sess.Tier); err != nil {
			return protocol.Message{}, err
		}
		for _, id := range um.AttachmentIDs {
			att, err := a.s.st.Attachment(ctx, id)
			if errors.Is(err, store.ErrNotFound) || (err == nil && att.PersonID != p.ID) {
				return protocol.Message{}, ErrNotFound
			}
			if err != nil {
				return protocol.Message{}, err
			}
			parts = append(parts, protocol.MessagePart{Type: "attachment", ID: att.ID, MediaType: att.MediaType, Filename: att.Filename})
		}
		if err := a.taint(ctx); err != nil {
			return protocol.Message{}, err
		}
	}

	parent, err := a.newestMessage(ctx)
	if err != nil {
		return protocol.Message{}, err
	}
	turnID := newID("turn_", a.s.clock())
	user, err := a.s.st.InsertMessage(ctx, protocol.Message{
		ID: msgID, SessionID: a.id, ParentMessageID: parent, TurnID: turnID,
		Role: "user", Content: parts, Status: "complete",
	})
	if err != nil {
		return protocol.Message{}, err
	}
	a.lastMsg = &user.ID
	a.turn = &turn{id: turnID, user: user, started: a.s.clock()}
	a.s.setRunning(a.id, turnID)
	if err := a.startTurn(ctx, um); err != nil {
		a.warn("turn start failed", err)
		a.failTurn(ctx, protocol.CodeUpstreamError, true)
		return protocol.Message{}, err
	}
	a.deliver(ctx, um, blocks)
	return user, nil
}

// newestMessage is the parent of the next message: the session's newest
// message, or for a fork's first message its fork point.
func (a *actor) newestMessage(ctx context.Context) (*string, error) {
	if a.lastMsg != nil {
		return a.lastMsg, nil
	}
	msgs, err := a.s.st.Messages(ctx, a.id)
	if err != nil {
		return nil, err
	}
	if len(msgs) > 0 {
		a.lastMsg = &msgs[len(msgs)-1].ID
		return a.lastMsg, nil
	}
	return a.sess.ForkAtMessageID, nil
}

// startTurn persists and fans out the turn's start: the title (from the
// first message), message.user, then session.status running.
func (a *actor) startTurn(ctx context.Context, um protocol.UserMessage) error {
	if a.sess.Title == "" {
		text := make([]string, 0, len(um.Content))
		for _, b := range um.Content {
			text = append(text, b.Text)
		}
		if title := titleFrom(strings.Join(text, " ")); title != "" {
			if err := a.s.st.SetSessionTitle(ctx, a.id, title); err != nil {
				return err
			}
			a.sess.Title = title
		}
	}
	if _, err := a.emit(ctx, protocol.MessageUser{Message: a.turn.user}, store.Durable); err != nil {
		return err
	}
	return a.setStatus(ctx, "running")
}

// deliver opens a host if the session has none on this link (lazily, with
// resume once the SDK has seen the session; S2), then sends the turn. A
// failure fails the turn: the message is already safe.
func (a *actor) deliver(ctx context.Context, um protocol.UserMessage, blocks []protocol.ContentBlock) {
	if !a.hostOpen {
		spec, err := a.hostSpec(ctx)
		if err == nil {
			err = a.s.cond.Open(ctx, spec)
		}
		if err != nil {
			a.warn("host.open failed", err)
			a.failTurn(ctx, protocol.CodeConductorRestarted, true)
			return
		}
		a.hostOpen = true
	}
	content := make([]protocol.ContentBlock, 0, len(um.Content)+len(blocks)+1)
	for _, b := range um.Content {
		if b.Text != "" {
			content = append(content, protocol.ContentBlock{Type: "text", Text: b.Text})
		}
	}
	content = append(content, blocks...)
	content = append(content, protocol.ContentBlock{Type: "text", Text: NowContext(a.s.clock(), a.s.cfg.Location)})
	ts := protocol.TurnSend{SessionID: a.id, TurnID: a.turn.id, Content: content, Untrusted: a.sess.TaintedAt != nil}
	if err := a.s.cond.SendTurn(ctx, ts); err != nil {
		a.warn("turn.send failed", err)
		a.failTurn(ctx, protocol.CodeConductorRestarted, true)
	}
}

// hostSpec is the HostSpec of the session as stored: the opaque person ref
// (never a name or registry id), the snapshotted agent and helper model, the
// fenced profile snapshot, and resume (S2).
func (a *actor) hostSpec(ctx context.Context) (protocol.HostSpec, error) {
	ref, err := a.s.st.PersonRef(ctx, a.sess.PersonID)
	if err != nil {
		return protocol.HostSpec{}, err
	}
	return protocol.HostSpec{
		SessionID: a.id, RootID: a.sess.RootID, Person: protocol.HostPerson{Ref: ref}, Agent: a.spec,
		Model: a.sess.Model, HelperModel: a.sess.HelperModel, Effort: a.sess.Effort, CacheTTL: a.sess.CacheTTL,
		Workspace: a.sess.APIWorkspace, Source: a.sess.Source, Timezone: a.s.cfg.Location.String(),
		ProfileContext: a.sess.ProfileSnapshot, WebfetchDenyDomains: slices.Clone(a.s.cfg.WebFetchDenyDomains),
		Resume: a.resume(),
	}, nil
}

// resume is set on every open of a session the SDK has reported, with the
// last totals as the (informational, S31) baseline; null for a fresh one.
func (a *actor) resume() *protocol.Resume {
	if a.sess.SDKSessionID == nil {
		return nil
	}
	r := &protocol.Resume{SDKSessionID: *a.sess.SDKSessionID}
	if a.sess.LastTotals != nil {
		t := *a.sess.LastTotals
		r.BaselineTotals = &t
	}
	return r
}

func (a *actor) interrupt(ctx context.Context, p auth.Person) error {
	if err := a.reload(ctx); err != nil {
		return err
	}
	if a.sess.PersonID != p.ID {
		return ErrNotFound
	}
	if a.turn == nil {
		return nil
	}
	return a.s.cond.Interrupt(ctx, a.id)
}

// ---- the conductor's side of a turn ----

func (a *actor) onLink(ctx context.Context, turnID string, ev protocol.LinkEvent) {
	if a.turn == nil || a.turn.id != turnID {
		slog.Debug("sessions: dropped a link event for a turn that is not running", "session", a.id, "turn", turnID, "type", ev.LinkEventType())
		return
	}
	switch e := ev.(type) {
	case protocol.LinkMessageStarted:
		a.assistant(ctx, e.Model)
	case protocol.LinkTextDelta:
		a.textDelta(ctx, e.Text)
	case protocol.LinkToolStarted:
		a.toolStarted(ctx, e)
	case protocol.LinkToolFinished:
		a.toolFinished(ctx, e)
	case protocol.MainRequestUsage:
		a.s.led.RecordMainRequest(*a.sess, e)
	case protocol.LinkModelSwitched:
		a.modelSwitched(ctx, e)
	case protocol.TurnResult:
		a.turnResult(ctx, e)
	case protocol.LinkError:
		if e.Detail == "thinking_binding" {
			a.s.m.ThinkingBindingErrorsTotal.Inc()
		}
		slog.Info("sessions: turn error", "session", a.id, "turn", turnID, "code", e.Code, "detail", e.Detail)
		a.failTurn(ctx, e.Code, e.Retryable)
	default:
		slog.Warn("sessions: unknown link event", "session", a.id, "type", ev.LinkEventType())
	}
}

// assistant returns the turn's one assistant message, creating it (and
// emitting message.started) on the first call: message_started, or any event
// that needs the message first.
func (a *actor) assistant(ctx context.Context, model string) *protocol.Message {
	t := a.turn
	if t.asst != nil {
		return t.asst
	}
	if model == "" {
		model = a.sess.Model
	}
	m := protocol.Message{
		ID: newID("msg_", a.s.clock()), SessionID: a.id, ParentMessageID: &t.user.ID, TurnID: t.id,
		Role: "assistant", Content: []protocol.MessagePart{}, Model: &model, Status: "running",
	}
	if stored, err := a.s.st.InsertMessage(ctx, m); err != nil {
		a.warn("assistant message insert failed", err)
	} else {
		m = stored
	}
	t.asst = &m
	a.lastMsg = &m.ID
	a.note(ctx, protocol.MessageStarted{MessageID: m.ID, Model: model}, store.Durable)
	return t.asst
}

func (a *actor) textDelta(ctx context.Context, text string) {
	m := a.assistant(ctx, "")
	if n := len(m.Content); n > 0 && m.Content[n-1].Type == "text" {
		m.Content[n-1].Text += text
	} else {
		m.Content = append(m.Content, protocol.MessagePart{Type: "text", Text: text})
	}
	a.note(ctx, protocol.MessageTextDelta{MessageID: m.ID, Text: text}, store.Relaxed)
}

func (a *actor) toolStarted(ctx context.Context, e protocol.LinkToolStarted) {
	if slices.Contains(a.spec.UntrustedInputs, e.Name) {
		if err := a.taint(ctx); err != nil {
			a.warn("taint failed", err, "tool", e.Name)
		}
	}
	m := a.assistant(ctx, "")
	m.Content = append(m.Content, protocol.MessagePart{Type: "tool", ToolUseID: e.ToolUseID, Name: e.Name, Summary: e.Summary})
	a.note(ctx, protocol.ToolStarted{MessageID: m.ID, ToolUseID: e.ToolUseID, Name: e.Name, Summary: e.Summary}, store.Durable)
}

func (a *actor) toolFinished(ctx context.Context, e protocol.LinkToolFinished) {
	m := a.assistant(ctx, "")
	for i := range m.Content {
		if p := &m.Content[i]; p.Type == "tool" && p.ToolUseID == e.ToolUseID {
			p.IsError = e.IsError
			if e.Summary != "" {
				p.Summary = e.Summary
			}
		}
	}
	a.note(ctx, protocol.ToolFinished{MessageID: m.ID, ToolUseID: e.ToolUseID, IsError: e.IsError, Summary: e.Summary}, store.Durable)
}

// modelSwitched records a CLI model switch: accepted, never retried; the
// message carries the new model and it joins bound_models (P29).
func (a *actor) modelSwitched(ctx context.Context, e protocol.LinkModelSwitched) {
	m := a.assistant(ctx, e.From)
	to := e.To
	m.Model = &to
	if err := a.s.st.FinishMessage(ctx, *m); err != nil {
		a.warn("message model update failed", err)
	}
	if err := a.s.st.AddBoundModel(ctx, a.id, to); err != nil {
		a.warn("bound model append failed", err)
	} else if !slices.Contains(a.sess.BoundModels, to) {
		a.sess.BoundModels = append(a.sess.BoundModels, to)
	}
	slog.Info("sessions: model switched", "session", a.id, "turn", a.turn.id, "from", e.From, "to", e.To)
	a.note(ctx, protocol.ModelSwitched{MessageID: m.ID, From: e.From, To: e.To}, store.Durable)
}

// turnResult completes the turn: the message and last_totals in one
// transaction (S2), message.completed, usage, the ledger, then idle.
func (a *actor) turnResult(ctx context.Context, r protocol.TurnResult) {
	m := a.assistant(ctx, "")
	if len(r.Content) > 0 {
		m.Content = r.Content
	}
	if r.SDKUUID != "" {
		u := r.SDKUUID
		m.SDKUUID = &u
	}
	m.Status = "complete"
	if err := a.s.st.CompleteTurn(ctx, *m, r.Totals); err != nil {
		a.warn("turn completion failed", err)
	} else {
		t := r.Totals
		a.sess.LastTotals = &t
	}
	a.note(ctx, protocol.MessageCompleted{Message: *m}, store.Durable)
	a.note(ctx, protocol.Usage{TurnID: a.turn.id, CostUSD: r.Usage.CostUSD}, store.Durable)
	if err := a.s.led.RecordTurn(ctx, *a.sess, a.turn.id, r.Usage, r.CostBasis); err != nil {
		a.warn("ledger record failed", err)
	}
	a.endTurn(ctx, "complete")
}

// failTurn ends the running turn with an error the client sees as
// error{code, retryable}. The turn's assistant message (created if the
// conductor never started one) is marked error with the code.
func (a *actor) failTurn(ctx context.Context, code protocol.ErrorCode, retryable bool) {
	t := a.turn
	if t == nil {
		return
	}
	c := code
	switch {
	case t.user.ID == "":
		// A recovered turn without a user message: nothing to mark.
	case t.asst == nil:
		m := protocol.Message{
			ID: newID("msg_", a.s.clock()), SessionID: a.id, ParentMessageID: &t.user.ID, TurnID: t.id,
			Role: "assistant", Content: []protocol.MessagePart{}, Status: "error", ErrorCode: &c,
		}
		if stored, err := a.s.st.InsertMessage(ctx, m); err != nil {
			a.warn("error message insert failed", err)
		} else {
			m = stored
		}
		t.asst = &m
		a.lastMsg = &m.ID
		a.note(ctx, protocol.MessageCompleted{Message: m}, store.Durable)
	case t.asst.Status == "running":
		t.asst.Status, t.asst.ErrorCode = "error", &c
		if err := a.s.st.FinishMessage(ctx, *t.asst); err != nil {
			a.warn("error message update failed", err)
		}
		a.note(ctx, protocol.MessageCompleted{Message: *t.asst}, store.Durable)
	}
	a.note(ctx, protocol.ErrorEvent{Code: code, Retryable: retryable}, store.Durable)
	a.endTurn(ctx, "error")
}

func (a *actor) endTurn(ctx context.Context, outcome string) {
	t := a.turn
	a.turn = nil
	a.s.setRunning(a.id, "")
	if err := a.setStatus(ctx, "idle"); err != nil {
		a.warn("status idle failed", err)
	}
	a.s.m.TurnsTotal.WithLabelValues(a.sess.AgentID, a.sess.Tier, a.sess.Source, outcome).Inc()
	if !t.started.IsZero() {
		a.s.m.TurnDurationSeconds.WithLabelValues(a.sess.AgentID, a.sess.Tier).Observe(a.s.clock().Sub(t.started).Seconds())
	}
	slog.Info("sessions: turn ended", "session", a.id, "turn", t.id, "outcome", outcome)
}

// ---- hosts and restarts ----

func (a *actor) onHostState(ctx context.Context, hs protocol.HostState) {
	if err := a.load(ctx); err != nil {
		a.warn("host.state for an unknown session", err)
		return
	}
	if id := hs.SDKSessionID; id != "" && (a.sess.SDKSessionID == nil || *a.sess.SDKSessionID != id) {
		if err := a.s.st.SetSDKSessionID(ctx, a.id, id); err != nil {
			a.warn("sdk_session_id update failed", err)
		} else {
			a.sess.SDKSessionID = &id
		}
	}
	switch hs.State {
	case "ready":
		a.hostOpen = true
	case "closed":
		a.hostOpen = false
		if a.turn != nil {
			slog.Info("sessions: host closed under a turn", "session", a.id, "turn", a.turn.id, "reason", hs.Reason)
			a.failTurn(ctx, closedCode(hs.Reason), true)
		}
	}
}

// closedCode is the client error for a host that closed under a pending turn.
func closedCode(reason string) protocol.ErrorCode {
	switch reason {
	case "capacity":
		return protocol.CodeCapacity
	case "maintenance":
		return protocol.CodeMaintenance
	case "shutdown":
		return protocol.CodeConductorRestarted
	}
	return protocol.CodeUpstreamError
}

func (a *actor) onConductorLost(ctx context.Context) {
	a.hostOpen = false
	a.failTurn(ctx, protocol.CodeConductorRestarted, true)
}

// sweep fails a turn a previous Core left running. The turn is rebuilt from
// the store: the newest user message and its assistant message, if any.
func (a *actor) sweep(ctx context.Context) error {
	if err := a.reload(ctx); err != nil {
		return err
	}
	if a.sess.Status != "running" || a.turn != nil {
		return nil
	}
	msgs, err := a.s.st.Messages(ctx, a.id)
	if err != nil {
		return err
	}
	t := &turn{}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			t.id, t.user = msgs[i].TurnID, msgs[i]
			break
		}
	}
	for i := range msgs {
		if m := msgs[i]; t.id != "" && m.Role == "assistant" && m.TurnID == t.id {
			t.asst = &m
		}
	}
	if len(msgs) > 0 {
		a.lastMsg = &msgs[len(msgs)-1].ID
	}
	a.turn = t
	a.failTurn(ctx, protocol.CodeConductorRestarted, true)
	return nil
}
