package sessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ChristopherJMiller/luma/core/internal/internalauth"
	"github.com/ChristopherJMiller/luma/core/internal/link"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// Barrier returns only once the work already on every live actor is done: a
// send that passed the Gate has delivered its turn (S52).
func TestBarrierWaitsForWorkInFlight(t *testing.T) {
	h := newHarness(t)
	idle := h.newSession(family)
	ses := h.newSession(family)
	h.send(family, idle.ID, "one") // another live actor, with a host
	entered, release := make(chan struct{}), make(chan struct{})
	h.cond.mu.Lock()
	h.cond.onSend = func(ts protocol.TurnSend) {
		if ts.SessionID == ses.ID {
			close(entered)
			<-release
		}
	}
	h.cond.mu.Unlock()
	sent := make(chan error, 1)
	go func() {
		_, err := h.svc.Send(bg, family, ses.ID, userMsg("held", newClientID()))
		sent <- err
	}()
	<-entered
	barrier := make(chan error, 1)
	go func() { barrier <- h.svc.Barrier(bg) }()
	select {
	case err := <-barrier:
		t.Fatalf("Barrier returned (%v) while a send was still delivering", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-barrier; err != nil {
		t.Fatalf("Barrier = %v", err)
	}
	if n := len(h.cond.Sends()); n != 2 {
		t.Fatalf("turn.sends when Barrier returned = %d, want the held one delivered", n)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}

	// Its context bounds it.
	h.cond.mu.Lock()
	block := make(chan struct{})
	h.cond.onSend = func(protocol.TurnSend) { <-block }
	h.cond.mu.Unlock()
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, result("sdk-1", "ok", 0.01))
	go func() { _, _ = h.svc.Send(bg, family, ses.ID, userMsg("stuck", newClientID())) }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if err := h.svc.Barrier(ctx); err == nil {
		t.Fatal("Barrier past its deadline returned nil")
	}
	close(block)
}

// ---- the S51 trace over a real link ----

type lateService struct{ *Service }

type tokenReviewer struct{}

func (tokenReviewer) Review(_ context.Context, token string) (internalauth.Identity, error) {
	if token != "aaaa.bbbb.cccc" {
		return internalauth.Identity{}, internalauth.ErrUnauthenticated
	}
	return internalauth.Identity{Username: "system:serviceaccount:luma:luma-conductor", Audiences: []string{"luma-internal"}}, nil
}

// wsConductor is a test conductor on a real link.
type wsConductor struct {
	t      *testing.T
	ws     *websocket.Conn
	frames chan protocol.LinkFrame
}

func dialConductor(t *testing.T, srv *link.Server) *wsConductor {
	t.Helper()
	mux := http.NewServeMux()
	srv.Routes(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http")+link.Path,
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer aaaa.bbbb.cccc"}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	c := &wsConductor{t: t, ws: ws, frames: make(chan protocol.LinkFrame, 64)}
	go func() {
		for {
			_, b, err := ws.Read(bg)
			if err != nil {
				return
			}
			if f, err := protocol.DecodeLinkFrame(b); err == nil {
				c.frames <- f
			}
		}
	}()
	waitFor(t, "the link is up", srv.Connected)
	return c
}

func (c *wsConductor) send(f protocol.LinkFrame) {
	c.t.Helper()
	b, err := protocol.EncodeLinkFrame(f)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.ws.Write(bg, websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsConductor) next() protocol.LinkFrame {
	c.t.Helper()
	select {
	case f := <-c.frames:
		return f
	case <-time.After(3 * time.Second):
		c.t.Fatal("no frame from Core within 3s")
	}
	return nil
}

func (c *wsConductor) none() {
	c.t.Helper()
	select {
	case f := <-c.frames:
		c.t.Fatalf("unexpected frame %#v", f)
	case <-time.After(150 * time.Millisecond):
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// S51: Core gives up on a stuck turn with host.close{error} (S48); a quick
// retry reopens only once the old host's closed has arrived, that late
// closed fails nothing, and the retried turn runs on the new host.
func TestRetryAfterHostCloseErrorWaitsForTheOldHost(t *testing.T) {
	h := newHarness(t)
	late := &lateService{}
	srv := link.NewServer(link.Handlers{
		Auth: internalauth.Middleware(tokenReviewer{}, "luma-internal",
			map[string]internalauth.Principal{"system:serviceaccount:luma:luma-conductor": internalauth.PrincipalConductor},
			internalauth.PrincipalConductor),
		Sessions: late,
		Metrics:  h.m,
	})
	svc := h.service(srv)
	late.Service = svc
	c := dialConductor(t, srv)

	ses, err := svc.Create(bg, family, protocol.CreateSession{Agent: "luma"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(bg, family, ses.ID, userMsg("one", newClientID())); err != nil {
		t.Fatal(err)
	}
	c.next() // host.open
	ts1 := c.next().(protocol.TurnSend)
	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts1.TurnID, Event: protocol.LinkMessageStarted{Model: haiku}})
	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts1.TurnID, Event: protocol.LinkError{Code: protocol.CodeOverloaded, Retryable: true}})
	waitFor(t, "the link error is applied", func() bool { return len(h.events(ses.ID, "error")) == 1 })

	// No turn_result within 120 s: Core closes the host and fails the turn.
	h.clk.Advance(errorGrace)
	if f := c.next(); f != (protocol.HostClose{SessionID: ses.ID, Reason: "error"}) {
		t.Fatalf("frame = %#v, want host.close{error}", f)
	}
	waitFor(t, "the turn fails", func() bool { _, running := svc.RunningTurn(ses.ID); return !running })

	// A quick retry waits for the old host's closed before reopening.
	retried := make(chan error, 1)
	go func() {
		_, err := svc.Send(bg, family, ses.ID, userMsg("retry", newClientID()))
		retried <- err
	}()
	c.none()
	c.send(protocol.HostState{SessionID: ses.ID, State: "closed", Reason: "error"})
	if f, ok := c.next().(protocol.HostOpen); !ok || f.SessionID != ses.ID {
		t.Fatalf("frame = %#v, want the reopen", f)
	}
	ts2, ok := c.next().(protocol.TurnSend)
	if !ok || ts2.TurnID == ts1.TurnID {
		t.Fatalf("frame = %#v, want the retried turn", ts2)
	}
	if err := <-retried; err != nil {
		t.Fatalf("retry = %v", err)
	}
	if turn, running := svc.RunningTurn(ses.ID); !running || turn != ts2.TurnID {
		t.Fatalf("running turn = %q %v, want the retry running", turn, running)
	}
	if n := srv.OpenHosts(); n != 1 {
		t.Fatalf("OpenHosts = %d, want the new host tracked", n)
	}

	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts2.TurnID, Event: protocol.LinkMessageStarted{Model: haiku}})
	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts2.TurnID, Event: result("sdk-2", "fine", 0.01)})
	waitFor(t, "the retry completes", func() bool { _, running := svc.RunningTurn(ses.ID); return !running })
	asst := h.messages(ses.ID, "assistant")
	if len(asst) != 2 || asst[1].Status != "complete" || asst[1].TurnID != ts2.TurnID {
		t.Fatalf("assistant messages = %+v, want the retry complete", asst)
	}
	if errs := h.events(ses.ID, "error"); len(errs) != 1 {
		t.Fatalf("error events = %d, want only the first turn's", len(errs))
	}
}

// S63, over the real link: the cap gives up on a turn (S48), its result then
// arrives while the old host is still closing, and its cost is ledgered;
// the message stays as the backstop left it.
func TestLateTurnResultOverTheLinkIsLedgered(t *testing.T) {
	h := newHarness(t)
	late := &lateService{}
	srv := link.NewServer(link.Handlers{
		Auth: internalauth.Middleware(tokenReviewer{}, "luma-internal",
			map[string]internalauth.Principal{"system:serviceaccount:luma:luma-conductor": internalauth.PrincipalConductor},
			internalauth.PrincipalConductor),
		Sessions: late,
		Metrics:  h.m,
	})
	svc := h.service(srv)
	late.Service = svc
	c := dialConductor(t, srv)

	ses, err := svc.Create(bg, family, protocol.CreateSession{Agent: "luma"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(bg, family, ses.ID, userMsg("one", newClientID())); err != nil {
		t.Fatal(err)
	}
	c.next() // host.open
	ts := c.next().(protocol.TurnSend)
	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts.TurnID, Event: protocol.LinkMessageStarted{Model: haiku}})
	waitFor(t, "the message starts", func() bool { return len(h.messages(ses.ID, "assistant")) == 1 })

	h.clk.Advance(15 * time.Minute) // the cap
	if f := c.next(); f != (protocol.TurnInterrupt{SessionID: ses.ID}) {
		t.Fatalf("frame = %#v, want turn.interrupt", f)
	}
	h.clk.Advance(interruptGrace) // no result: Core gives up
	if f := c.next(); f != (protocol.HostClose{SessionID: ses.ID, Reason: "error"}) {
		t.Fatalf("frame = %#v, want host.close{error}", f)
	}
	waitFor(t, "the turn fails", func() bool { _, running := svc.RunningTurn(ses.ID); return !running })
	before := h.messages(ses.ID, "assistant")

	// The CLI finished after all: its result comes on the closing host.
	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts.TurnID, Event: result("sdk-late", "late", 0.04)})
	waitFor(t, "the late result is ledgered", func() bool { return h.led.count() == 1 })
	h.led.mu.Lock()
	got := h.led.turns[0]
	h.led.mu.Unlock()
	if got.turnID != ts.TurnID || got.usage.CostUSD != 0.04 {
		t.Fatalf("ledgered %+v, want the late result's $0.04 for %s", got, ts.TurnID)
	}
	if after := h.messages(ses.ID, "assistant"); !reflect.DeepEqual(after, before) || after[0].Status != "error" {
		t.Fatalf("assistant message = %+v, want it as the backstop left it (%+v)", after, before)
	}
	if v := h.counter("luma_late_turn_results_total"); v != 1 {
		t.Fatalf("luma_late_turn_results_total = %v, want 1", v)
	}
}
