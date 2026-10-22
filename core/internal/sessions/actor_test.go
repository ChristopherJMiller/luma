package sessions

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
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
		protocol.LinkError{Code: protocol.CodeUpstreamError, Retryable: true, Detail: "thinking_binding"},
		result("sdk-1", "", 0))
	if got := h.counter("luma_thinking_binding_errors_total"); got != 1 {
		t.Fatalf("thinking binding errors = %v, want 1", got)
	}
	errs := h.events(ses.ID, "error")
	if len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeUpstreamError, Retryable: true}) {
		t.Fatalf("error events = %+v", errs)
	}
	h.send(family, ses.ID, "two")
	h.link(ses.ID, protocol.LinkError{Code: protocol.CodeRateLimited, Retryable: true}, result("sdk-2", "", 0))
	if got := h.counter("luma_thinking_binding_errors_total"); got != 1 {
		t.Fatalf("thinking binding errors after a plain error = %v, want still 1", got)
	}
	asst := h.messages(ses.ID, "assistant")
	if len(asst) != 2 || asst[0].Status != "error" || *asst[0].ErrorCode != protocol.CodeUpstreamError ||
		asst[1].Status != "error" || *asst[1].ErrorCode != protocol.CodeRateLimited {
		t.Fatalf("assistant messages = %+v", asst)
	}
}

// A link error marks the message and tells the client, but the turn ends on
// its turn_result, whose spend is ledgered like any other (S45).
func TestErrorThenTurnResultIsLedgered(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	turnID := h.lastSend().TurnID
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkTextDelta{Text: "partial"},
		protocol.LinkError{Code: protocol.CodeRateLimited, Retryable: true})
	if _, running := h.svc.RunningTurn(ses.ID); !running || h.row(ses.ID).Status != "running" {
		t.Fatal("a link error ended the turn; it ends on turn_result")
	}
	if asst := h.messages(ses.ID, "assistant"); len(asst) != 1 || asst[0].Status != "error" || *asst[0].ErrorCode != protocol.CodeRateLimited {
		t.Fatalf("assistant message after the error = %+v", asst)
	}
	r := result("sdk-after-error", "partial", 0.004)
	h.link(ses.ID, r)
	m := h.messages(ses.ID, "assistant")[0]
	if m.Status != "error" || *m.ErrorCode != protocol.CodeRateLimited || m.SDKUUID == nil || *m.SDKUUID != "sdk-after-error" {
		t.Fatalf("assistant message = %+v; want status error, its code, and the result's sdk_uuid", m)
	}
	if len(h.led.turns) != 1 || h.led.turns[0].turnID != turnID || !reflect.DeepEqual(h.led.turns[0].usage, r.Usage) {
		t.Fatalf("ledger turns = %+v, want the failed turn's spend", h.led.turns)
	}
	if got := h.row(ses.ID).LastTotals; got == nil || !reflect.DeepEqual(*got, r.Totals) {
		t.Fatalf("last_totals = %+v", got)
	}
	if _, running := h.svc.RunningTurn(ses.ID); running || h.row(ses.ID).Status != "idle" {
		t.Fatal("turn_result did not end the turn")
	}
	completed := h.events(ses.ID, "message.completed")
	if len(completed) != 1 || completed[0].Body.(protocol.MessageCompleted).Message.Status != "error" {
		t.Fatalf("message.completed = %+v", completed)
	}
	if n := len(h.events(ses.ID, "error")); n != 1 {
		t.Fatalf("error events = %d, want 1", n)
	}
}

// When no result will come, the host closing ends an errored turn; there is
// no spend to ledger and the client is not told twice.
func TestErrorThenHostCloseEndsTurn(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkError{Code: protocol.CodeOverloaded, Retryable: true})
	h.hostState(protocol.HostState{SessionID: ses.ID, State: "closed", Reason: "error"})
	if _, running := h.svc.RunningTurn(ses.ID); running || h.row(ses.ID).Status != "idle" {
		t.Fatal("the turn is still running after its host closed")
	}
	if len(h.led.turns) != 0 {
		t.Fatalf("ledger turns = %+v, want none", h.led.turns)
	}
	if asst := h.messages(ses.ID, "assistant"); len(asst) != 1 || asst[0].Status != "error" || *asst[0].ErrorCode != protocol.CodeOverloaded {
		t.Fatalf("assistant message = %+v, want the link error's code kept", asst)
	}
	errs := h.events(ses.ID, "error")
	if len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeOverloaded, Retryable: true}) {
		t.Fatalf("error events = %+v, want only the link's", errs)
	}
	if completed := h.events(ses.ID, "message.completed"); len(completed) != 1 {
		t.Fatalf("message.completed events = %d, want 1", len(completed))
	}
}

// A store failure on the assistant message fails the turn instead of
// completing it (minor 7): no message.completed with status complete.
func TestStoreFailureFailsTurnNotCompletes(t *testing.T) {
	h := newHarness(t)
	h.sql(`create function luma_fail() returns trigger language plpgsql as $$ begin raise exception 'injected failure'; end $$`)

	// The assistant message cannot be inserted: the client is told, the CLI's
	// turn is interrupted, and the turn waits for its result to ledger it
	// (review round 2, minor 1).
	h.sql(`create trigger fail_asst_insert before insert on messages for each row when (new.role = 'assistant') execute function luma_fail()`)
	a := h.newSession(family)
	h.send(family, a.ID, "one")
	h.link(a.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkTextDelta{Text: "lost"})
	if _, running := h.svc.RunningTurn(a.ID); !running || h.row(a.ID).Status != "running" {
		t.Fatal("the turn went idle before its result")
	}
	if n := len(h.events(a.ID, "message.started")); n != 0 {
		t.Fatalf("message.started emitted for a message that was not stored (%d)", n)
	}
	if errs := h.events(a.ID, "error"); len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeUpstreamError, Retryable: true}) {
		t.Fatalf("error events = %+v", errs)
	}
	if got := h.cond.Interrupts(); !reflect.DeepEqual(got, []string{a.ID}) {
		t.Fatalf("interrupts = %v, want the abandoned turn interrupted once", got)
	}
	ra := result("sdk-a", "lost", 0.02)
	h.link(a.ID, ra)
	if len(h.led.turns) != 1 || h.led.turns[0].sessionID != a.ID || !reflect.DeepEqual(h.led.turns[0].usage, ra.Usage) {
		t.Fatalf("ledger turns = %+v, want the abandoned turn's spend", h.led.turns)
	}
	if row := h.row(a.ID); row.Status != "idle" || row.LastTotals == nil || !reflect.DeepEqual(*row.LastTotals, ra.Totals) {
		t.Fatalf("after the result: status %q, last_totals %+v", row.Status, row.LastTotals)
	}
	if asst := h.messages(a.ID, "assistant"); len(asst) != 0 {
		t.Fatalf("assistant messages = %+v, want none stored", asst)
	}
	// Without a result, a host close ends it, with nothing ledgered.
	c := h.newSession(family)
	h.send(family, c.ID, "three")
	h.link(c.ID, protocol.LinkMessageStarted{Model: haiku})
	h.hostState(protocol.HostState{SessionID: c.ID, State: "closed", Reason: "error"})
	if _, running := h.svc.RunningTurn(c.ID); running || h.row(c.ID).Status != "idle" || len(h.led.turns) != 1 {
		t.Fatal("a host close did not end the abandoned turn, or something was ledgered")
	}
	h.sql(`drop trigger fail_asst_insert on messages`)

	// The completion cannot be written.
	b := h.newSession(family)
	h.send(family, b.ID, "two")
	h.link(b.ID, protocol.LinkMessageStarted{Model: haiku})
	h.sql(`create trigger fail_complete before update on messages for each row when (new.status = 'complete') execute function luma_fail()`)
	h.link(b.ID, result("sdk-b", "hello", 0.01))
	for _, e := range h.events(b.ID, "message.completed") {
		if st := e.Body.(protocol.MessageCompleted).Message.Status; st == "complete" {
			t.Fatal("message.completed{complete} emitted for a completion that was not stored")
		}
	}
	if errs := h.events(b.ID, "error"); len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeUpstreamError, Retryable: true}) {
		t.Fatalf("error events = %+v", errs)
	}
	if asst := h.messages(b.ID, "assistant"); len(asst) != 1 || asst[0].Status != "error" {
		t.Fatalf("assistant message = %+v, want error", asst)
	}
	if len(h.led.turns) != 2 || h.led.turns[1].sessionID != b.ID {
		t.Fatalf("ledger turns = %+v, want the real spend recorded", h.led.turns)
	}
	if row := h.row(b.ID); row.LastTotals != nil || row.Status != "idle" {
		t.Fatalf("row last_totals = %+v, status %q; want none (rolled back) and idle", row.LastTotals, row.Status)
	}
}

// A taint that could not be written stays in force in memory, is retried at
// the next turn's start, and fails that turn closed if it still cannot be
// written (S46).
func TestTaintWriteFailureIsStickyAndFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.sql(`create function luma_fail() returns trigger language plpgsql as $$ begin raise exception 'injected failure'; end $$`)
	h.sql(`create trigger fail_taint before update on sessions for each row when (old.tainted_at is distinct from new.tainted_at) execute function luma_fail()`)
	ses := h.newSession(family)
	h.send(family, ses.ID, "fetch it")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkToolStarted{ToolUseID: "tu1", Name: "WebFetch", Summary: "example.test"})
	if h.row(ses.ID).TaintedAt != nil {
		t.Fatal("the injected failure did not stop the taint write")
	}
	if tainted, err := h.svc.Tainted(bg, ses.ID); err != nil || !tainted {
		t.Fatalf("Tainted = %v, %v; want true from memory", tainted, err)
	}
	h.link(ses.ID, result("sdk-1", "done", 0.01))
	h.hostState(protocol.HostState{SessionID: ses.ID, State: "closed", Reason: "idle"})

	sends := len(h.cond.Sends())
	if _, err := h.svc.Send(bg, family, ses.ID, userMsg("again", newClientID())); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("Send with an unwritable taint: err = %v, want the turn failed closed", err)
	}
	if n := len(h.cond.Sends()); n != sends {
		t.Fatalf("turn.sends went from %d to %d; a turn ran with its taint unwritten", sends, n)
	}
	if tainted, _ := h.svc.Tainted(bg, ses.ID); !tainted {
		t.Fatal("the in-memory taint was lost")
	}

	h.sql(`drop trigger fail_taint on sessions`)
	h.send(family, ses.ID, "third")
	if h.row(ses.ID).TaintedAt == nil || !h.lastSend().Untrusted {
		t.Fatal("the retried taint was not written, or the turn was not untrusted")
	}
}

// The conductor's untrusted flag taints through the actor (S47).
func TestTaintFromConductorFlag(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	if err := h.svc.Taint(bg, ses.ID); err != nil {
		t.Fatal(err)
	}
	if tainted, err := h.svc.Tainted(bg, ses.ID); err != nil || !tainted || h.row(ses.ID).TaintedAt == nil {
		t.Fatalf("after Taint: tainted = %v, %v", tainted, err)
	}
	h.send(family, ses.ID, "hi")
	if !h.lastSend().Untrusted {
		t.Fatal("the turn after Taint was not untrusted")
	}
	if err := h.svc.Taint(bg, "ses_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Taint of an unknown session = %v, want ErrNotFound", err)
	}
}

// S48: no turn_result 120 s after a link error → host.close{error} and the
// turn fails; the message keeps the error the client was already told.
func TestErrorWithoutResultClosesHostAfter120s(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkError{Code: protocol.CodeOverloaded, Retryable: true})
	h.advance(ses.ID, 119*time.Second)
	if _, running := h.svc.RunningTurn(ses.ID); !running || len(h.cond.Closes()) != 0 {
		t.Fatal("the turn was given up before 120 s")
	}
	h.advance(ses.ID, time.Second)
	if _, running := h.svc.RunningTurn(ses.ID); running || h.row(ses.ID).Status != "idle" {
		t.Fatal("the turn is still open 120 s after its error")
	}
	if got := h.cond.Closes(); !reflect.DeepEqual(got, []string{ses.ID + ":error"}) {
		t.Fatalf("host.close calls = %v, want one with reason error", got)
	}
	if asst := h.messages(ses.ID, "assistant"); len(asst) != 1 || asst[0].Status != "error" || *asst[0].ErrorCode != protocol.CodeOverloaded {
		t.Fatalf("assistant message = %+v", asst)
	}
	if n, c := len(h.events(ses.ID, "error")), len(h.events(ses.ID, "message.completed")); n != 1 || c != 1 || len(h.led.turns) != 0 {
		t.Fatalf("error events %d, message.completed %d, ledger turns %d; want 1, 1, 0", n, c, len(h.led.turns))
	}
	h.send(family, ses.ID, "again")
	if n := len(h.cond.Opens()); n != 2 {
		t.Fatalf("host.open sent %d times, want 2 (Core closed the host)", n)
	}
}

// S48: a turn running LUMA_TURN_MAX_MINUTES (default 15) is interrupted; no
// turn_result 30 s later → host.close{error} and upstream_error.
func TestTurnWallClockCapInterruptsThenFails(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "a long one")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku})
	h.advance(ses.ID, 15*time.Minute-time.Second)
	if n := len(h.cond.Interrupts()); n != 0 {
		t.Fatalf("interrupted %d times before the cap", n)
	}
	h.advance(ses.ID, time.Second)
	if got := h.cond.Interrupts(); !reflect.DeepEqual(got, []string{ses.ID}) {
		t.Fatalf("interrupts at the cap = %v", got)
	}
	h.advance(ses.ID, 29*time.Second)
	if _, running := h.svc.RunningTurn(ses.ID); !running {
		t.Fatal("the turn failed before the 30 s grace ran out")
	}
	h.advance(ses.ID, time.Second)
	if _, running := h.svc.RunningTurn(ses.ID); running || h.row(ses.ID).Status != "idle" {
		t.Fatal("the turn is still open after the interrupt grace")
	}
	if errs := h.events(ses.ID, "error"); len(errs) != 1 || errs[0].Body != (protocol.ErrorEvent{Code: protocol.CodeUpstreamError, Retryable: true}) {
		t.Fatalf("error events = %+v", errs)
	}
	if got := h.cond.Closes(); !reflect.DeepEqual(got, []string{ses.ID + ":error"}) {
		t.Fatalf("host.close calls = %v", got)
	}

	// The cap comes from Config.TurnMax (LUMA_TURN_MAX_MINUTES).
	cond := &fakeConductor{}
	short := h.service(cond, func(c *Config) { c.TurnMax = 5 * time.Minute })
	s2, err := short.Create(bg, family, protocol.CreateSession{Agent: "luma"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := short.Send(bg, family, s2.ID, userMsg("hi", newClientID())); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(5 * time.Minute)
	if err := short.call(bg, s2.ID, func(context.Context, *actor) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := cond.Interrupts(); !reflect.DeepEqual(got, []string{s2.ID}) {
		t.Fatalf("interrupts with a 5 minute cap = %v", got)
	}
}

// A turn_result in time cancels the backstops.
func TestTurnCapCancelledByResult(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "one")
	h.advance(ses.ID, 15*time.Minute)
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, result("sdk-1", "made it", 0.01))
	h.advance(ses.ID, time.Minute)
	if errs := h.events(ses.ID, "error"); len(errs) != 0 || len(h.cond.Closes()) != 0 {
		t.Fatalf("a turn that finished inside its grace got errors %+v and closes %v", errs, h.cond.Closes())
	}
	if asst := h.messages(ses.ID, "assistant"); len(asst) != 1 || asst[0].Status != "complete" {
		t.Fatalf("assistant message = %+v", asst)
	}
	h.send(family, ses.ID, "two")
	h.link(ses.ID, protocol.LinkError{Code: protocol.CodeRateLimited, Retryable: true}, result("sdk-2", "", 0))
	h.advance(ses.ID, 20*time.Minute)
	if n, c := len(h.cond.Interrupts()), len(h.cond.Closes()); n != 1 || c != 0 {
		t.Fatalf("interrupts %d, closes %d after finished turns; their timers must stop", n, c)
	}
}

// A link error whose message cannot be stored leaves no message `running`:
// the turn waits for its result without retrying the insert (round 2,
// minor 2). The first assistant insert fails, later ones would succeed.
func TestErrorInsertFailureLeavesNoRunningMessage(t *testing.T) {
	h := newHarness(t)
	h.sql(`create sequence luma_fail_once`)
	h.sql(`grant usage on sequence luma_fail_once to public`) // the store's role runs the trigger
	h.sql(`create function luma_fail_first() returns trigger language plpgsql as $$
		begin if nextval('luma_fail_once') = 1 then raise exception 'injected failure'; end if; return new; end $$`)
	h.sql(`create trigger fail_first_asst before insert on messages for each row when (new.role = 'assistant') execute function luma_fail_first()`)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	h.link(ses.ID, protocol.LinkError{Code: protocol.CodeOverloaded, Retryable: true},
		protocol.LinkTextDelta{Text: "late"}, result("sdk-1", "late", 0.01))
	for _, m := range h.messages(ses.ID, "assistant") {
		if m.Status == "running" {
			t.Fatalf("assistant message left running: %+v", m)
		}
	}
	if len(h.led.turns) != 1 || h.row(ses.ID).Status != "idle" || len(h.events(ses.ID, "error")) != 1 {
		t.Fatalf("ledger %d, status %q, error events %d; want 1, idle, 1",
			len(h.led.turns), h.row(ses.ID).Status, len(h.events(ses.ID, "error")))
	}
}

// Tainting a session deleted under its turn is nothing to taint: it must not
// pin the actor with an unsaved taint (round 2, minor 4).
func TestTaintOfDeletedSessionDoesNotPinActor(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "fetch it")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku})
	if err := h.st.DeleteTree(bg, family.ID, ses.ID); err != nil {
		t.Fatal(err)
	}
	h.link(ses.ID, protocol.LinkToolStarted{ToolUseID: "tu1", Name: "WebFetch", Summary: "example.test"})
	h.svc.OnConductorLost([]string{ses.ID})
	h.flush(ses.ID)
	deadline := time.Now().Add(3 * time.Second)
	for h.actorCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the deleted session's actor is still alive")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An unsaved taint is retried when the turn ends too (round 2, minor 5).
func TestUnsavedTaintRetriedWhenTurnEnds(t *testing.T) {
	h := newHarness(t)
	h.sql(`create function luma_fail() returns trigger language plpgsql as $$ begin raise exception 'injected failure'; end $$`)
	h.sql(`create trigger fail_taint before update on sessions for each row when (old.tainted_at is distinct from new.tainted_at) execute function luma_fail()`)
	ses := h.newSession(family)
	h.send(family, ses.ID, "fetch it")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkToolStarted{ToolUseID: "tu1", Name: "WebFetch", Summary: "example.test"})
	if h.row(ses.ID).TaintedAt != nil {
		t.Fatal("the injected failure did not stop the taint write")
	}
	h.sql(`drop trigger fail_taint on sessions`)
	h.link(ses.ID, result("sdk-1", "done", 0.01))
	if h.row(ses.ID).TaintedAt == nil {
		t.Fatal("the unsaved taint was not stored when the turn ended")
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
	// Core died after completing the message but before going idle.
	finished := h.newSession(family)
	h.send(family, finished.ID, "four")
	h.finish(finished.ID)
	if err := h.st.SetSessionStatus(bg, finished.ID, "running"); err != nil {
		t.Fatal(err)
	}
	// Core died after a link error, waiting for the turn_result (S45).
	errored := h.newSession(family)
	h.send(family, errored.ID, "five")
	h.link(errored.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkError{Code: protocol.CodeOverloaded, Retryable: true})

	// Core restarts: a new Service over the same store.
	restarted := h.service(&fakeConductor{})
	if err := restarted.SweepOnStart(bg); err != nil {
		t.Fatal(err)
	}
	// A turn whose message already settled only goes idle: no restart error.
	// Each still ends with exactly one message.completed (the errored one's
	// comes from the sweep).
	for _, c := range []struct {
		id     string
		errs   int
		status string
	}{{finished.ID, 0, "complete"}, {errored.ID, 1, "error"}} {
		if completed := h.events(c.id, "message.completed"); len(completed) != 1 ||
			completed[0].Body.(protocol.MessageCompleted).Message.Status != c.status {
			t.Errorf("session %s message.completed = %+v, want one with status %s", c.id, completed, c.status)
		}
		if st := h.row(c.id).Status; st != "idle" {
			t.Errorf("session %s status = %q, want idle", c.id, st)
		}
		if n := len(h.events(c.id, "error")); n != c.errs {
			t.Errorf("session %s error events = %d, want %d (none from the sweep)", c.id, n, c.errs)
		}
		if asst := h.messages(c.id, "assistant"); len(asst) != 1 || asst[0].Status != c.status {
			t.Errorf("session %s assistant messages = %+v, want one left %s", c.id, asst, c.status)
		}
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

// One session the sweep cannot handle does not stop it (minor 9).
func TestSweepOnStartContinuesPastABadSession(t *testing.T) {
	h := newHarness(t)
	bad := h.newSession(family)
	h.send(family, bad.ID, "one")
	good := h.newSession(family)
	h.send(family, good.ID, "two")
	h.sql(`update sessions set agent_snapshot = '[1]' where id = '` + bad.ID + `'`) // no longer decodes
	if err := h.service(&fakeConductor{}).SweepOnStart(bg); err != nil {
		t.Fatalf("SweepOnStart = %v, want it to log the bad session and go on", err)
	}
	if st := h.row(good.ID).Status; st != "idle" {
		t.Fatalf("the good session's status = %q, want idle", st)
	}
	if st := h.row(bad.ID).Status; st != "running" {
		t.Fatalf("the bad session's status = %q; it could not be swept", st)
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

// Sink emits and turn events are written by the session's one actor: no two
// appends of the session ever overlap, and every stored event was appended
// through that path. Each append is slowed so an overlap would show.
func TestSinkEventsSerialisedWithTurnEvents(t *testing.T) {
	h := newHarness(t)
	var inflight, maxInflight, appends atomic.Int64
	direct := h.svc.appendEvent
	h.svc.appendEvent = func(ctx context.Context, sessionID, typ string, payload any, d store.Durability) (int64, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			if m := maxInflight.Load(); n <= m || maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
		appends.Add(1)
		time.Sleep(200 * time.Microsecond)
		return direct(ctx, sessionID, typ, payload, d)
	}
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
	if got := maxInflight.Load(); got != 1 {
		t.Fatalf("%d appends of the session overlapped; the actor is its only writer", got)
	}
	if got := appends.Load(); got != int64(len(all)) {
		t.Fatalf("appends through the actor = %d, stored events = %d; something wrote around it", got, len(all))
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
