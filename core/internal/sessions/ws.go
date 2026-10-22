package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// The event stream, GET /v1/sessions/{id}/stream?since=<id> (C1): a
// WebSocket of server → client Event frames. A turn runs to completion
// whether or not anyone is connected; a client that comes back reconnects
// with the last id it saw and gets every later event once, in id order.

const (
	// subscriberBuffer is how far a subscriber may fall behind the session.
	// One event more and the hub drops it (its channel closes) rather than
	// block the session's actor; the client reconnects with since.
	subscriberBuffer = 1000
	// replayPage is the page size of the log read after since.
	replayPage = 500
	// pingEvery is C1's ping interval; pongWait bounds the wait for each pong.
	pingEvery = 25 * time.Second
	pongWait  = 20 * time.Second
	// writeWait bounds one frame's write: a client that cannot take a frame
	// in that time is closed.
	writeWait = 10 * time.Second
)

// The stream's close codes (C1). Any other close (1006, a network drop)
// means reconnect with since.
const (
	// closeSessionEnded: the web session ended; sign in again (Task 21).
	closeSessionEnded websocket.StatusCode = 4401
	// closeTreeDeleted: the conversation's tree was deleted; drop the thread.
	closeTreeDeleted websocket.StatusCode = 4404
	// websocket.StatusTryAgainLater (1013): too far behind, or the log could
	// not be read for now; reconnect with since.
	// websocket.StatusInternalError (1011): a stored event cannot be read,
	// and never will be; do not reconnect.
)

// subscription is a stream of one session's events. end says why events
// closed; the pump writes it before the close, so a receiver that sees the
// channel closed reads it.
type subscription struct {
	events <-chan protocol.Event
	end    subEnd
}

// Subscribe streams a session's events with id > since: first the stored
// log, then live events. It subscribes to the live hub before it reads the
// log and drops live events the log already gave, so no event is missed or
// repeated: the session's ids commit in id order (S38) and its actor
// publishes each one after its commit, so an event is either in the log read
// or published after the subscription.
//
// The channel closes when ctx ends, when the subscriber falls more than
// subscriberBuffer events behind, when the session's tree is deleted, or when
// the log cannot be read; the caller resumes with since = the last id it
// received. Only the session's owner may subscribe (ErrNotFound otherwise).
func (s *Service) Subscribe(ctx context.Context, p auth.Person, sessionID string, since int64) (<-chan protocol.Event, error) {
	sub, err := s.subscribe(ctx, p, sessionID, since)
	if err != nil {
		return nil, err
	}
	return sub.events, nil
}

func (s *Service) subscribe(ctx context.Context, p auth.Person, sessionID string, since int64) (*subscription, error) {
	if since < 0 {
		return nil, fmt.Errorf("%w: since must not be negative", ErrInvalid)
	}
	live, unsubscribe := s.hub.subscribe(sessionID, subscriberBuffer)
	// The owner check follows the live subscription, so a tree deleted in
	// between either has no row now, or drops this subscriber with it.
	if _, err := s.owned(ctx, p, sessionID); err != nil {
		unsubscribe()
		return nil, err
	}
	if hook := s.streams.beforeReplay; hook != nil {
		hook()
	}
	out := make(chan protocol.Event)
	sub := &subscription{events: out}
	go s.pump(ctx, sub, out, sessionID, since, live, unsubscribe)
	return sub, nil
}

// pump delivers the log after since, then the live events after the last id
// delivered, and records why it stopped in sub.end.
func (s *Service) pump(ctx context.Context, sub *subscription, out chan<- protocol.Event, sessionID string, since int64, live *liveSub, unsubscribe func()) {
	defer close(out) // after sub.end is set
	defer unsubscribe()
	last := since
	deliver := func(ev protocol.Event) bool {
		select {
		case out <- ev:
			last = ev.ID
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		page, err := s.st.EventsSince(ctx, sessionID, last, replayPage)
		for _, ev := range page { // with a decode error, the events before it
			if !deliver(ev) {
				return
			}
		}
		if err != nil {
			var bad *store.EventDecodeError
			switch {
			case ctx.Err() != nil:
				sub.end = endClosed
			case errors.As(err, &bad):
				sub.end = endPermanent
				s.streams.badEvent(sessionID, bad.EventID)
			default:
				sub.end = endTransient
				slog.Warn("sessions: replay read failed; the client will retry", "session", sessionID, "err", err)
			}
			return
		}
		if len(page) < replayPage {
			break
		}
	}
	for {
		select {
		case ev, ok := <-live.ch:
			if !ok {
				sub.end = live.end
				if live.end == endBehind {
					slog.Info("sessions: stream subscriber fell behind; dropped", "session", sessionID, "last_event", last)
				}
				return
			}
			if ev.ID <= last {
				continue // the log read had it
			}
			if !deliver(ev) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// handleStream upgrades to the event stream. RequirePerson has checked the
// cookie, and held the handshake's Origin to the allowlist (a foreign
// Origin, or a WebSocket handshake with none, is 403 before the cookie is
// read, S17).
//
// No session end is missed (Task 8 review): the socket is registered under
// its web session and person before the upgrade, then the web session's row
// is read again. A session that ended after RequirePerson read it has no row
// (every end deletes the row before OnSessionEnded fires) and gets 401 with
// no upgrade; one that ends after the registration closes the socket with
// 4401.
func (s *Service) handleStream(w http.ResponseWriter, r *http.Request, p auth.Person) {
	// Exactly one Origin: RequirePerson refused a foreign one, but it knows a
	// handshake only by "Upgrade: websocket", so one sent as "Upgrade: h2c,
	// websocket" (which Accept takes) passes it with none.
	if len(r.Header.Values("Origin")) != 1 {
		http.Error(w, "forbidden: origin not allowed", http.StatusForbidden)
		return
	}
	hash, ok := auth.WebSessionIDHashFrom(r.Context())
	if !ok {
		unauthenticated(w)
		return
	}
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if hook := s.streams.afterAuth; hook != nil {
		hook()
	}
	st := s.streams.add(hash, p.ID)
	defer s.streams.remove(st)
	if _, err := s.st.WebSession(r.Context(), hash, s.clock()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			unauthenticated(w)
			return
		}
		writeError(w, r, err)
		return
	}
	// The stream outlives the request's context once the connection is
	// hijacked; it ends when the socket closes or the handler returns.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	sessionID := r.PathValue("id")
	sub, err := s.subscribe(ctx, p, sessionID, since)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if st.isEnded() {
		unauthenticated(w)
		return
	}
	// The Origin allowlist is RequirePerson's (above). coder/websocket's own
	// check only compares the Origin's host with the Host header, which
	// would refuse an allowed origin that differs from the Host Core sees
	// (LUMA_ALLOWED_ORIGINS, Task 15), so it is not used.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept has answered the request
	}
	defer func() { _ = c.CloseNow() }()
	s.serveStream(ctx, c, sessionID, st, sub)
}

// serveStream writes the events to the socket until it closes, the web
// session ends (4401), or the subscription ends (closeFor).
func (s *Service) serveStream(ctx context.Context, c *websocket.Conn, sessionID string, st *stream, sub *subscription) {
	// The client sends nothing. CloseRead answers its pings and close frame,
	// closes the socket on a data message, and ends ctx when the socket goes.
	ctx = c.CloseRead(ctx)
	if hook := s.streams.afterAccept; hook != nil {
		hook()
	}
	go s.keepAlive(ctx, c, sessionID)
	for {
		select {
		case <-st.ended:
			_ = c.Close(closeSessionEnded, "session ended")
			return
		case <-ctx.Done():
			return
		case ev, ok := <-sub.events:
			if !ok {
				closeFor(c, sub.end)
				return
			}
			if st.isEnded() {
				_ = c.Close(closeSessionEnded, "session ended")
				return
			}
			if err := writeEvent(ctx, c, ev); err != nil {
				return
			}
		}
	}
}

// closeFor closes the socket of a subscription that ended, with the code
// that tells the client what to do (C1).
func closeFor(c *websocket.Conn, end subEnd) {
	switch end {
	case endBehind, endTransient:
		_ = c.Close(websocket.StatusTryAgainLater, "reconnect with since")
	case endDeleted:
		_ = c.Close(closeTreeDeleted, "conversation deleted")
	case endPermanent:
		_ = c.Close(websocket.StatusInternalError, "a stored event cannot be read")
	case endClosed: // the stream's own context ended: the handler is returning
	}
}

func writeEvent(ctx context.Context, c *websocket.Conn, ev protocol.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		slog.Error("sessions: encoding an event failed", "session", ev.SessionID, "event", ev.ID, "err", err)
		_ = c.Close(websocket.StatusInternalError, "encoding failed")
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeWait)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b) // a timeout closes the socket
}

// keepAlive pings every pingEvery and closes a peer that does not answer
// within pongWait (a phone that went away without closing).
func (s *Service) keepAlive(ctx context.Context, c *websocket.Conn, sessionID string) {
	every, wait := s.streams.intervals()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, wait)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					slog.Info("sessions: stream peer stopped answering pings; closing", "session", sessionID)
				}
				_ = c.CloseNow()
				return
			}
		}
	}
}

func parseSince(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: since must be an event id", ErrInvalid)
	}
	return n, nil
}

// ---- session ends ----

// WebSessionEnded closes, with 4401, every stream of a web session that
// ended: logout, expiry, a login replacing it, or a registry change. It is
// the BFF's OnSessionEnded subscriber (Task 15 wires
// bff.OnSessionEnded(svc.WebSessionEnded)). A logout on one device leaves the
// person's other web sessions, and their streams, open. An empty hash ends
// every stream of the person. It never blocks, and a repeat is harmless.
func (s *Service) WebSessionEnded(webSessionIDHash []byte, personID string) {
	s.streams.end(webSessionIDHash, personID)
}

// streams indexes the open streams by web session hash and by person.
type streams struct {
	mu       sync.Mutex
	byHash   map[string]map[*stream]struct{}
	byPerson map[string]map[*stream]struct{}
	// badEvents are the stored events already reported as undecodable.
	badEvents map[int64]struct{}

	// Test hooks, nil in production: afterAuth runs once RequirePerson has
	// admitted a handshake, afterAccept once the socket is upgraded, and
	// beforeReplay between Subscribe's live subscription and its log read.
	afterAuth, afterAccept, beforeReplay func()
	// pingEvery and pongWait replace the constants when set (tests).
	pingEvery, pongWait time.Duration
}

// badEvent reports a stored event that cannot be decoded, at error level
// once per event (a client that retries anyway does not repeat it). Only the
// ids are logged: the payload is the person's content.
func (t *streams) badEvent(sessionID string, eventID int64) {
	t.mu.Lock()
	_, seen := t.badEvents[eventID]
	if t.badEvents == nil {
		t.badEvents = map[int64]struct{}{}
	}
	t.badEvents[eventID] = struct{}{}
	t.mu.Unlock()
	if seen {
		slog.Debug("sessions: stream stopped at an undecodable event again", "session", sessionID, "event", eventID)
		return
	}
	slog.Error("sessions: a stored event cannot be decoded; its session's stream stops there (1011)", "session", sessionID, "event", eventID)
}

// stream is one open (or opening) socket's registration.
type stream struct {
	hash, person string
	ended        chan struct{} // closed when its web session ends
	once         sync.Once
}

func (st *stream) end() { st.once.Do(func() { close(st.ended) }) }

func (st *stream) isEnded() bool {
	select {
	case <-st.ended:
		return true
	default:
		return false
	}
}

func (t *streams) add(hash []byte, person string) *stream {
	st := &stream{hash: string(hash), person: person, ended: make(chan struct{})}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byHash == nil {
		t.byHash, t.byPerson = map[string]map[*stream]struct{}{}, map[string]map[*stream]struct{}{}
	}
	addStream(t.byHash, st.hash, st)
	addStream(t.byPerson, st.person, st)
	return st
}

func (t *streams) remove(st *stream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	removeStream(t.byHash, st.hash, st)
	removeStream(t.byPerson, st.person, st)
}

func (t *streams) end(hash []byte, person string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	set := t.byHash[string(hash)]
	if len(hash) == 0 {
		set = t.byPerson[person]
	}
	for st := range set {
		st.end()
	}
}

func (t *streams) intervals() (every, wait time.Duration) {
	every, wait = pingEvery, pongWait
	if t.pingEvery > 0 {
		every = t.pingEvery
	}
	if t.pongWait > 0 {
		wait = t.pongWait
	}
	return every, wait
}

func addStream(m map[string]map[*stream]struct{}, key string, st *stream) {
	if m[key] == nil {
		m[key] = map[*stream]struct{}{}
	}
	m[key][st] = struct{}{}
}

func removeStream(m map[string]map[*stream]struct{}, key string, st *stream) {
	delete(m[key], st)
	if len(m[key]) == 0 {
		delete(m, key)
	}
}
