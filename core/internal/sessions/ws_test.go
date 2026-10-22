package sessions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// ---- helpers ----

// emit appends n memory.write events to a session through the sink, as the
// memory package does, and returns their ids. It reports failures with
// t.Error, so it may run off the test goroutine.
func (h *harness) emit(sessionID string, n int) []int64 {
	ids := make([]int64, 0, n)
	sink := h.svc.Sink()
	for i := range n {
		sid := sessionID
		id, err := sink.Emit(bg, sessionID, "memory.write", protocol.MemoryWriteEvent{Write: protocol.MemoryWrite{
			ID: fmt.Sprintf("mem_%d", i), SessionID: &sid, TurnID: "turn_synthetic", Op: "remember",
			Text: "synthetic", Status: "pending", CreatedAt: testNow,
		}})
		if err != nil {
			h.t.Error(err)
			return ids
		}
		ids = append(ids, id)
	}
	return ids
}

// subscribers is the number of live subscribers of a session in the hub.
func (h *harness) subscribers(sessionID string) int {
	h.svc.hub.mu.Lock()
	defer h.svc.hub.mu.Unlock()
	return len(h.svc.hub.subs[sessionID])
}

// dropSubscribers drops a session's subscribers as the hub does when one
// falls too far behind.
func (h *harness) dropSubscribers(sessionID string) {
	h.svc.hub.mu.Lock()
	defer h.svc.hub.mu.Unlock()
	for ch := range h.svc.hub.subs[sessionID] {
		h.svc.hub.drop(sessionID, ch)
	}
}

func (h *harness) waitSubscribers(sessionID string, n int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.subscribers(sessionID) != n {
		if time.Now().After(deadline) {
			h.t.Fatalf("session %s has %d subscribers, want %d", sessionID, h.subscribers(sessionID), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func eventIDs(evs []protocol.Event) []int64 {
	out := make([]int64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

// collect reads exactly n events from a subscription, then checks that no
// further event arrives.
func collect(t *testing.T, ch <-chan protocol.Event, n int) []protocol.Event {
	t.Helper()
	timeout := time.After(20 * time.Second)
	out := make([]protocol.Event, 0, n)
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("the subscription closed after %d of %d events", len(out), n)
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("received %d of %d events", len(out), n)
		}
	}
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("an extra event %d (%s) arrived", ev.ID, ev.Body.EventType())
		}
	case <-time.After(100 * time.Millisecond):
	}
	return out
}

func strictlyIncreasing(t *testing.T, ids []int64) {
	t.Helper()
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("event ids not strictly increasing at %d: %d after %d", i, ids[i], ids[i-1])
		}
	}
}

// handshake is the HTTP answer to a stream handshake: 101, or the refusal's
// status and (up to 1 KiB of) body.
type handshake struct {
	status int
	body   []byte
}

// dialFrom opens a session's event stream with the cookie, from origin (""
// sends no Origin header); since "" sends none.
func (w *web) dialFrom(origin string, c *http.Cookie, sessionID, since string, opts ...func(*websocket.DialOptions)) (*websocket.Conn, handshake, error) {
	w.h.t.Helper()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	if c != nil {
		hdr.Set("Cookie", c.String())
	}
	o := &websocket.DialOptions{HTTPHeader: hdr}
	for _, f := range opts {
		f(o)
	}
	ctx, cancel := context.WithTimeout(bg, time.Minute)
	w.h.t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(w.srv.URL, "http") + "/v1/sessions/" + sessionID + "/stream"
	if since != "" {
		url += "?since=" + since
	}
	conn, resp, err := websocket.Dial(ctx, url, o)
	var hs handshake
	if resp != nil {
		hs.status = resp.StatusCode
		if resp.Body != nil { // only a refusal keeps one
			hs.body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
	}
	if conn != nil {
		conn.SetReadLimit(-1)
		w.h.t.Cleanup(func() { _ = conn.CloseNow() })
	}
	return conn, hs, err
}

// dial opens a session's stream from the allowed origin; it must upgrade.
func (w *web) dial(c *http.Cookie, sessionID string, since int64, opts ...func(*websocket.DialOptions)) *websocket.Conn {
	w.h.t.Helper()
	conn, hs, err := w.dialFrom(w.origin, c, sessionID, strconv.FormatInt(since, 10), opts...)
	if err != nil {
		w.h.t.Fatalf("dial %s since %d: %v (status %d %s)", sessionID, since, err, hs.status, hs.body)
	}
	return conn
}

// next reads one event frame.
func next(t *testing.T, c *websocket.Conn) protocol.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("frame type %v, want text", typ)
	}
	ev, err := protocol.DecodeEvent(b)
	if err != nil {
		t.Fatalf("frame %s: %v", b, err)
	}
	return ev
}

// expectSessionEnded reads until the server closes the socket, which must be
// with 4401 (the web session ended; the PWA goes to sign-in).
func expectSessionEnded(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		if got := websocket.CloseStatus(err); got != 4401 {
			t.Fatalf("socket ended with %v (status %d), want close 4401", err, got)
		}
		return
	}
}

// stillOpen checks a socket delivers a fresh event.
func stillOpen(t *testing.T, h *harness, c *websocket.Conn, sessionID string) {
	t.Helper()
	id := h.emit(sessionID, 1)
	if ev := next(t, c); ev.ID != id[0] {
		t.Fatalf("read event %d, want the fresh %d", ev.ID, id[0])
	}
}

// ---- replay ----

// Review Focus 3 (Android backgrounding): subscribing to the live hub
// before reading the log, and dropping what the log already gave, means a
// subscriber sees every event after since exactly once, in id order, however
// its subscribe interleaves with the session's writers. Ids are a global
// identity, monotonic but not contiguous (S20).
func TestReplaySinceNoGapsNoDupes(t *testing.T) {
	t.Run("concurrent emits", func(t *testing.T) {
		h := newHarness(t)
		ses, noise := h.newSession(family), h.newSession(family)
		before := h.emit(ses.ID, 3)
		since := before[len(before)-1]

		// Events written between the live subscribe and the log read are in
		// both: they must arrive once. Swapping the two steps would lose them.
		var hookMu sync.Mutex
		var hookIDs []int64
		h.svc.streams.beforeReplay = func() {
			ids := h.emit(ses.ID, 5)
			hookMu.Lock()
			hookIDs = append(hookIDs, ids...)
			hookMu.Unlock()
		}

		const total = 500
		progress := make(chan int, total)
		var emitted []int64
		var wg sync.WaitGroup
		stop := make(chan struct{})
		t.Cleanup(func() { close(stop); wg.Wait() }) // before the store closes
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range total {
				select {
				case <-stop:
					return
				default:
				}
				emitted = append(emitted, h.emit(ses.ID, 1)...)
				if i%7 == 0 {
					h.emit(noise.ID, 1) // another session takes ids in between
				}
				progress <- i + 1
			}
		}()
		ctx, cancel := context.WithCancel(bg)
		t.Cleanup(cancel)
		var subs []<-chan protocol.Event
		done := 0
		for _, at := range []int{0, 100, 250, total - 1} {
			for done < at {
				done = <-progress
			}
			ch, err := h.svc.Subscribe(ctx, family, ses.ID, since)
			if err != nil {
				t.Fatal(err)
			}
			subs = append(subs, ch)
		}
		wg.Wait()

		want := append(slices.Clone(emitted), hookIDs...)
		slices.Sort(want)
		if len(want) != total+5*len(subs) {
			t.Fatalf("emitted %d events, want %d", len(want), total+5*len(subs))
		}
		for i, ch := range subs {
			got := eventIDs(collect(t, ch, len(want)))
			strictlyIncreasing(t, got)
			if !slices.Equal(got, want) {
				t.Fatalf("subscriber %d received %d ids, not exactly the %d emitted after since", i, len(got), len(want))
			}
		}
	})

	t.Run("socket dies mid-turn", func(t *testing.T) {
		h := newHarness(t)
		w := newWeb(h)
		c := w.login(family)
		ses := h.newSession(family)
		h.send(family, ses.ID, "tell me a story")
		h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku})

		var seen []protocol.Event
		ws1 := w.dial(c, ses.ID, 0)
		for i := 1; i <= 10; i++ {
			h.link(ses.ID, protocol.LinkTextDelta{Text: fmt.Sprintf("d%02d ", i)})
		}
		for {
			ev := next(t, ws1)
			seen = append(seen, ev)
			if d, ok := ev.Body.(protocol.MessageTextDelta); ok && d.Text == "d05 " {
				break
			}
		}
		_ = ws1.CloseNow() // the phone went to the background mid-answer
		h.waitSubscribers(ses.ID, 0)

		var story strings.Builder
		for i := 1; i <= 20; i++ {
			fmt.Fprintf(&story, "d%02d ", i)
			if i > 10 {
				h.link(ses.ID, protocol.LinkTextDelta{Text: fmt.Sprintf("d%02d ", i)})
			}
		}
		h.link(ses.ID, result("sdk-story", story.String(), 0.02))

		ws2 := w.dial(c, ses.ID, seen[len(seen)-1].ID) // back: reconnect with since
		for {
			ev := next(t, ws2)
			seen = append(seen, ev)
			if st, ok := ev.Body.(protocol.SessionStatus); ok && st.Status == "idle" {
				break
			}
		}

		var text strings.Builder
		completed := 0
		for _, ev := range seen {
			switch b := ev.Body.(type) {
			case protocol.MessageTextDelta:
				text.WriteString(b.Text)
			case protocol.MessageCompleted:
				completed++
				if b.Message.Status != "complete" || len(b.Message.Content) != 1 || b.Message.Content[0].Text != story.String() {
					t.Fatalf("message.completed = %+v", b.Message)
				}
			}
		}
		if text.String() != story.String() {
			t.Fatalf("deltas seen = %q, want each once, in order: %q", text.String(), story.String())
		}
		if completed != 1 {
			t.Fatalf("message.completed seen %d times, want 1", completed)
		}
		got := eventIDs(seen)
		strictlyIncreasing(t, got)
		if stored := eventIDs(h.events(ses.ID, "")); !slices.Equal(got, stored) {
			t.Fatalf("the two sockets saw %v, want exactly the stored log %v", got, stored)
		}
	})
}

// ---- Origin and cookie ----

// The handshake's Origin must be exactly an allowed one (S17); the cookie
// must name a live web session.
func TestWSRejectsForeignOrigin(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	c := w.login(family)
	ses := h.newSession(family)
	for _, origin := range []string{"https://evil.example", "", w.origin + "/", "null"} {
		_, hs, err := w.dialFrom(origin, c, ses.ID, "0")
		if err == nil || hs.status != http.StatusForbidden {
			t.Fatalf("Origin %q: err %v, status %d; want 403 and no upgrade", origin, err, hs.status)
		}
	}
	for name, cookie := range map[string]*http.Cookie{
		"no cookie":      nil,
		"unknown cookie": {Name: devCookie, Value: strings.Repeat("A", 43)},
	} {
		_, hs, err := w.dialFrom(w.origin, cookie, ses.ID, "0")
		if err == nil || hs.status != http.StatusUnauthorized {
			t.Fatalf("%s: err %v, status %d; want 401 and no upgrade", name, err, hs.status)
		}
	}
	if n := h.subscribers(ses.ID); n != 0 {
		t.Fatalf("refused handshakes left %d subscribers", n)
	}
	ws := w.dial(c, ses.ID, 0)
	stillOpen(t, h, ws, ses.ID)
}

// The allowlist decides, not the Host header: an allowed origin other than
// the host Core is reached on (Traefik, LUMA_ALLOWED_ORIGINS) upgrades.
func TestWSAllowsConfiguredOriginOtherThanHost(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h, "https://luma.example")
	c := w.login(family)
	ses := h.newSession(family)
	conn, hs, err := w.dialFrom("https://luma.example", c, ses.ID, "0")
	if err != nil {
		t.Fatalf("an allowed origin other than the Host was refused: %v (status %d)", err, hs.status)
	}
	stillOpen(t, h, conn, ses.ID)
}

// Before the upgrade a stream is refused with a REST error: another person's
// or an unknown session is 404, a bad since is 400.
func TestWSRefusedBeforeUpgrade(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	c := w.login(family)
	mine, theirs := h.newSession(family), h.newSession(other)
	for _, tc := range []struct {
		session, since string
		status         int
	}{
		{theirs.ID, "0", http.StatusNotFound},
		{"ses_00000000000000000000000000", "0", http.StatusNotFound},
		{mine.ID, "-1", http.StatusBadRequest},
		{mine.ID, "seven", http.StatusBadRequest},
	} {
		_, hs, err := w.dialFrom(w.origin, c, tc.session, tc.since)
		if err == nil {
			t.Fatalf("%s since %s upgraded", tc.session, tc.since)
		}
		wantErrorInfo(t, hs.status, hs.body, tc.status, protocol.CodeUpstreamError, protocol.ErrorCopy[protocol.CodeUpstreamError])
	}
	// No since replays from the start.
	conn, _, err := w.dialFrom(w.origin, c, mine.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	ids := h.emit(mine.ID, 1)
	if ev := next(t, conn); ev.ID != ids[0] {
		t.Fatalf("read %d, want %d", ev.ID, ids[0])
	}
}

// ---- session end ----

// Logout closes the sockets of that web session with 4401; the same person's
// socket on another device stays open.
func TestWSClosedOnLogout(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	phone, laptop := w.login(family), w.login(family)
	ses := h.newSession(family)
	a, a2, b := w.dial(phone, ses.ID, 0), w.dial(phone, ses.ID, 0), w.dial(laptop, ses.ID, 0)
	w.logout(phone)
	expectSessionEnded(t, a)
	expectSessionEnded(t, a2)
	stillOpen(t, h, b, ses.ID)
}

// A registry change that drops a person ends their web sessions, and so
// closes their sockets with 4401; other people's stay open.
func TestWSClosedOnRegistryRemoval(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	mine, theirs := h.newSession(family), h.newSession(other)
	a := w.dial(w.login(family), mine.ID, 0)
	b := w.dial(w.login(other), theirs.ID, 0)
	without := strings.Replace(webRegistry, `  pers-fam:
    display_name: Synthetic Family
    tier: family
    emails:
      mock: [fam@example.test]
`, "", 1)
	if without == webRegistry {
		t.Fatal("the registry edit removed nobody")
	}
	writeFile(t, w.regPath, without)
	reg, err := auth.LoadRegistry(w.regPath)
	if err != nil {
		t.Fatal(err)
	}
	w.bff.SetRegistry(reg)
	expectSessionEnded(t, a)
	stillOpen(t, h, b, theirs.ID)
}

// The socket is registered for session ends before the upgrade, and the web
// session is read again after registering (Task 8 review): one that ends
// between RequirePerson and the upgrade gets 401 and no upgrade, and one that
// ends right after the upgrade closes the socket with 4401.
func TestWSSessionEndedDuringUpgrade(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	ses := h.newSession(family)

	c := w.login(family)
	h.svc.streams.afterAuth = func() { w.logout(c) }
	_, hs, err := w.dialFrom(w.origin, c, ses.ID, "0")
	if err == nil || hs.status != http.StatusUnauthorized {
		t.Fatalf("a session ended before the upgrade: err %v, status %d; want 401", err, hs.status)
	}
	if n := h.subscribers(ses.ID); n != 0 {
		t.Fatalf("the refused stream subscribed (%d subscribers)", n)
	}

	h.svc.streams.afterAuth = nil
	c = w.login(family)
	h.svc.streams.afterAccept = func() { w.logout(c) }
	ws := w.dial(c, ses.ID, 0)
	expectSessionEnded(t, ws)
	h.waitSubscribers(ses.ID, 0)
}

// ---- backpressure and liveness ----

// A subscriber more than 1000 events behind is dropped, so the hub never
// blocks the session's actor; the client reconnects with since. Over a
// socket, the drop closes it with 1013 (try again later).
func TestSlowSubscriberDropped(t *testing.T) {
	t.Run("subscription", func(t *testing.T) {
		h := newHarness(t)
		ses := h.newSession(family)
		first := h.emit(ses.ID, 1)
		ctx, cancel := context.WithCancel(bg)
		t.Cleanup(cancel)
		ch, err := h.svc.Subscribe(ctx, family, ses.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if ev := <-ch; ev.ID != first[0] { // the replay is done; the rest is live
			t.Fatalf("replayed %d, want %d", ev.ID, first[0])
		}

		emitted := make(chan []int64, 1)
		go func() { emitted <- h.emit(ses.ID, 1100) }() // nobody reads meanwhile
		var ids []int64
		select {
		case ids = <-emitted:
		case <-time.After(2 * time.Minute):
			t.Fatal("the session's writes blocked behind a subscriber that does not read")
		}
		if len(ids) != 1100 {
			t.Fatalf("emitted %d", len(ids))
		}
		var got []int64
		for ev := range ch {
			got = append(got, ev.ID)
		}
		if len(got) < 1000 || len(got) > 1001 {
			t.Fatalf("a dropped subscriber received %d events, want the 1000 (or 1001 with the one in hand) it was behind by", len(got))
		}
		if !slices.Equal(got, ids[:len(got)]) {
			t.Fatal("the dropped subscriber's events are not the oldest ones, in order")
		}
		if n := h.subscribers(ses.ID); n != 0 {
			t.Fatalf("the dropped subscriber is still registered (%d)", n)
		}
	})

	t.Run("socket", func(t *testing.T) {
		h := newHarness(t)
		w := newWeb(h)
		c := w.login(family)
		ses := h.newSession(family)
		ws := w.dial(c, ses.ID, 0)
		ids := h.emit(ses.ID, 2)
		last := next(t, ws)
		h.waitSubscribers(ses.ID, 1)
		h.dropSubscribers(ses.ID)
		// The socket may still deliver what was in flight, then must close.
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				if got := websocket.CloseStatus(err); got != websocket.StatusTryAgainLater {
					t.Fatalf("a dropped socket ended with %v, want close 1013", err)
				}
				break
			}
			ev, derr := protocol.DecodeEvent(b)
			if derr != nil {
				t.Fatal(derr)
			}
			last = ev
		}
		more := h.emit(ses.ID, 1)
		ws2 := w.dial(c, ses.ID, last.ID)
		var rest []int64
		for len(rest) == 0 || rest[len(rest)-1] != more[0] {
			rest = append(rest, next(t, ws2).ID)
		}
		want := slices.DeleteFunc(append(slices.Clone(ids), more...), func(id int64) bool { return id <= last.ID })
		if !slices.Equal(rest, want) {
			t.Fatalf("after reconnecting with since=%d got %v, want %v", last.ID, rest, want)
		}
	})
}

// The server pings every interval; a peer that stops answering is closed.
func TestWSPingsAndClosesDeadPeer(t *testing.T) {
	h := newHarness(t)
	h.svc.streams.pingEvery, h.svc.streams.pongWait = 40*time.Millisecond, 100*time.Millisecond
	w := newWeb(h)
	c := w.login(family)
	ses := h.newSession(family)

	var pings atomic.Int64
	alive := w.dial(c, ses.ID, 0, func(o *websocket.DialOptions) {
		o.OnPingReceived = func(context.Context, []byte) bool { pings.Add(1); return true }
	})
	aliveErr := make(chan error, 1)
	go func() {
		_, _, err := alive.Read(bg)
		aliveErr <- err
	}()
	dead := w.dial(c, ses.ID, 0, func(o *websocket.DialOptions) {
		o.OnPingReceived = func(context.Context, []byte) bool { return false } // never pongs
	})
	deadErr := make(chan error, 1)
	go func() {
		_, _, err := dead.Read(bg)
		deadErr <- err
	}()

	select {
	case <-deadErr:
	case <-time.After(5 * time.Second):
		t.Fatal("a peer that never answers pings was not closed")
	}
	time.Sleep(300 * time.Millisecond)
	if n := pings.Load(); n < 3 {
		t.Fatalf("an answering peer got %d pings in ~0.4 s at a 40 ms interval", n)
	}
	select {
	case err := <-aliveErr:
		t.Fatalf("the answering peer was closed: %v", err)
	default:
	}
	h.waitSubscribers(ses.ID, 1)
	if pingEvery != 25*time.Second {
		t.Fatalf("pingEvery = %v, want 25s (C1)", pingEvery)
	}
}

// S45: a link error marks the turn's message and reaches the client while
// the turn is still running; the socket just forwards it, and the turn's
// turn_result follows on the same socket.
func TestWSForwardsErrorWithoutEndingStream(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	ses := h.newSession(family)
	ws := w.dial(w.login(family), ses.ID, 0)
	h.send(family, ses.ID, "hi")
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkError{Code: protocol.CodeOverloaded, Retryable: true})
	var types []string
	for {
		ev := next(t, ws)
		types = append(types, ev.Body.EventType())
		if ev.Body.EventType() == "error" {
			break
		}
	}
	h.link(ses.ID, result("sdk-1", "", 0.01))
	for {
		ev := next(t, ws)
		types = append(types, ev.Body.EventType())
		if st, ok := ev.Body.(protocol.SessionStatus); ok && st.Status == "idle" {
			break
		}
	}
	want := []string{"message.user", "session.status", "message.started", "error", "message.completed", "usage", "session.status"}
	if !slices.Equal(types, want) {
		t.Fatalf("socket saw %v, want %v", types, want)
	}
	stillOpen(t, h, ws, ses.ID)
}
