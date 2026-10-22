package link

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/internalauth"
	"github.com/ChristopherJMiller/luma/core/internal/maintenance"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

// The contracts this package meets and consumes.
var (
	_ sessions.Conductor = (*Server)(nil)
	_ Sessions           = (*sessions.Service)(nil)
	_ SessionRows        = (*store.Store)(nil)
)

var bg = context.Background()

// Synthetic, JWT-shaped bearers and the ServiceAccounts they review as.
const (
	conductorToken = "aaaa.bbbb.cccc"
	evalsToken     = "dddd.eeee.ffff"
	audience       = "luma-internal"
)

var principals = map[string]internalauth.Principal{
	"system:serviceaccount:luma:luma-conductor": internalauth.PrincipalConductor,
	"system:serviceaccount:luma:luma-evals":     internalauth.PrincipalEvals,
}

type fakeReviewer map[string]string // token -> username

func (r fakeReviewer) Review(_ context.Context, token string) (internalauth.Identity, error) {
	user, ok := r[token]
	if !ok {
		return internalauth.Identity{}, internalauth.ErrUnauthenticated
	}
	return internalauth.Identity{Username: user, Audiences: []string{audience}, PodName: "pod-x"}, nil
}

var reviewer = fakeReviewer{
	conductorToken: "system:serviceaccount:luma:luma-conductor",
	evalsToken:     "system:serviceaccount:luma:luma-evals",
}

// ---- the session service, faked ----

type linkEvent struct {
	session, turn, typ string
}

type fakeSessions struct {
	mu       sync.Mutex
	running  map[string]string
	events   []linkEvent
	states   []protocol.HostState
	lost     [][]string
	log      []string // events and losses, in the order they arrived
	barriers int

	// While hold is set, OnLinkEvent blocks on it (a slow actor whose
	// mailbox is full); held is signalled once per blocked call.
	hold chan struct{}
	held chan struct{}
	// slow is how long every OnLinkEvent takes.
	slow time.Duration
}

func newFakeSessions() *fakeSessions { return &fakeSessions{running: map[string]string{}} }

func (f *fakeSessions) OnLinkEvent(sessionID, turnID string, ev protocol.LinkEvent) {
	f.mu.Lock()
	hold, held, slow := f.hold, f.held, f.slow
	f.mu.Unlock()
	if hold != nil {
		held <- struct{}{}
		<-hold
	}
	time.Sleep(slow)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, linkEvent{sessionID, turnID, ev.LinkEventType()})
	f.log = append(f.log, "event")
}

func (f *fakeSessions) OnHostState(hs protocol.HostState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, hs)
}

func (f *fakeSessions) OnConductorLost(ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lost = append(f.lost, slices.Sorted(slices.Values(ids)))
	f.log = append(f.log, "lost")
}

func (f *fakeSessions) Barrier(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.barriers++
	return nil
}

// holdEvents makes OnLinkEvent block until the returned release is called.
func (f *fakeSessions) holdEvents() (held <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold, f.held = make(chan struct{}), make(chan struct{}, 64)
	hold := f.hold
	return f.held, func() {
		f.mu.Lock()
		f.hold = nil
		f.mu.Unlock()
		close(hold)
	}
}

func (f *fakeSessions) Log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

func (f *fakeSessions) RunningTurn(sessionID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.running[sessionID]
	return t, ok
}

func (f *fakeSessions) setRunning(sessionID, turnID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if turnID == "" {
		delete(f.running, sessionID)
		return
	}
	f.running[sessionID] = turnID
}

func (f *fakeSessions) Events() []linkEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

func (f *fakeSessions) States() []protocol.HostState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.states)
}

func (f *fakeSessions) Lost() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lost)
}

// ---- a fake clock for the purge timeout ----

type fakeTimer struct {
	at time.Duration
	c  chan time.Time
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*fakeTimer
}

func (c *fakeClock) newTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now + d, c: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t.c, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		i := slices.Index(c.timers, t)
		if i < 0 {
			return false
		}
		c.timers = slices.Delete(c.timers, i, i+1)
		return true
	}
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += d
	kept := c.timers[:0]
	for _, t := range c.timers {
		if t.at <= c.now {
			t.c <- time.Time{}
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

// ---- the harness ----

type harness struct {
	t    *testing.T
	srv  *Server
	sess *fakeSessions
	reg  *prometheus.Registry
	http *httptest.Server
}

// An option sets a Handlers field before NewServer or a Server field after.
type option struct {
	handlers func(*Handlers)
	server   func(*Server)
}

func withRPC(r RPCHandler) option { return option{handlers: func(h *Handlers) { h.RPC = r }} }

func withLiveness(ping, pong time.Duration) option {
	return option{server: func(s *Server) { s.pingEvery, s.pongWait = ping, pong }}
}

func withClock(c *fakeClock) option {
	return option{server: func(s *Server) { s.newTimer = c.newTimer }}
}

func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	reg := prometheus.NewRegistry()
	sess := newFakeSessions()
	h := Handlers{
		Auth:     internalauth.Middleware(reviewer, audience, principals, internalauth.PrincipalConductor),
		Sessions: sess,
		Metrics:  metrics.NewCore(reg),
	}
	for _, o := range opts {
		if o.handlers != nil {
			o.handlers(&h)
		}
	}
	srv := NewServer(h)
	for _, o := range opts {
		if o.server != nil {
			o.server(srv)
		}
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return &harness{t: t, srv: srv, sess: sess, reg: reg, http: hs}
}

// dial opens the link with token and returns the handshake's HTTP status.
func (h *harness) dial(token string) (*websocket.Conn, int, error) {
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	url := "ws" + strings.TrimPrefix(h.http.URL, "http") + "/internal/v1/conductor"
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return ws, status, err
}

// conductor is a test conductor on the link. Unless it is mute, a goroutine
// reads every frame Core sends (which also answers Core's pings).
type conductor struct {
	t      *testing.T
	ws     *websocket.Conn
	frames chan protocol.LinkFrame
	done   chan error // the read error, once the link ends
}

// connect dials as the conductor and waits until Core has taken the link.
func (h *harness) connect(mute bool) *conductor {
	h.t.Helper()
	before := h.srv.accepted.Load()
	ws, _, err := h.dial(conductorToken)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(64 << 20)
	c := &conductor{t: h.t, ws: ws, frames: make(chan protocol.LinkFrame, 64), done: make(chan error, 1)}
	h.t.Cleanup(func() { _ = ws.CloseNow() })
	if !mute {
		go func() {
			for {
				_, b, err := ws.Read(bg)
				if err != nil {
					c.done <- err
					return
				}
				f, err := protocol.DecodeLinkFrame(b)
				if err != nil {
					c.done <- err
					return
				}
				c.frames <- f
			}
		}()
	}
	eventually(h.t, "Core takes the link", func() bool { return h.srv.accepted.Load() > before })
	return c
}

func (c *conductor) send(f protocol.LinkFrame) {
	c.t.Helper()
	b, err := protocol.EncodeLinkFrame(f)
	if err != nil {
		c.t.Fatal(err)
	}
	c.sendRaw(string(b))
}

func (c *conductor) sendRaw(s string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		c.t.Fatalf("conductor write: %v", err)
	}
}

func (c *conductor) next() protocol.LinkFrame {
	c.t.Helper()
	select {
	case f := <-c.frames:
		return f
	case err := <-c.done:
		c.t.Fatalf("link ended while waiting for a frame: %v", err)
	case <-time.After(2 * time.Second):
		c.t.Fatal("no frame from Core within 2s")
	}
	return nil
}

// none asserts that Core sends nothing for a moment.
func (c *conductor) none() {
	c.t.Helper()
	select {
	case f := <-c.frames:
		c.t.Fatalf("unexpected frame %#v", f)
	case <-time.After(100 * time.Millisecond):
	}
}

func (c *conductor) expectClose(code websocket.StatusCode) {
	c.t.Helper()
	for {
		select {
		case <-c.frames:
		case err := <-c.done:
			if got := websocket.CloseStatus(err); got != code {
				c.t.Fatalf("link closed with %d (%v), want %d", got, err, code)
			}
			return
		case <-time.After(2 * time.Second):
			c.t.Fatalf("link not closed within 2s, want %d", code)
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			if !reflect.DeepEqual(got, labels) && (len(labels) != 0 || len(got) != 0) {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func rejected(t *testing.T, h *harness, reason string) float64 {
	t.Helper()
	return metricValue(t, h.reg, "luma_link_rejected_total", map[string]string{"reason": reason})
}

func spec(sessionID string) protocol.HostSpec {
	return protocol.HostSpec{SessionID: sessionID, RootID: sessionID, Person: protocol.HostPerson{Ref: "u_0a1b2c3d4e"}}
}

// open opens a host for sessionID on the current link and reads the frame.
func (h *harness) open(c *conductor, sessionID string) {
	h.t.Helper()
	if err := h.srv.Open(bg, spec(sessionID)); err != nil {
		h.t.Fatalf("Open %s: %v", sessionID, err)
	}
	if f, ok := c.next().(protocol.HostOpen); !ok || f.SessionID != sessionID {
		h.t.Fatalf("frame after Open = %#v, want host.open for %s", f, sessionID)
	}
}

func textDelta(sessionID, turnID, text string) protocol.EventFrame {
	return protocol.EventFrame{SessionID: sessionID, TurnID: turnID, Event: protocol.LinkTextDelta{Text: text}}
}

// ---- auth ----

func TestLinkRequiresConductorPrincipal(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"no bearer", "", http.StatusUnauthorized},
		{"unknown bearer", "gggg.hhhh.iiii", http.StatusUnauthorized},
		{"an evals token", evalsToken, http.StatusForbidden},
	} {
		ws, status, err := h.dial(tc.token)
		if err == nil {
			_ = ws.CloseNow()
			t.Fatalf("%s: the link upgraded", tc.name)
		}
		if status != tc.want {
			t.Fatalf("%s: status %d (%v), want %d", tc.name, status, err, tc.want)
		}
		if h.srv.Connected() {
			t.Fatalf("%s: Connected() after a refused handshake", tc.name)
		}
	}
	if got := metricValue(t, h.reg, "luma_conductor_connected", nil); got != 0 {
		t.Fatalf("luma_conductor_connected = %v before any link", got)
	}

	h.connect(false)
	if !h.srv.Connected() {
		t.Fatal("Connected() = false after the conductor's handshake")
	}
	if got := metricValue(t, h.reg, "luma_conductor_connected", nil); got != 1 {
		t.Fatalf("luma_conductor_connected = %v with a link up, want 1", got)
	}
}

// A mis-wired route (a middleware that lets any principal through) still
// refuses anyone but the conductor.
func TestLinkChecksThePrincipalItself(t *testing.T) {
	reg := prometheus.NewRegistry()
	srv := NewServer(Handlers{
		Auth:     internalauth.Middleware(reviewer, audience, principals, internalauth.PrincipalConductor, internalauth.PrincipalEvals),
		Sessions: newFakeSessions(),
		Metrics:  metrics.NewCore(reg),
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	h := &harness{t: t, srv: srv, reg: reg, http: hs}
	ws, status, err := h.dial(evalsToken)
	if err == nil {
		_ = ws.CloseNow()
		t.Fatal("an evals token upgraded the link")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status %d, want 403", status)
	}

	// No middleware at all: nobody is authenticated, so nobody gets in.
	srv2 := NewServer(Handlers{Sessions: newFakeSessions(), Metrics: metrics.NewCore(prometheus.NewRegistry())})
	mux2 := http.NewServeMux()
	srv2.Routes(mux2)
	hs2 := httptest.NewServer(mux2)
	defer hs2.Close()
	h2 := &harness{t: t, srv: srv2, http: hs2}
	if ws, status, err := h2.dial(conductorToken); err == nil || status != http.StatusForbidden {
		if ws != nil {
			_ = ws.CloseNow()
		}
		t.Fatalf("without Auth: status %d (%v), want 403", status, err)
	}
}

// ---- newest link wins, liveness ----

func TestNewestConductorWins(t *testing.T) {
	h := newHarness(t)
	a := h.connect(false)
	h.open(a, "ses_A")
	h.open(a, "ses_B")

	b := h.connect(false)
	a.expectClose(4000)
	eventually(t, "OnConductorLost for the old link's sessions", func() bool { return len(h.sess.Lost()) == 1 })
	if got := h.sess.Lost(); !reflect.DeepEqual(got, [][]string{{"ses_A", "ses_B"}}) {
		t.Fatalf("OnConductorLost = %v, want the old link's two sessions", got)
	}
	if !h.srv.Connected() {
		t.Fatal("Connected() = false with the new link up")
	}
	if got := h.srv.OpenHosts(); got != 0 {
		t.Fatalf("OpenHosts = %d on the new link, want 0", got)
	}

	// Frames go to the new link only.
	h.open(b, "ses_C")
	// The old sessions are not open on the new link: their events are refused.
	h.sess.setRunning("ses_A", "turn_1")
	b.send(textDelta("ses_A", "turn_1", "late"))
	eventually(t, "the event is refused as unopened", func() bool { return rejected(t, h, "unopened") == 1 })
	if got := h.sess.Events(); len(got) != 0 {
		t.Fatalf("events forwarded = %v, want none", got)
	}

	// A third link supersedes the second the same way.
	h.connect(false)
	b.expectClose(4000)
	eventually(t, "OnConductorLost for the second link", func() bool { return len(h.sess.Lost()) == 2 })
	if got := h.sess.Lost()[1]; !reflect.DeepEqual(got, []string{"ses_C"}) {
		t.Fatalf("second OnConductorLost = %v, want [ses_C]", got)
	}
	if got := metricValue(t, h.reg, "luma_conductor_connected", nil); got != 1 {
		t.Fatalf("luma_conductor_connected = %v, want 1", got)
	}
}

func TestLinkPingTimeoutCallsOnConductorLost(t *testing.T) {
	if pingInterval != 10*time.Second || pongTimeout != 30*time.Second {
		t.Fatalf("liveness = ping %v, pong %v; want 10s and 30s", pingInterval, pongTimeout)
	}

	// A conductor that answers pings stays up across many pong timeouts.
	h := newHarness(t, withLiveness(20*time.Millisecond, 150*time.Millisecond))
	alive := h.connect(false)
	h.open(alive, "ses_A")
	time.Sleep(750 * time.Millisecond)
	if !h.srv.Connected() || len(h.sess.Lost()) != 0 {
		t.Fatalf("a responsive link was dropped: connected %v, lost %v", h.srv.Connected(), h.sess.Lost())
	}

	// A conductor that never reads never answers a ping: lost after the timeout.
	h2 := newHarness(t, withLiveness(20*time.Millisecond, 150*time.Millisecond))
	mute := h2.connect(true)
	if err := h2.srv.Open(bg, spec("ses_M")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	eventually(t, "the mute link is lost", func() bool { return len(h2.sess.Lost()) == 1 })
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Fatalf("lost after %v, before the pong timeout", took)
	}
	if got := h2.sess.Lost(); !reflect.DeepEqual(got, [][]string{{"ses_M"}}) {
		t.Fatalf("OnConductorLost = %v, want [[ses_M]]", got)
	}
	if h2.srv.Connected() {
		t.Fatal("Connected() = true after the ping timeout")
	}
	if got := metricValue(t, h2.reg, "luma_conductor_connected", nil); got != 0 {
		t.Fatalf("luma_conductor_connected = %v after the loss, want 0", got)
	}
	if err := h2.srv.Open(bg, spec("ses_N")); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Open with no link = %v, want ErrNotConnected", err)
	}
	_ = mute
}

// A conductor that drops the connection is lost at once.
func TestLinkCloseCallsOnConductorLost(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	h.open(c, "ses_A")
	_ = c.ws.Close(websocket.StatusNormalClosure, "bye")
	eventually(t, "OnConductorLost", func() bool { return len(h.sess.Lost()) == 1 })
	if got := h.sess.Lost(); !reflect.DeepEqual(got, [][]string{{"ses_A"}}) {
		t.Fatalf("OnConductorLost = %v", got)
	}
	if h.srv.Connected() {
		t.Fatal("Connected() after the conductor left")
	}
}

// ---- Core -> conductor frames ----

func TestFramesToTheConductor(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	h.open(c, "ses_A")
	if got := h.srv.OpenHosts(); got != 1 {
		t.Fatalf("OpenHosts = %d, want 1", got)
	}
	if got := metricValue(t, h.reg, "luma_open_hosts", nil); got != 1 {
		t.Fatalf("luma_open_hosts = %v, want 1", got)
	}

	ts := protocol.TurnSend{SessionID: "ses_A", TurnID: "turn_1", Content: []protocol.ContentBlock{{Type: "text", Text: "hi"}}, Untrusted: true}
	if err := h.srv.SendTurn(bg, ts); err != nil {
		t.Fatal(err)
	}
	if f := c.next(); !reflect.DeepEqual(f, ts) {
		t.Fatalf("turn.send = %#v", f)
	}
	if err := h.srv.Interrupt(bg, "ses_A"); err != nil {
		t.Fatal(err)
	}
	if f := c.next(); f != (protocol.TurnInterrupt{SessionID: "ses_A"}) {
		t.Fatalf("turn.interrupt = %#v", f)
	}
	// host.close carries every reason, "error" included (S48).
	for _, reason := range []string{"error", "deleted", "shutdown", "maintenance"} {
		if err := h.srv.Close(bg, "ses_A", reason); err != nil {
			t.Fatal(err)
		}
		if f := c.next(); f != (protocol.HostClose{SessionID: "ses_A", Reason: reason}) {
			t.Fatalf("host.close = %#v, want reason %s", f, reason)
		}
	}
	if err := h.srv.Shutdown(bg, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if f := c.next(); f != (protocol.Shutdown{Reason: "maintenance"}) {
		t.Fatalf("shutdown = %#v", f)
	}
}

func TestNoLinkRefusesFrames(t *testing.T) {
	h := newHarness(t)
	if h.srv.Connected() {
		t.Fatal("Connected() with no link")
	}
	if err := h.srv.Open(bg, spec("ses_A")); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Open = %v", err)
	}
	if err := h.srv.SendTurn(bg, protocol.TurnSend{SessionID: "ses_A", TurnID: "t"}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("SendTurn = %v", err)
	}
	if err := h.srv.Interrupt(bg, "ses_A"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Interrupt = %v", err)
	}
	if err := h.srv.Close(bg, "ses_A", "error"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Close = %v", err)
	}
	// Nothing runs on a conductor that is not there: shutdown has nothing to do.
	if err := h.srv.Shutdown(bg, "maintenance"); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
}

// ---- conductor -> Core: session binding ----

func TestEventForUnopenedSessionRejected(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	h.sess.setRunning("ses_Z", "turn_1")
	c.send(textDelta("ses_Z", "turn_1", "never opened"))
	c.send(protocol.RPC{RPCID: "rpc_1", SessionID: "ses_Z", TurnID: "turn_1", Method: "memory.remember", Params: protocol.RPCParams{Text: "x"}})
	// An rpc that is refused is answered, so the conductor's tool call ends.
	if f, ok := c.next().(protocol.RPCResult); !ok || f.RPCID != "rpc_1" || f.OK || f.Error == nil || f.Error.Code != protocol.CodeUpstreamError {
		t.Fatalf("answer to a refused rpc = %#v", f)
	}
	eventually(t, "two unopened rejections", func() bool { return rejected(t, h, "unopened") == 2 })
	if got := h.sess.Events(); len(got) != 0 {
		t.Fatalf("events forwarded = %v, want none", got)
	}
	// host.state for a session Core never opened here is refused too.
	c.send(protocol.HostState{SessionID: "ses_Z", State: "ready"})
	eventually(t, "a third unopened rejection", func() bool { return rejected(t, h, "unopened") == 3 })
	if got := h.sess.States(); len(got) != 0 {
		t.Fatalf("host.state forwarded = %v", got)
	}

	// Once its host has closed, a session is no longer open on the link.
	h.open(c, "ses_A")
	h.sess.setRunning("ses_A", "turn_2")
	c.send(textDelta("ses_A", "turn_2", "ok"))
	c.send(protocol.HostState{SessionID: "ses_A", State: "closed", Reason: "idle"})
	c.send(textDelta("ses_A", "turn_2", "after close"))
	eventually(t, "a fourth unopened rejection", func() bool { return rejected(t, h, "unopened") == 4 })
	if got := h.sess.Events(); !reflect.DeepEqual(got, []linkEvent{{"ses_A", "turn_2", "text_delta"}}) {
		t.Fatalf("events forwarded = %v", got)
	}
	if got := h.srv.OpenHosts(); got != 0 {
		t.Fatalf("OpenHosts = %d after the close, want 0", got)
	}
}

func TestEventForStaleTurnRejected(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	h.open(c, "ses_A")

	c.send(textDelta("ses_A", "turn_1", "no turn running"))
	eventually(t, "a stale rejection", func() bool { return rejected(t, h, "stale_turn") == 1 })

	h.sess.setRunning("ses_A", "turn_2")
	c.send(textDelta("ses_A", "turn_1", "an old turn"))
	c.send(protocol.RPC{RPCID: "rpc_1", SessionID: "ses_A", TurnID: "turn_1", Method: "memory.remember", Params: protocol.RPCParams{Text: "x"}})
	if f, ok := c.next().(protocol.RPCResult); !ok || f.RPCID != "rpc_1" || f.OK {
		t.Fatalf("answer to a stale rpc = %#v", f)
	}
	c.send(protocol.EventFrame{SessionID: "ses_A", TurnID: "turn_2", Event: protocol.LinkMessageStarted{Model: "m"}})
	c.send(textDelta("ses_A", "turn_2", "the running turn"))
	c.send(protocol.EventFrame{SessionID: "ses_A", TurnID: "turn_2", Event: protocol.TurnResult{SDKUUID: "u"}})
	eventually(t, "three forwarded events", func() bool { return len(h.sess.Events()) == 3 })
	want := []linkEvent{{"ses_A", "turn_2", "message_started"}, {"ses_A", "turn_2", "text_delta"}, {"ses_A", "turn_2", "turn_result"}}
	if got := h.sess.Events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events forwarded = %v, want %v (in frame order)", got, want)
	}
	if got := rejected(t, h, "stale_turn"); got != 3 {
		t.Fatalf("stale_turn rejections = %v, want 3", got)
	}
	if got := rejected(t, h, "unopened"); got != 0 {
		t.Fatalf("unopened rejections = %v, want 0", got)
	}
}

func TestMalformedFramesRejected(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	c.sendRaw(`{"type":"no.such.frame"}`)
	c.sendRaw(`not json`)
	// A Core -> conductor frame has no business coming the other way.
	c.send(protocol.HostOpen{SessionID: "ses_A", Spec: spec("ses_A")})
	// Frames are text; a binary one is malformed even if its bytes are JSON.
	ctx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, []byte(`{"type":"hello","conductor_version":"v","sdk_version":"s","cli_version":"c","max_hosts":4}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "four malformed rejections", func() bool { return rejected(t, h, "malformed") == 4 })
	// The link stays up and keeps working.
	c.send(protocol.Hello{ConductorVersion: "v", SDKVersion: "s", CLIVersion: "c", MaxHosts: 4})
	h.open(c, "ses_B")
	if got := rejected(t, h, "malformed"); got != 4 {
		t.Fatalf("hello was rejected: malformed = %v", got)
	}
	if !h.srv.Connected() {
		t.Fatal("a malformed frame ended the link")
	}
}

// A frame over the read limit ends the link (1009), and its sessions are lost.
func TestOversizedFrameEndsTheLink(t *testing.T) {
	if readLimit != 8<<20 {
		t.Fatalf("readLimit = %d, want 8 MiB", readLimit)
	}
	h := newHarness(t)
	c := h.connect(false)
	h.open(c, "ses_A")
	h.sess.setRunning("ses_A", "turn_1")
	// Core may close (1009) before it has read the whole frame, so the 8 MiB
	// write may stall or fail, slowest under a loaded -race run; the close
	// code is what this test is about.
	b, err := protocol.EncodeLinkFrame(textDelta("ses_A", "turn_1", strings.Repeat("x", readLimit)))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(bg, 30*time.Second)
	_ = c.ws.Write(wctx, websocket.MessageText, b)
	cancel()
	c.expectClose(websocket.StatusMessageTooBig)
	eventually(t, "the link's sessions are lost", func() bool { return len(h.sess.Lost()) == 1 })
	if h.srv.Connected() || len(h.sess.Events()) != 0 {
		t.Fatalf("connected %v, events %v after an oversized frame", h.srv.Connected(), h.sess.Events())
	}
}

// S51: after Core's host.close, the session's next host.state is the old
// host's. A reopen waits for its closed (consumed: it fails no turn and does
// not remove the new host), at most 10 s, or until the link drops.
func TestReopenWaitsForTheOldHostsClose(t *testing.T) {
	if closingWait != 10*time.Second {
		t.Fatalf("closingWait = %v, want 10s", closingWait)
	}

	t.Run("the old host's closed lets the reopen go", func(t *testing.T) {
		h := newHarness(t)
		c := h.connect(false)
		h.open(c, "ses_A")
		if err := h.srv.Close(bg, "ses_A", "error"); err != nil {
			t.Fatal(err)
		}
		if f := c.next(); f != (protocol.HostClose{SessionID: "ses_A", Reason: "error"}) {
			t.Fatalf("frame = %#v", f)
		}
		done := make(chan error, 1)
		go func() { done <- h.srv.Open(bg, spec("ses_A")) }()
		c.none() // no host.open while the old host is closing
		// The old host reports in before it closes: dropped, not forwarded.
		c.send(protocol.HostState{SessionID: "ses_A", State: "ready", SDKSessionID: "0d9c8b7a-6f5e-4d3c-9b2a-1f0e9d8c7b6a"})
		c.none()
		c.send(protocol.HostState{SessionID: "ses_A", State: "closed", Reason: "error"})
		if f, ok := c.next().(protocol.HostOpen); !ok || f.SessionID != "ses_A" {
			t.Fatalf("frame after the old close = %#v, want host.open", f)
		}
		if err := <-done; err != nil {
			t.Fatalf("Open = %v", err)
		}
		if got := h.sess.States(); len(got) != 0 {
			t.Fatalf("the old host's states reached the sessions: %v", got)
		}
		if got := h.srv.OpenHosts(); got != 1 {
			t.Fatalf("OpenHosts = %d, want the new host tracked", got)
		}
		// The new host works, and its own close is forwarded.
		h.sess.setRunning("ses_A", "turn_2")
		c.send(textDelta("ses_A", "turn_2", "new host"))
		c.send(protocol.HostState{SessionID: "ses_A", State: "closed", Reason: "idle"})
		eventually(t, "the new host's close is forwarded", func() bool { return len(h.sess.States()) == 1 })
		if got := h.sess.Events(); !reflect.DeepEqual(got, []linkEvent{{"ses_A", "turn_2", "text_delta"}}) {
			t.Fatalf("events = %v", got)
		}
	})

	t.Run("after 10 s the old host counts as closed", func(t *testing.T) {
		clock := &fakeClock{}
		h := newHarness(t, withClock(clock))
		c := h.connect(false)
		h.open(c, "ses_A")
		if err := h.srv.Close(bg, "ses_A", "error"); err != nil {
			t.Fatal(err)
		}
		c.next()
		done := make(chan error, 1)
		go func() { done <- h.srv.Open(bg, spec("ses_A")) }()
		eventually(t, "the reopen waits", func() bool { return clock.pending() == 1 })
		clock.advance(10*time.Second - time.Millisecond)
		c.none()
		clock.advance(time.Millisecond)
		if f, ok := c.next().(protocol.HostOpen); !ok || f.SessionID != "ses_A" {
			t.Fatalf("frame after 10 s = %#v, want host.open", f)
		}
		if err := <-done; err != nil {
			t.Fatalf("Open = %v", err)
		}
		if got := h.srv.OpenHosts(); got != 1 {
			t.Fatalf("OpenHosts = %d", got)
		}
	})

	t.Run("a lost link ends the wait", func(t *testing.T) {
		h := newHarness(t)
		c := h.connect(false)
		h.open(c, "ses_A")
		if err := h.srv.Close(bg, "ses_A", "error"); err != nil {
			t.Fatal(err)
		}
		c.next()
		done := make(chan error, 1)
		go func() { done <- h.srv.Open(bg, spec("ses_A")) }()
		c.none()
		_ = c.ws.Close(websocket.StatusGoingAway, "gone")
		select {
		case err := <-done:
			if !errors.Is(err, ErrNotConnected) {
				t.Fatalf("Open = %v, want ErrNotConnected", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Open kept waiting after the link dropped")
		}
	})
}

// The reader keeps reading while a slow actor holds the forwarder, so pongs
// flow and the link stays up; the held frames are then forwarded in order.
func TestSlowActorKeepsTheLinkAlive(t *testing.T) {
	h := newHarness(t, withLiveness(20*time.Millisecond, 150*time.Millisecond))
	c := h.connect(false)
	h.open(c, "ses_A")
	h.sess.setRunning("ses_A", "turn_1")
	held, release := h.sess.holdEvents()
	for i := range 5 {
		c.send(textDelta("ses_A", "turn_1", fmt.Sprint(i)))
	}
	<-held
	time.Sleep(600 * time.Millisecond) // four pong timeouts
	if !h.srv.Connected() || len(h.sess.Lost()) != 0 {
		t.Fatalf("a slow actor dropped the link: connected %v, lost %v", h.srv.Connected(), h.sess.Lost())
	}
	release()
	eventually(t, "the held frames are forwarded, or the link lost", func() bool {
		return len(h.sess.Events()) == 5 || len(h.sess.Lost()) > 0
	})
	if lost := h.sess.Lost(); len(lost) != 0 {
		t.Fatalf("the link was lost while the actor was slow (%v); events %v", lost, h.sess.Events())
	}
	if len(h.sess.Events()) != 5 {
		t.Fatalf("events = %v, want all five in order", h.sess.Events())
	}
}

// A superseded link's frames reach the sessions before its OnConductorLost,
// and none after it.
func TestSupersededLinkForwardsNothingAfterTheLoss(t *testing.T) {
	h := newHarness(t)
	a := h.connect(false)
	h.open(a, "ses_A")
	h.sess.setRunning("ses_A", "turn_1")
	// Each event takes 5 ms, so the link's end, once waiting, gets in between
	// two of them and cuts the queue.
	h.sess.mu.Lock()
	h.sess.slow = 5 * time.Millisecond
	h.sess.mu.Unlock()
	held, release := h.sess.holdEvents()
	for i := range 20 {
		a.send(textDelta("ses_A", "turn_1", fmt.Sprint(i)))
	}
	<-held // the forwarder is inside OnLinkEvent with frames queued behind it
	connected := make(chan struct{})
	go func() {
		h.connect(false)
		close(connected)
	}()
	time.Sleep(50 * time.Millisecond) // the new link is waiting for the old one to end
	release()
	<-connected
	eventually(t, "the old link is lost", func() bool { return len(h.sess.Lost()) == 1 })
	time.Sleep(100 * time.Millisecond)
	log := h.sess.Log()
	if log[len(log)-1] != "lost" || slices.Index(log, "lost") != len(log)-1 {
		t.Fatalf("log = %v: an old-link frame reached the sessions after its loss", log)
	}
	if n := len(log) - 1; n == 20 {
		t.Fatalf("all 20 queued frames were forwarded; the loss did not cut the queue")
	}
}

// Writes are bounded by their own timeout, not by the caller's context: a
// caller that gives up must not close the conductor's link.
func TestWritesIgnoreTheCallersCancellation(t *testing.T) {
	if writeTimeout != 30*time.Second {
		t.Fatalf("writeTimeout = %v, want 30s", writeTimeout)
	}
	h := newHarness(t)
	c := h.connect(false)
	h.open(c, "ses_A")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	ts := protocol.TurnSend{SessionID: "ses_A", TurnID: "turn_1", Content: []protocol.ContentBlock{{Type: "text", Text: "hi"}}}
	if err := h.srv.SendTurn(ctx, ts); err != nil {
		t.Fatalf("SendTurn with a cancelled context = %v", err)
	}
	if f := c.next(); !reflect.DeepEqual(f, ts) {
		t.Fatalf("frame = %#v", f)
	}
	if !h.srv.Connected() {
		t.Fatal("a cancelled caller closed the link")
	}
}

// A host.open that cannot be encoded leaves no host behind.
func TestOpenThatCannotBeSentLeavesNoHost(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	bad := spec("ses_A")
	bad.Resume = &protocol.Resume{SDKSessionID: "s", BaselineTotals: &protocol.Totals{CostUSD: math.NaN()}}
	if err := h.srv.Open(bg, bad); err == nil {
		t.Fatal("Open of an unencodable spec returned nil")
	}
	if got := h.srv.OpenHosts(); got != 0 {
		t.Fatalf("OpenHosts = %d after a failed Open", got)
	}
	if got := metricValue(t, h.reg, "luma_open_hosts", nil); got != 0 {
		t.Fatalf("luma_open_hosts = %v after a failed Open", got)
	}
	c.none()
	h.open(c, "ses_B")
}

// Barrier passes through to the session service (maintenance, S52).
func TestBarrierReachesTheSessions(t *testing.T) {
	h := newHarness(t)
	if err := h.srv.Barrier(bg); err != nil {
		t.Fatal(err)
	}
	h.sess.mu.Lock()
	defer h.sess.mu.Unlock()
	if h.sess.barriers != 1 {
		t.Fatalf("barriers = %d, want 1", h.sess.barriers)
	}
}

func TestShutdownReasonMaintenance(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	h.open(c, "ses_A")
	c.sendRaw(`{"type":"host.state","session_id":"ses_A","state":"closed","reason":"maintenance"}`)
	eventually(t, "the host.state is forwarded", func() bool { return len(h.sess.States()) == 1 })
	if got := h.sess.States()[0]; got != (protocol.HostState{SessionID: "ses_A", State: "closed", Reason: "maintenance"}) {
		t.Fatalf("forwarded %#v", got)
	}
	if got := h.srv.OpenHosts(); got != 0 {
		t.Fatalf("OpenHosts = %d after the maintenance close", got)
	}
	if got := rejected(t, h, "malformed") + rejected(t, h, "unopened"); got != 0 {
		t.Fatalf("the frame was rejected (%v)", got)
	}
}

func TestHostStateReadyForwarded(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	h.open(c, "ses_A")
	c.send(protocol.HostState{SessionID: "ses_A", State: "ready"})
	c.send(protocol.HostState{SessionID: "ses_A", State: "ready", SDKSessionID: "0d9c8b7a-6f5e-4d3c-9b2a-1f0e9d8c7b6a"})
	eventually(t, "two host.state forwarded", func() bool { return len(h.sess.States()) == 2 })
	if got := h.sess.States()[1].SDKSessionID; got != "0d9c8b7a-6f5e-4d3c-9b2a-1f0e9d8c7b6a" {
		t.Fatalf("sdk_session_id = %q", got)
	}
}

// RunningTurns lists the sessions with a running turn among those open on the
// link; maintenance drains them.
func TestRunningTurns(t *testing.T) {
	h := newHarness(t)
	if got := h.srv.RunningTurns(); len(got) != 0 {
		t.Fatalf("RunningTurns with no link = %v", got)
	}
	c := h.connect(false)
	h.open(c, "ses_A")
	h.open(c, "ses_B")
	h.sess.setRunning("ses_B", "turn_1")
	h.sess.setRunning("ses_X", "turn_9") // running, but not on this link
	if got := h.srv.RunningTurns(); !reflect.DeepEqual(got, []string{"ses_B"}) {
		t.Fatalf("RunningTurns = %v, want [ses_B]", got)
	}
}

// ---- rpc routing ----

type fakeRows map[string]store.Session

func (r fakeRows) Session(_ context.Context, id string) (store.Session, error) {
	s, ok := r[id]
	if !ok {
		return store.Session{}, store.ErrNotFound
	}
	return s, nil
}

type memCall struct {
	op, person, session, turn, text string
	flagged                         bool
}

type fakeMemory struct {
	mu    sync.Mutex
	calls []memCall
}

func (m *fakeMemory) record(op string, sess store.Session, turnID, text string, flagged bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, memCall{op, sess.PersonID, sess.ID, turnID, text, flagged})
}

func (m *fakeMemory) Remember(_ context.Context, sess store.Session, turnID, text string, flagged bool) (protocol.MemoryResult, error) {
	m.record("remember", sess, turnID, text, flagged)
	return protocol.MemoryResult{Status: "applied", WriteID: "mw_1"}, nil
}

func (m *fakeMemory) Forget(_ context.Context, sess store.Session, turnID, query string, flagged bool) (protocol.MemoryResult, error) {
	m.record("forget", sess, turnID, query, flagged)
	n := 2
	return protocol.MemoryResult{Status: "applied", WriteID: "mw_2", Removed: &n}, nil
}

func (m *fakeMemory) Calls() []memCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

func row(id string, incognito bool, memory string) store.Session {
	snap, _ := json.Marshal(protocol.AgentSpec{ID: "luma", Memory: memory})
	return store.Session{ID: id, RootID: id, PersonID: "pers-fam", Incognito: incognito, AgentSnapshot: snap}
}

func TestRPCRememberDerivesPersonFromSession(t *testing.T) {
	mem := &fakeMemory{}
	rows := fakeRows{"ses_A": row("ses_A", false, "person")}
	h := newHarness(t, withRPC(MemoryRPC(rows, mem)))
	c := h.connect(false)
	h.open(c, "ses_A")
	h.sess.setRunning("ses_A", "turn_1")

	// The params (and the frame) name another person; they are ignored.
	c.sendRaw(`{"type":"rpc","rpc_id":"rpc_1","session_id":"ses_A","turn_id":"turn_1","method":"memory.remember",` +
		`"person_id":"pers-oth","params":{"text":"likes green tea","untrusted":true,"person_id":"pers-oth","person":{"ref":"u_ffffffffff"}}}`)
	got, ok := c.next().(protocol.RPCResult)
	if !ok || !got.OK || got.RPCID != "rpc_1" || got.Result == nil || *got.Result != (protocol.MemoryResult{Status: "applied", WriteID: "mw_1"}) {
		t.Fatalf("rpc.result = %#v", got)
	}
	c.send(protocol.RPC{RPCID: "rpc_2", SessionID: "ses_A", TurnID: "turn_1", Method: "memory.forget", Params: protocol.RPCParams{Text: "green tea"}})
	got, ok = c.next().(protocol.RPCResult)
	if !ok || !got.OK || got.RPCID != "rpc_2" || got.Result == nil || got.Result.Removed == nil || *got.Result.Removed != 2 {
		t.Fatalf("rpc.result = %#v", got)
	}
	want := []memCall{
		{"remember", "pers-fam", "ses_A", "turn_1", "likes green tea", true},
		{"forget", "pers-fam", "ses_A", "turn_1", "green tea", false},
	}
	if got := mem.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("memory calls = %v, want %v", got, want)
	}
}

func TestRPCRefusedForIncognito(t *testing.T) {
	mem := &fakeMemory{}
	rows := fakeRows{
		"ses_I": row("ses_I", true, "person"),
		"ses_N": row("ses_N", false, "none"),
		// ses_U is open with a running turn, but its row is gone (deleted).
	}
	h := newHarness(t, withRPC(MemoryRPC(rows, mem)))
	c := h.connect(false)
	for i, id := range []string{"ses_I", "ses_N", "ses_U"} {
		h.open(c, id)
		h.sess.setRunning(id, "turn_1")
		for _, method := range []string{"memory.remember", "memory.forget"} {
			c.send(protocol.RPC{RPCID: id + method, SessionID: id, TurnID: "turn_1", Method: method, Params: protocol.RPCParams{Text: "something"}})
			got, ok := c.next().(protocol.RPCResult)
			want := protocol.MemoryResult{Status: "disabled", Code: protocol.CodeMemoryDisabled}
			if !ok || !got.OK || got.RPCID != id+method || got.Result == nil || *got.Result != want {
				t.Fatalf("%d %s %s: rpc.result = %#v, want disabled with memory_disabled", i, id, method, got)
			}
		}
	}
	if got := mem.Calls(); len(got) != 0 {
		t.Fatalf("memory was called: %v", got)
	}
}

// With no memory handler wired, an rpc is answered disabled, with its code.
func TestRPCWithoutMemoryIsDisabled(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	h.open(c, "ses_A")
	h.sess.setRunning("ses_A", "turn_1")
	c.send(protocol.RPC{RPCID: "rpc_1", SessionID: "ses_A", TurnID: "turn_1", Method: "memory.remember", Params: protocol.RPCParams{Text: "something"}})
	got, ok := c.next().(protocol.RPCResult)
	want := protocol.MemoryResult{Status: "disabled", Code: protocol.CodeMemoryDisabled}
	if !ok || !got.OK || got.Result == nil || *got.Result != want {
		t.Fatalf("rpc.result = %#v, want disabled with memory_disabled", got)
	}
}

func TestRPCFailuresAnswered(t *testing.T) {
	failing := func(context.Context, string, string, string, json.RawMessage) (protocol.MemoryResult, error) {
		return protocol.MemoryResult{}, errors.New("db down")
	}
	h := newHarness(t, withRPC(failing))
	c := h.connect(false)
	h.open(c, "ses_A")
	h.sess.setRunning("ses_A", "turn_1")
	c.send(protocol.RPC{RPCID: "rpc_1", SessionID: "ses_A", TurnID: "turn_1", Method: "memory.remember", Params: protocol.RPCParams{Text: "x"}})
	got, ok := c.next().(protocol.RPCResult)
	want := protocol.ErrorInfo{Code: protocol.CodeUpstreamError, Message: protocol.ErrorCopy[protocol.CodeUpstreamError]}
	if !ok || got.OK || got.Result != nil || got.Error == nil || *got.Error != want {
		t.Fatalf("rpc.result = %#v", got)
	}

	// An unknown method is an error, never a memory call.
	mem := &fakeMemory{}
	route := MemoryRPC(fakeRows{"ses_A": row("ses_A", false, "person")}, mem)
	if _, err := route(bg, "ses_A", "turn_1", "memory.wipe", json.RawMessage(`{"text":"x","untrusted":false}`)); err == nil {
		t.Fatal("memory.wipe routed without an error")
	}
	if len(mem.Calls()) != 0 {
		t.Fatal("memory.wipe reached the memory service")
	}
}

// ---- tree.purge (S14, S39) ----

func TestTreePurgeCarriesPersonRef(t *testing.T) {
	h := newHarness(t)
	c := h.connect(false)
	done := make(chan error, 1)
	go func() { done <- h.srv.PurgeTree(bg, "ses_ROOT", "u_0a1b2c3d4e") }()
	if f := c.next(); f != (protocol.TreePurge{RootID: "ses_ROOT", PersonRef: "u_0a1b2c3d4e"}) {
		t.Fatalf("frame = %#v, want tree.purge with the person ref", f)
	}
	c.send(protocol.TreePurged{RootID: "ses_ROOT"})
	if err := <-done; err != nil {
		t.Fatalf("PurgeTree = %v", err)
	}
}

func TestPurgeTreeWaitsForAck(t *testing.T) {
	if purgeTimeout != 30*time.Second {
		t.Fatalf("purgeTimeout = %v, want 30s", purgeTimeout)
	}

	t.Run("returns after its ack", func(t *testing.T) {
		h := newHarness(t)
		c := h.connect(false)
		done := make(chan error, 1)
		go func() { done <- h.srv.PurgeTree(bg, "ses_R", "u_0a1b2c3d4e") }()
		c.next()
		// An ack for another tree releases nothing.
		c.send(protocol.TreePurged{RootID: "ses_OTHER"})
		select {
		case err := <-done:
			t.Fatalf("PurgeTree returned before its ack: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		c.send(protocol.TreePurged{RootID: "ses_R"})
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("PurgeTree = %v", err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("PurgeTree had not returned 100 ms after its ack")
		}
		// A second ack, with no purge waiting, is ignored.
		c.send(protocol.TreePurged{RootID: "ses_R"})
		h.open(c, "ses_S")
	})

	t.Run("concurrent purges of one root share one ack", func(t *testing.T) {
		h := newHarness(t)
		c := h.connect(false)
		done := make(chan error, 2)
		go func() { done <- h.srv.PurgeTree(bg, "ses_R", "u_0a1b2c3d4e") }()
		c.next()
		go func() { done <- h.srv.PurgeTree(bg, "ses_R", "u_0a1b2c3d4e") }()
		c.none() // the second purge sends no second tree.purge
		c.send(protocol.TreePurged{RootID: "ses_R"})
		for range 2 {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("PurgeTree = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("one ack did not release both purges")
			}
		}
	})

	t.Run("returns when the link is lost", func(t *testing.T) {
		h := newHarness(t)
		c := h.connect(false)
		done := make(chan error, 1)
		go func() { done <- h.srv.PurgeTree(bg, "ses_R", "u_0a1b2c3d4e") }()
		c.next()
		_ = c.ws.Close(websocket.StatusGoingAway, "gone")
		select {
		case err := <-done:
			if !errors.Is(err, ErrLinkLost) {
				t.Fatalf("PurgeTree = %v, want ErrLinkLost", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("PurgeTree did not return on link loss")
		}
	})

	t.Run("returns when the link is superseded", func(t *testing.T) {
		h := newHarness(t)
		c := h.connect(false)
		done := make(chan error, 1)
		go func() { done <- h.srv.PurgeTree(bg, "ses_R", "u_0a1b2c3d4e") }()
		c.next()
		b := h.connect(false)
		select {
		case err := <-done:
			if !errors.Is(err, ErrLinkLost) {
				t.Fatalf("PurgeTree = %v, want ErrLinkLost", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("PurgeTree did not return when its link was superseded")
		}
		// The new link's ack of the old purge is ignored.
		b.send(protocol.TreePurged{RootID: "ses_R"})
		h.open(b, "ses_S")
	})

	t.Run("times out at 30 s", func(t *testing.T) {
		clock := &fakeClock{}
		h := newHarness(t, withClock(clock))
		c := h.connect(false)
		done := make(chan error, 1)
		go func() { done <- h.srv.PurgeTree(bg, "ses_R", "u_0a1b2c3d4e") }()
		c.next()
		eventually(t, "the purge timer is armed", func() bool { return clock.pending() == 1 })
		clock.advance(30*time.Second - time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("PurgeTree returned before 30 s: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		clock.advance(time.Millisecond)
		select {
		case err := <-done:
			if !errors.Is(err, ErrPurgeTimeout) {
				t.Fatalf("PurgeTree = %v, want ErrPurgeTimeout", err)
			}
		case <-time.After(time.Second):
			t.Fatal("PurgeTree did not time out at 30 s")
		}
		// A late ack after the timeout is ignored, and the next purge of the
		// root sends its own tree.purge.
		c.send(protocol.TreePurged{RootID: "ses_R"})
		go func() { done <- h.srv.PurgeTree(bg, "ses_R", "u_0a1b2c3d4e") }()
		if f := c.next(); f != (protocol.TreePurge{RootID: "ses_R", PersonRef: "u_0a1b2c3d4e"}) {
			t.Fatalf("frame = %#v", f)
		}
		c.send(protocol.TreePurged{RootID: "ses_R"})
		if err := <-done; err != nil {
			t.Fatalf("second PurgeTree = %v", err)
		}
	})

	t.Run("returns at once with no link", func(t *testing.T) {
		h := newHarness(t)
		start := time.Now()
		if err := h.srv.PurgeTree(bg, "ses_R", "u_0a1b2c3d4e"); err != nil {
			t.Fatalf("PurgeTree with no link = %v, want nil", err)
		}
		if took := time.Since(start); took > 50*time.Millisecond {
			t.Fatalf("PurgeTree with no link took %v", took)
		}
	})

	t.Run("its context bounds it", func(t *testing.T) {
		h := newHarness(t)
		c := h.connect(false)
		ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
		defer cancel()
		go func() { c.next() }()
		if err := h.srv.PurgeTree(ctx, "ses_R", "u_0a1b2c3d4e"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("PurgeTree = %v, want the context's deadline", err)
		}
	})
}

// ---- the real session service and maintenance over the link ----

// lateSessions breaks the construction cycle (the Service needs the Server,
// and the Server's Handlers need the Service), as serve must.
type lateSessions struct{ *sessions.Service }

type noProfile struct{}

func (noProfile) Snapshot(context.Context, string) (string, error) { return "", nil }

type noAttachments struct{}

func (noAttachments) Bind(context.Context, auth.Person, string, []string, string) ([]protocol.ContentBlock, error) {
	return nil, nil
}

type stack struct {
	*harness
	st  *store.Store
	svc *sessions.Service
	mt  *maintenance.Maintenance
	mem *fakeMemory
}

func newStack(t *testing.T, att ...sessions.AttachmentResolver) *stack {
	t.Helper()
	var resolver sessions.AttachmentResolver = noAttachments{}
	if len(att) > 0 {
		resolver = att[0]
	}
	st := storetest.New(t)
	cat, err := agents.LoadCatalog("../../../agents")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	m := metrics.NewCore(reg)
	late := &lateSessions{}
	mem := &fakeMemory{}
	srv := NewServer(Handlers{
		Auth:     internalauth.Middleware(reviewer, audience, principals, internalauth.PrincipalConductor),
		Sessions: late,
		RPC:      MemoryRPC(st, mem),
		Metrics:  m,
	})
	mt, err := maintenance.New(bg, st, srv, m)
	if err != nil {
		t.Fatal(err)
	}
	late.Service = sessions.NewService(st, agents.NewLive(cat), srv, noProfile{}, resolver, mt,
		sessions.NopLedger{}, m, time.Now, sessions.Config{})
	mux := http.NewServeMux()
	srv.Routes(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return &stack{harness: &harness{t: t, srv: srv, reg: reg, http: hs}, st: st, svc: late.Service, mt: mt, mem: mem}
}

var person = auth.Person{ID: "pers-fam", DisplayName: "Synthetic Family", Tier: "family"}

func userMessage(text string) protocol.UserMessage {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return protocol.UserMessage{ClientID: id, Content: []protocol.TextBlock{{Type: "text", Text: text}}}
}

func (s *stack) send(sessionID, text string) {
	s.t.Helper()
	if _, err := s.svc.Send(bg, person, sessionID, userMessage(text)); err != nil {
		s.t.Fatalf("Send: %v", err)
	}
}

func (s *stack) assistant(sessionID string) []protocol.Message {
	s.t.Helper()
	all, err := s.st.Messages(bg, sessionID)
	if err != nil {
		s.t.Fatal(err)
	}
	var out []protocol.Message
	for _, m := range all {
		if m.Role == "assistant" {
			out = append(out, m)
		}
	}
	return out
}

func (s *stack) errorCodes(sessionID string) []protocol.ErrorCode {
	s.t.Helper()
	evs, err := s.st.EventsSince(bg, sessionID, 0, 1000)
	if err != nil {
		s.t.Fatal(err)
	}
	var out []protocol.ErrorCode
	for _, e := range evs {
		if b, ok := e.Body.(protocol.ErrorEvent); ok {
			out = append(out, b.Code)
		}
	}
	return out
}

func (s *stack) turnEnded(sessionID string) func() bool {
	return func() bool { _, running := s.svc.RunningTurn(sessionID); return !running }
}

// A turn runs end to end through the link: Send → host.open + turn.send; the
// conductor's events complete the message; a memory rpc reaches the memory
// service with the row's person; a dropped link fails the next turn.
func TestLinkDrivesTheSessionService(t *testing.T) {
	s := newStack(t)
	c := s.connect(false)
	ses, err := s.svc.Create(bg, person, protocol.CreateSession{Agent: "luma"})
	if err != nil {
		t.Fatal(err)
	}
	s.send(ses.ID, "hello")
	open, ok := c.next().(protocol.HostOpen)
	if !ok || open.SessionID != ses.ID || open.Spec.Person.Ref == "" {
		t.Fatalf("first frame = %#v, want host.open", open)
	}
	ts, ok := c.next().(protocol.TurnSend)
	if !ok || ts.SessionID != ses.ID {
		t.Fatalf("second frame = %#v, want turn.send", ts)
	}
	c.send(protocol.HostState{SessionID: ses.ID, State: "ready", SDKSessionID: "0d9c8b7a-6f5e-4d3c-9b2a-1f0e9d8c7b6a"})
	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts.TurnID, Event: protocol.LinkMessageStarted{Model: "claude-haiku-5-5"}})
	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts.TurnID, Event: protocol.LinkTextDelta{Text: "hi"}})
	c.send(protocol.RPC{RPCID: "rpc_1", SessionID: ses.ID, TurnID: ts.TurnID, Method: "memory.remember", Params: protocol.RPCParams{Text: "likes tea"}})
	if r, ok := c.next().(protocol.RPCResult); !ok || !r.OK || r.Result == nil || r.Result.Status != "applied" {
		t.Fatalf("rpc.result = %#v", r)
	}
	if calls := s.mem.Calls(); len(calls) != 1 || calls[0].person != person.ID || calls[0].session != ses.ID {
		t.Fatalf("memory calls = %v", calls)
	}
	c.send(protocol.EventFrame{SessionID: ses.ID, TurnID: ts.TurnID, Event: protocol.TurnResult{
		SDKUUID: "sdk-1", Content: []protocol.MessagePart{{Type: "text", Text: "hi"}}}})
	eventually(t, "the turn completes", s.turnEnded(ses.ID))
	if asst := s.assistant(ses.ID); len(asst) != 1 || asst[0].Status != "complete" {
		t.Fatalf("assistant messages = %+v", asst)
	}

	s.send(ses.ID, "again")
	if f, ok := c.next().(protocol.TurnSend); !ok {
		t.Fatalf("frame = %#v, want turn.send on the open host", f)
	}
	_ = c.ws.Close(websocket.StatusGoingAway, "restarting")
	eventually(t, "the turn fails", s.turnEnded(ses.ID))
	if got := s.errorCodes(ses.ID); !reflect.DeepEqual(got, []protocol.ErrorCode{protocol.CodeConductorRestarted}) {
		t.Fatalf("error events = %v, want [conductor_restarted]", got)
	}
}

// Maintenance over the real link: the running turn is interrupted after the
// drain; the conductor closing its host (maintenance) fails it with
// `maintenance`; shutdown{maintenance} goes out; new work is refused until
// Off, after which a host reopens lazily.
func TestMaintenanceOverTheLink(t *testing.T) {
	s := newStack(t)
	c := s.connect(false)
	ses, err := s.svc.Create(bg, person, protocol.CreateSession{Agent: "luma"})
	if err != nil {
		t.Fatal(err)
	}
	s.send(ses.ID, "a long one")
	c.next() // host.open
	c.next() // turn.send

	done := make(chan maintenance.Status, 1)
	go func() {
		st, err := s.mt.On(bg, "upgrade", 0)
		if err != nil {
			t.Error(err)
		}
		done <- st
	}()
	if f := c.next(); f != (protocol.TurnInterrupt{SessionID: ses.ID}) {
		t.Fatalf("frame = %#v, want turn.interrupt", f)
	}
	c.send(protocol.HostState{SessionID: ses.ID, State: "closed", Reason: "maintenance"})
	if f := c.next(); f != (protocol.Shutdown{Reason: "maintenance"}) {
		t.Fatalf("frame = %#v, want shutdown{maintenance}", f)
	}
	if st := <-done; !st.Enabled || st.RunningTurns != 0 || st.OpenHosts != 0 {
		t.Fatalf("status = %#v", st)
	}
	if got := s.errorCodes(ses.ID); !reflect.DeepEqual(got, []protocol.ErrorCode{protocol.CodeMaintenance}) {
		t.Fatalf("error events = %v, want [maintenance]", got)
	}
	var refused *sessions.RefusedError
	if _, err := s.svc.Send(bg, person, ses.ID, userMessage("x")); !errors.As(err, &refused) || refused.Info.Code != protocol.CodeMaintenance {
		t.Fatalf("Send in maintenance = %v", err)
	}
	if _, err := s.svc.Create(bg, person, protocol.CreateSession{Agent: "luma"}); !errors.As(err, &refused) || refused.Info.Code != protocol.CodeMaintenance {
		t.Fatalf("Create in maintenance = %v", err)
	}

	if _, err := s.mt.Off(bg); err != nil {
		t.Fatal(err)
	}
	s.send(ses.ID, "back")
	if f, ok := c.next().(protocol.HostOpen); !ok || f.SessionID != ses.ID {
		t.Fatalf("frame after Off = %#v, want host.open", f)
	}
}

// heldAttachments holds Bind until released: a send sits on its actor, past
// the Gate, before its host.open.
type heldAttachments struct {
	entered chan struct{}
	release chan struct{}
}

func (a *heldAttachments) Bind(context.Context, auth.Person, string, []string, string) ([]protocol.ContentBlock, error) {
	close(a.entered)
	<-a.release
	return []protocol.ContentBlock{{Type: "text", Text: "an attached note"}}, nil
}

// S52: a send that passed the Gate just before maintenance closed it is
// delivered before the drain looks at the running turns, so it is drained
// (interrupted) instead of slipping past the shutdown.
func TestMaintenanceWaitsForASendThatPassedTheGate(t *testing.T) {
	att := &heldAttachments{entered: make(chan struct{}), release: make(chan struct{})}
	s := newStack(t, att)
	c := s.connect(false)
	ses, err := s.svc.Create(bg, person, protocol.CreateSession{Agent: "luma"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.InsertAttachment(bg, store.Attachment{ID: "att_1", PersonID: person.ID, MediaType: "text/plain",
		Filename: "note.txt", SizeBytes: 4, SHA256: strings.Repeat("0", 64), Data: []byte("note")}); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		msg := userMessage("read this")
		msg.AttachmentIDs = []string{"att_1"}
		_, err := s.svc.Send(bg, person, ses.ID, msg)
		sent <- err
	}()
	<-att.entered // the send is past the Gate

	done := make(chan maintenance.Status, 1)
	go func() {
		st, err := s.mt.On(bg, "upgrade", 0)
		if err != nil {
			t.Error(err)
		}
		done <- st
	}()
	eventually(t, "the gate closes", func() bool { return s.mt.CheckNewTurn(bg) != nil })
	c.none() // On waits at the barrier: no shutdown ahead of the held send
	close(att.release)

	if f, ok := c.next().(protocol.HostOpen); !ok || f.SessionID != ses.ID {
		t.Fatalf("frame = %#v, want the held send's host.open", f)
	}
	if f, ok := c.next().(protocol.TurnSend); !ok || f.SessionID != ses.ID {
		t.Fatalf("frame = %#v, want its turn.send", f)
	}
	if err := <-sent; err != nil {
		t.Fatalf("the held send = %v", err)
	}
	if f := c.next(); f != (protocol.TurnInterrupt{SessionID: ses.ID}) {
		t.Fatalf("frame = %#v, want the drain's turn.interrupt", f)
	}
	c.send(protocol.HostState{SessionID: ses.ID, State: "closed", Reason: "maintenance"})
	if f := c.next(); f != (protocol.Shutdown{Reason: "maintenance"}) {
		t.Fatalf("frame = %#v, want shutdown{maintenance}", f)
	}
	if st := <-done; !st.Enabled || st.OpenHosts != 0 || st.RunningTurns != 0 {
		t.Fatalf("status = %#v", st)
	}
	if got := s.errorCodes(ses.ID); !reflect.DeepEqual(got, []protocol.ErrorCode{protocol.CodeMaintenance}) {
		t.Fatalf("error events = %v, want [maintenance]", got)
	}
}
