package sessions

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

func TestOneAssistantMessagePerTurn(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	user := h.send(family, ses.ID, "kites?")
	turnID := h.lastSend().TurnID
	final := []protocol.MessagePart{
		{Type: "text", Text: "Let me look. "},
		{Type: "tool", ToolUseID: "tu1", Name: "WebSearch", Summary: "3 results"},
		{Type: "text", Text: "Fly them in 10-20 km/h winds."},
	}
	basis := "list"
	h.link(ses.ID,
		protocol.LinkMessageStarted{Model: haiku},
		protocol.LinkTextDelta{Text: "Let me "},
		protocol.LinkTextDelta{Text: "look. "},
		protocol.LinkToolStarted{ToolUseID: "tu1", Name: "WebSearch", Summary: "kite wind"},
		protocol.LinkToolFinished{ToolUseID: "tu1", Summary: "3 results"},
		protocol.LinkMessageStarted{Model: haiku}, // a repeat must not open a second message
		protocol.LinkTextDelta{Text: "Fly them in "},
		protocol.LinkTextDelta{Text: "10-20 km/h winds."},
		protocol.TurnResult{SDKUUID: "sdk-last", Content: final,
			Usage:  protocol.TurnUsage{Models: map[string]protocol.ModelUsage{}, CostUSD: 0.01},
			Totals: protocol.Totals{Models: map[string]protocol.ModelUsage{}, CostUSD: 0.01}, CostBasis: &basis},
	)
	asst := h.messages(ses.ID, "assistant")
	if len(asst) != 1 {
		t.Fatalf("assistant messages = %d, want exactly 1", len(asst))
	}
	m := asst[0]
	if m.Status != "complete" || m.SDKUUID == nil || *m.SDKUUID != "sdk-last" || m.TurnID != turnID ||
		m.ParentMessageID == nil || *m.ParentMessageID != user.ID || !reflect.DeepEqual(m.Content, final) {
		t.Fatalf("assistant message = %+v", m)
	}
	if n, c := len(h.events(ses.ID, "message.started")), len(h.events(ses.ID, "message.completed")); n != 1 || c != 1 {
		t.Fatalf("message.started = %d, message.completed = %d; want 1 and 1", n, c)
	}
	deltas := h.events(ses.ID, "message.text_delta")
	if len(deltas) != 4 || deltas[0].Body.(protocol.MessageTextDelta).MessageID != m.ID {
		t.Fatalf("deltas = %+v", deltas)
	}
	started := h.events(ses.ID, "tool.started")
	finished := h.events(ses.ID, "tool.finished")
	if len(started) != 1 || len(finished) != 1 ||
		started[0].Body != (protocol.ToolStarted{MessageID: m.ID, ToolUseID: "tu1", Name: "WebSearch", Summary: "kite wind"}) ||
		finished[0].Body != (protocol.ToolFinished{MessageID: m.ID, ToolUseID: "tu1", Summary: "3 results"}) {
		t.Fatalf("tool events = %+v, %+v", started, finished)
	}
	if h.row(ses.ID).Status != "idle" {
		t.Fatalf("session status = %q, want idle", h.row(ses.ID).Status)
	}
}

func TestTurnCompletesWithoutSubscribers(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkTextDelta{Text: "hel"}, result("sdk-1", "hello", 0.01))
	asst := h.messages(ses.ID, "assistant")
	if len(asst) != 1 || asst[0].Status != "complete" {
		t.Fatalf("assistant messages = %+v", asst)
	}
	if _, running := h.svc.RunningTurn(ses.ID); running || h.row(ses.ID).Status != "idle" {
		t.Fatal("the turn did not finish")
	}
	statuses := h.events(ses.ID, "session.status")
	if len(statuses) != 2 || statuses[0].Body.(protocol.SessionStatus).Status != "running" ||
		statuses[1].Body.(protocol.SessionStatus).Status != "idle" {
		t.Fatalf("session.status events = %+v", statuses)
	}
}

func TestMainRequestUsageRoutedToLedger(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	u := protocol.MainRequestUsage{APIMessageID: "msg_api_1", Model: haiku, SLOExclusion: "first", Input: 12, CacheRead: 3400, CacheCreate: 56}
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, u)
	if len(h.led.mains) != 1 || h.led.mains[0] != (ledgerMain{ses.ID, u}) {
		t.Fatalf("ledger main requests = %+v", h.led.mains)
	}
	if len(h.led.turns) != 0 {
		t.Fatal("a main request usage recorded a turn")
	}
}

func TestTurnResultRecordedInLedger(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	turnID := h.lastSend().TurnID
	r := result("sdk-1", "hello", 0.0375)
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, r)
	if len(h.led.turns) != 1 {
		t.Fatalf("ledger turns = %d, want 1", len(h.led.turns))
	}
	got := h.led.turns[0]
	if got.sessionID != ses.ID || got.turnID != turnID || !reflect.DeepEqual(got.usage, r.Usage) || got.costBasis == nil || *got.costBasis != "list" {
		t.Fatalf("ledger turn = %+v", got)
	}
	usage := h.events(ses.ID, "usage")
	if len(usage) != 1 || usage[0].Body != (protocol.Usage{TurnID: turnID, CostUSD: 0.0375}) {
		t.Fatalf("usage events = %+v", usage)
	}
}

func TestThinkingBindingDetailCounted(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "one")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku},
		protocol.LinkError{Code: protocol.CodeUpstreamError, Retryable: true, Detail: "thinking_binding"})
	if got := h.counter("luma_thinking_binding_errors_total"); got != 1 {
		t.Fatalf("thinking binding errors = %v, want 1", got)
	}
	errs := h.events(ses.ID, "error")
	if len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeUpstreamError, Retryable: true}) {
		t.Fatalf("error events = %+v", errs)
	}
	h.send(family, ses.ID, "two")
	h.link(ses.ID, protocol.LinkError{Code: protocol.CodeRateLimited, Retryable: true})
	if got := h.counter("luma_thinking_binding_errors_total"); got != 1 {
		t.Fatalf("thinking binding errors after a plain error = %v, want still 1", got)
	}
	asst := h.messages(ses.ID, "assistant")
	if len(asst) != 2 || asst[0].Status != "error" || *asst[0].ErrorCode != protocol.CodeUpstreamError ||
		asst[1].Status != "error" || *asst[1].ErrorCode != protocol.CodeRateLimited {
		t.Fatalf("assistant messages = %+v", asst)
	}
}

func TestConductorLostFailsRunningTurn(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkTextDelta{Text: "partial"})
	h.svc.OnConductorLost([]string{ses.ID})
	h.flush(ses.ID)
	errs := h.events(ses.ID, "error")
	if len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeConductorRestarted, Retryable: true}) {
		t.Fatalf("error events = %+v", errs)
	}
	asst := h.messages(ses.ID, "assistant")
	if len(asst) != 1 || asst[0].Status != "error" || *asst[0].ErrorCode != protocol.CodeConductorRestarted {
		t.Fatalf("assistant message = %+v", asst)
	}
	if _, running := h.svc.RunningTurn(ses.ID); running || h.row(ses.ID).Status != "idle" {
		t.Fatal("the turn is still running")
	}
	h.send(family, ses.ID, "again")
	if n := len(h.cond.Opens()); n != 2 {
		t.Fatalf("host.open sent %d times, want 2 (the lost link's host is gone)", n)
	}
}

func TestSweepOnStartFailsStuckTurns(t *testing.T) {
	h := newHarness(t)
	streamed := h.newSession(family)
	h.send(family, streamed.ID, "one")
	h.link(streamed.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkTextDelta{Text: "part"})
	queued := h.newSession(family)
	queuedUser := h.send(family, queued.ID, "two")
	done := h.newSession(family)
	h.send(family, done.ID, "three")
	h.finish(done.ID)

	// Core restarts: a new Service over the same store.
	restarted := h.service(&fakeConductor{})
	if err := restarted.SweepOnStart(bg); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{streamed.ID, queued.ID} {
		if st := h.row(id).Status; st != "idle" {
			t.Errorf("session %s status = %q, want idle", id, st)
		}
		errs := h.events(id, "error")
		if len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeConductorRestarted, Retryable: true}) {
			t.Errorf("session %s error events = %+v", id, errs)
		}
		asst := h.messages(id, "assistant")
		if len(asst) != 1 || asst[0].Status != "error" || *asst[0].ErrorCode != protocol.CodeConductorRestarted {
			t.Errorf("session %s assistant messages = %+v", id, asst)
		}
	}
	if asst := h.messages(queued.ID, "assistant"); len(asst) == 1 && asst[0].TurnID != queuedUser.TurnID {
		t.Errorf("the queued turn's error message has turn %q, want %q", asst[0].TurnID, queuedUser.TurnID)
	}
	if n := len(h.events(done.ID, "error")); n != 0 {
		t.Errorf("an idle session got %d error events", n)
	}
}

func TestCapacityClosedFailsTurn(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	h.hostState(protocol.HostState{SessionID: ses.ID, State: "closed", Reason: "capacity"})
	errs := h.events(ses.ID, "error")
	if len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeCapacity, Retryable: true}) {
		t.Fatalf("error events = %+v", errs)
	}
	asst := h.messages(ses.ID, "assistant")
	if len(asst) != 1 || asst[0].Status != "error" || *asst[0].ErrorCode != protocol.CodeCapacity {
		t.Fatalf("assistant messages = %+v", asst)
	}
	if _, running := h.svc.RunningTurn(ses.ID); running || h.row(ses.ID).Status != "idle" {
		t.Fatal("the turn is still running")
	}
	h.send(family, ses.ID, "again")
	if n := len(h.cond.Opens()); n != 2 {
		t.Fatalf("host.open sent %d times, want 2", n)
	}
}

func TestModelSwitchedRecordedAndBound(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku},
		protocol.LinkModelSwitched{From: haiku, To: "claude-opus-5-5"})
	asst := h.messages(ses.ID, "assistant")
	if len(asst) != 1 {
		t.Fatalf("assistant messages = %d", len(asst))
	}
	sw := h.events(ses.ID, "model.switched")
	if len(sw) != 1 || sw[0].Body != (protocol.ModelSwitched{MessageID: asst[0].ID, From: haiku, To: "claude-opus-5-5"}) {
		t.Fatalf("model.switched events = %+v", sw)
	}
	if bound := h.row(ses.ID).BoundModels; !slices.Contains(bound, "claude-opus-5-5") {
		t.Fatalf("bound_models = %v, want the switch target appended", bound)
	}
	h.link(ses.ID, result("sdk-1", "hello", 0.01))
	if m := h.messages(ses.ID, "assistant")[0]; m.Model == nil || *m.Model != "claude-opus-5-5" {
		t.Fatalf("message model = %v, want the switch target", m.Model)
	}
	if n := len(h.cond.Sends()); n != 1 {
		t.Fatalf("turn.sends = %d, want 1 (no retry)", n)
	}
	if h.row(ses.ID).Model != haiku {
		t.Fatal("the session's pinned model changed")
	}
}

func TestTaintFromWebFetchToolStarted(t *testing.T) {
	h := newHarness(t)
	fetch := h.newSession(family)
	h.send(family, fetch.ID, "read example.test")
	h.link(fetch.ID, protocol.LinkMessageStarted{Model: haiku})
	if tainted, _ := h.svc.Tainted(bg, fetch.ID); tainted {
		t.Fatal("tainted before any untrusted tool")
	}
	h.link(fetch.ID, protocol.LinkToolStarted{ToolUseID: "tu1", Name: "WebFetch", Summary: "example.test"})
	if tainted, err := h.svc.Tainted(bg, fetch.ID); err != nil || !tainted || h.row(fetch.ID).TaintedAt == nil {
		t.Fatalf("after WebFetch tool.started: tainted = %v, %v", tainted, err)
	}

	think := h.newSession(family)
	h.send(family, think.ID, "hard sum")
	h.link(think.ID, protocol.LinkMessageStarted{Model: haiku},
		protocol.LinkToolStarted{ToolUseID: "tu2", Name: "deep-think", Summary: "sum"},
		protocol.LinkToolStarted{ToolUseID: "tu3", Name: "mcp__luma__remember", Summary: "likes kites"})
	if tainted, _ := h.svc.Tainted(bg, think.ID); tainted {
		t.Fatal("a tool outside untrusted_inputs tainted the session")
	}
	h.link(think.ID, protocol.LinkToolStarted{ToolUseID: "tu4", Name: "WebSearch", Summary: "kites"})
	if tainted, _ := h.svc.Tainted(bg, think.ID); !tainted {
		t.Fatal("WebSearch did not taint the session")
	}
}

func TestSinkEventsSerialisedWithTurnEvents(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	turnID := h.lastSend().TurnID
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku})

	const n = 100
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		emitted []int64
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range n {
			h.svc.OnLinkEvent(ses.ID, turnID, protocol.LinkTextDelta{Text: fmt.Sprint(i)})
		}
	}()
	sink := h.svc.Sink()
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range n / 4 {
				sid := ses.ID
				id, err := sink.Emit(bg, ses.ID, "memory.write", protocol.MemoryWriteEvent{Write: protocol.MemoryWrite{
					ID: fmt.Sprintf("mem_%d_%d", w, i), SessionID: &sid, TurnID: turnID, Op: "remember",
					Text: "synthetic", Status: "pending", CreatedAt: testNow}})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				emitted = append(emitted, id)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	h.flush(ses.ID)

	all := h.events(ses.ID, "")
	var memIDs []int64
	deltas := 0
	for i, e := range all {
		if i > 0 && e.ID <= all[i-1].ID {
			t.Fatalf("event ids not strictly increasing at %d: %d after %d", i, e.ID, all[i-1].ID)
		}
		switch e.Body.(type) {
		case protocol.MemoryWriteEvent:
			memIDs = append(memIDs, e.ID)
		case protocol.MessageTextDelta:
			deltas++
		}
	}
	slices.Sort(emitted)
	if !slices.Equal(memIDs, emitted) {
		t.Fatalf("stored memory.write ids = %v, want the %d ids Emit returned %v", memIDs, len(emitted), emitted)
	}
	if deltas != n {
		t.Fatalf("text deltas stored = %d, want %d", deltas, n)
	}
}

func TestSinkRejectsForeignPayload(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	if _, err := h.svc.Sink().Emit(bg, ses.ID, "memory.write", map[string]string{"x": "y"}); err == nil {
		t.Fatal("Emit accepted a payload that is not a protocol event body")
	}
	if _, err := h.svc.Sink().Emit(bg, "ses_missing", "session.status", protocol.SessionStatus{Status: "idle"}); err == nil {
		t.Fatal("Emit accepted an unknown session")
	}
	if n := len(h.events(ses.ID, "")); n != 0 {
		t.Fatalf("events = %d, want 0", n)
	}
}
