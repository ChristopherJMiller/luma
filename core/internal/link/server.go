// Package link is Core's end of the conductor link (C2): one WebSocket at
// GET /internal/v1/conductor, open only to the conductor's ServiceAccount
// (Task 8b). The newest authenticated link wins (P30); a link that stops
// answering pings is lost. Frames from the conductor go to the session
// service in frame order, and only for sessions opened on that link and their
// running turn. Memory rpcs are routed by the session row, never by params.
package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/ChristopherJMiller/luma/core/internal/internalauth"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// Path is the link's route on the internal port (C10).
const Path = "/internal/v1/conductor"

const (
	pingInterval = 10 * time.Second // Core pings the conductor this often
	pongTimeout  = 30 * time.Second // no pong for this long: the link is lost
	purgeTimeout = 30 * time.Second // the longest PurgeTree waits for tree.purged (S39)
	writeTimeout = 30 * time.Second // one frame's write; a turn.send can carry ~27 MiB of base64
	readLimit    = 8 << 20          // the largest frame Core reads (a turn_result with a long answer)

	closeSuperseded websocket.StatusCode = 4000 // a newer authenticated link took over (P30)
)

var (
	// ErrNotConnected: no conductor link is up.
	ErrNotConnected = errors.New("link: no conductor connected")
	// ErrPurgeTimeout: the conductor did not ack a tree.purge within 30 s.
	ErrPurgeTimeout = errors.New("link: tree.purge not acked within 30s")
	// ErrLinkLost: the link a tree.purge went out on was lost or superseded
	// before its ack. Its hosts closed with it.
	ErrLinkLost = errors.New("link: conductor link lost before tree.purged")
)

// The rejection reasons of luma_link_rejected_total.
const (
	rejectUnopened  = "unopened"   // a frame for a session not opened on this link
	rejectStaleTurn = "stale_turn" // an event or rpc for a turn that is not the session's running one
	rejectMalformed = "malformed"  // undecodable, or a Core -> conductor frame sent the wrong way
)

// Sessions is the session service as the link drives it; *sessions.Service
// implements it. OnLinkEvent is called in frame order from one goroutine.
type Sessions interface {
	OnLinkEvent(sessionID, turnID string, ev protocol.LinkEvent)
	OnHostState(hs protocol.HostState)
	OnConductorLost(sessionIDs []string)
	RunningTurn(sessionID string) (turnID string, ok bool)
}

// RPCHandler serves one memory rpc of a session's running turn (MemoryRPC
// routes them to Task 13's memory service). params are the rpc's params.
type RPCHandler func(ctx context.Context, sessionID, turnID, method string, params json.RawMessage) (protocol.MemoryResult, error)

// Handlers are what the link needs. Auth is internalauth.Middleware(…,
// internalauth.PrincipalConductor); the link also checks the principal
// itself, so a missing or wider middleware lets nobody else in. A nil RPC
// answers every memory rpc `disabled`.
type Handlers struct {
	Auth     func(http.Handler) http.Handler
	Sessions Sessions
	RPC      RPCHandler
	Metrics  *metrics.Core
}

// Server is Core's end of the conductor link. It implements
// sessions.Conductor and maintenance's view of the conductor.
type Server struct {
	h Handlers

	pingEvery, pongWait time.Duration
	// newTimer starts a timer of the purge wait (tests inject a fake clock).
	newTimer func(d time.Duration) (<-chan time.Time, func() bool)
	accepted atomic.Int64 // links taken over, for tests

	mu  sync.Mutex
	cur *conn // the current link; nil when none
}

// conn is one authenticated link.
type conn struct {
	ws     *websocket.Conn
	ctx    context.Context // cancelled when the link ends
	cancel context.CancelFunc
	done   chan struct{} // closed when the link ends

	lastPong atomic.Int64 // unix nanos of the last pong (or the accept)

	// fwd orders forwarding against the link's end: a frame forwarded before
	// the end reaches the sessions before their OnConductorLost.
	fwd   sync.Mutex
	ended bool // guarded by fwd

	// Guarded by Server.mu.
	opened map[string]bool   // sessions whose host.open went out on this link, until host.state closed
	purges map[string]*purge // tree.purge waits by root
}

// purge is one tree.purge in flight; every concurrent PurgeTree of the root
// waits on it.
type purge struct {
	acked   chan struct{}
	waiters int
}

// NewServer builds the link server. Routes serves it.
func NewServer(h Handlers) *Server {
	return &Server{
		h:         h,
		pingEvery: pingInterval,
		pongWait:  pongTimeout,
		newTimer: func(d time.Duration) (<-chan time.Time, func() bool) {
			t := time.NewTimer(d)
			return t.C, t.Stop
		},
	}
}

// Routes registers GET /internal/v1/conductor behind Handlers.Auth.
func (s *Server) Routes(mux *http.ServeMux) {
	var h http.Handler = http.HandlerFunc(s.serveLink)
	if s.h.Auth != nil {
		h = s.h.Auth(h)
	}
	mux.Handle("GET "+Path, h)
}

// Connected reports whether a conductor link is up.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil
}

// OpenHosts is the number of sessions with a host open on the current link.
func (s *Server) OpenHosts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return 0
	}
	return len(s.cur.opened)
}

// RunningTurns lists, sorted, the sessions open on the current link whose
// turn is running.
func (s *Server) RunningTurns() []string {
	s.mu.Lock()
	var ids []string
	if s.cur != nil {
		for id := range s.cur.opened {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	running := ids[:0]
	for _, id := range ids {
		if _, ok := s.h.Sessions.RunningTurn(id); ok {
			running = append(running, id)
		}
	}
	slices.Sort(running)
	return running
}

// ---- Core -> conductor (sessions.Conductor) ----

// Open sends host.open; the session is open on this link from now until the
// conductor reports its host closed.
func (s *Server) Open(ctx context.Context, spec protocol.HostSpec) error {
	s.mu.Lock()
	c := s.cur
	if c == nil {
		s.mu.Unlock()
		return ErrNotConnected
	}
	c.opened[spec.SessionID] = true
	s.h.Metrics.OpenHosts.Set(float64(len(c.opened)))
	s.mu.Unlock()
	return s.write(ctx, c, protocol.HostOpen{SessionID: spec.SessionID, Spec: spec})
}

// SendTurn sends turn.send.
func (s *Server) SendTurn(ctx context.Context, t protocol.TurnSend) error {
	return s.send(ctx, t)
}

// Interrupt sends turn.interrupt.
func (s *Server) Interrupt(ctx context.Context, sessionID string) error {
	return s.send(ctx, protocol.TurnInterrupt{SessionID: sessionID})
}

// Close sends host.close with any C2 reason ("error" included, S48). The
// session stays open on the link until the conductor's host.state closed.
func (s *Server) Close(ctx context.Context, sessionID, reason string) error {
	return s.send(ctx, protocol.HostClose{SessionID: sessionID, Reason: reason})
}

// Shutdown sends shutdown{reason}: the conductor closes every host. With no
// link there is nothing to close.
func (s *Server) Shutdown(ctx context.Context, reason string) error {
	err := s.send(ctx, protocol.Shutdown{Reason: reason})
	if errors.Is(err, ErrNotConnected) {
		return nil
	}
	return err
}

// PurgeTree sends tree.purge{root, ref} and waits for the conductor's
// tree.purged for that root on the same link (S39). It returns ErrLinkLost
// if that link is lost or superseded first, ErrPurgeTimeout after 30 s, and
// nil at once when no link is up: the conductor's hosts closed with the
// link. Concurrent purges of one root share one tree.purge and its ack.
func (s *Server) PurgeTree(ctx context.Context, rootID, personRef string) error {
	s.mu.Lock()
	c := s.cur
	if c == nil {
		s.mu.Unlock()
		return nil
	}
	p := c.purges[rootID]
	first := p == nil
	if first {
		p = &purge{acked: make(chan struct{})}
		c.purges[rootID] = p
	}
	p.waiters++
	s.mu.Unlock()
	defer s.leavePurge(c, rootID, p)

	if first {
		if err := s.write(ctx, c, protocol.TreePurge{RootID: rootID, PersonRef: personRef}); err != nil {
			return err
		}
	}
	timeout, stop := s.newTimer(purgeTimeout)
	defer stop()
	select {
	case <-p.acked:
		return nil
	case <-c.done:
		return ErrLinkLost
	case <-timeout:
		return ErrPurgeTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// leavePurge drops a waiter; the last one forgets an unacked purge, so a late
// ack is ignored and the next purge of the root sends its own tree.purge.
func (s *Server) leavePurge(c *conn, rootID string, p *purge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.waiters--
	if p.waiters == 0 && c.purges[rootID] == p {
		delete(c.purges, rootID)
	}
}

func (s *Server) send(ctx context.Context, f protocol.LinkFrame) error {
	s.mu.Lock()
	c := s.cur
	s.mu.Unlock()
	if c == nil {
		return ErrNotConnected
	}
	return s.write(ctx, c, f)
}

func (s *Server) write(ctx context.Context, c *conn, f protocol.LinkFrame) error {
	b, err := protocol.EncodeLinkFrame(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("link: write %s: %w", f.FrameType(), err)
	}
	return nil
}

// ---- the link ----

func (s *Server) serveLink(w http.ResponseWriter, r *http.Request) {
	p, id, ok := internalauth.PrincipalFrom(r.Context())
	if !ok || p != internalauth.PrincipalConductor {
		slog.Warn("link: refused a caller that is not the conductor", "principal", string(p))
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		slog.Warn("link: handshake failed", "err", err)
		return
	}
	ws.SetReadLimit(readLimit)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	c := &conn{
		ws: ws, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		opened: map[string]bool{}, purges: map[string]*purge{},
	}
	c.lastPong.Store(time.Now().UnixNano())
	s.takeOver(c)
	slog.Info("link: conductor connected", "pod", id.PodName)

	go s.keepAlive(c)
	err = s.readLoop(c)
	slog.Info("link: conductor link ended", "pod", id.PodName, "close_status", int(websocket.CloseStatus(err)))
	s.end(c, websocket.StatusNormalClosure, "")
}

// takeOver makes c the current link; the links it replaces are superseded
// (4000) and their sessions lost (P30).
func (s *Server) takeOver(c *conn) {
	for {
		s.mu.Lock()
		old := s.cur
		if old == nil {
			s.cur = c
			s.mu.Unlock()
			break
		}
		s.mu.Unlock()
		slog.Info("link: a newer conductor link supersedes the current one")
		s.end(old, closeSuperseded, "superseded")
	}
	s.accepted.Add(1)
	s.h.Metrics.ConductorConnected.Set(1)
	s.h.Metrics.OpenHosts.Set(0)
}

// end finishes a link once: it stops forwarding, releases its purges, closes
// the socket with code and tells the sessions their hosts are gone.
func (s *Server) end(c *conn, code websocket.StatusCode, reason string) {
	c.fwd.Lock()
	if c.ended {
		c.fwd.Unlock()
		return
	}
	c.ended = true
	s.mu.Lock()
	current := s.cur == c
	if current {
		s.cur = nil
	}
	lost := make([]string, 0, len(c.opened))
	for id := range c.opened {
		lost = append(lost, id)
	}
	clear(c.opened)
	s.mu.Unlock()
	c.fwd.Unlock()

	close(c.done)
	if current {
		s.h.Metrics.ConductorConnected.Set(0)
		s.h.Metrics.OpenHosts.Set(0)
	}
	go func() {
		_ = c.ws.Close(code, reason)
		c.cancel()
	}()
	if len(lost) > 0 {
		slices.Sort(lost)
		s.h.Sessions.OnConductorLost(lost)
	}
}

// keepAlive pings every pingEvery; a link with no pong for pongWait is lost.
func (s *Server) keepAlive(c *conn) {
	t := time.NewTicker(s.pingEvery)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		if since := time.Since(time.Unix(0, c.lastPong.Load())); since >= s.pongWait {
			slog.Warn("link: no pong from the conductor; the link is lost", "since_pong", since.String())
			s.end(c, websocket.StatusGoingAway, "no pong")
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(c.ctx, s.pongWait)
			defer cancel()
			if c.ws.Ping(ctx) == nil {
				c.lastPong.Store(time.Now().UnixNano())
			}
		}()
	}
}

// readLoop reads and dispatches frames, one at a time, until the link ends.
func (s *Server) readLoop(c *conn) error {
	for {
		typ, b, err := c.ws.Read(c.ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			s.reject(rejectMalformed, "binary frame")
			continue
		}
		f, err := protocol.DecodeLinkFrame(b)
		if err != nil {
			s.reject(rejectMalformed, "undecodable frame", "err", err)
			continue
		}
		s.dispatch(c, f)
	}
}

func (s *Server) dispatch(c *conn, f protocol.LinkFrame) {
	c.fwd.Lock()
	defer c.fwd.Unlock()
	if c.ended {
		return
	}
	switch f := f.(type) {
	case protocol.Hello:
		slog.Info("link: hello", "conductor_version", f.ConductorVersion, "sdk_version", f.SDKVersion,
			"cli_version", f.CLIVersion, "max_hosts", f.MaxHosts)
	case protocol.HostState:
		s.mu.Lock()
		open := c.opened[f.SessionID]
		if open && f.State == "closed" {
			delete(c.opened, f.SessionID)
			s.h.Metrics.OpenHosts.Set(float64(len(c.opened)))
		}
		s.mu.Unlock()
		if !open {
			s.reject(rejectUnopened, "host.state", "session", f.SessionID)
			return
		}
		s.h.Sessions.OnHostState(f)
	case protocol.EventFrame:
		if s.bound(c, "event", f.SessionID, f.TurnID) {
			s.h.Sessions.OnLinkEvent(f.SessionID, f.TurnID, f.Event)
		}
	case protocol.RPC:
		if !s.bound(c, "rpc", f.SessionID, f.TurnID) {
			go s.answer(c, protocol.RPCResult{RPCID: f.RPCID, Error: upstreamError()})
			return
		}
		go s.serveRPC(c, f)
	case protocol.TreePurged:
		s.mu.Lock()
		p := c.purges[f.RootID]
		if p != nil {
			delete(c.purges, f.RootID)
			close(p.acked)
		}
		s.mu.Unlock()
		if p == nil {
			slog.Debug("link: tree.purged with no purge waiting", "root", f.RootID)
		}
	default:
		s.reject(rejectMalformed, "frame sent the wrong way", "frame", f.FrameType())
	}
}

// bound reports whether an event or rpc belongs to a session opened on this
// link and to its running turn; anything else is counted and dropped.
func (s *Server) bound(c *conn, what, sessionID, turnID string) bool {
	s.mu.Lock()
	open := c.opened[sessionID]
	s.mu.Unlock()
	if !open {
		s.reject(rejectUnopened, what, "session", sessionID, "turn", turnID)
		return false
	}
	if running, ok := s.h.Sessions.RunningTurn(sessionID); !ok || running != turnID {
		s.reject(rejectStaleTurn, what, "session", sessionID, "turn", turnID)
		return false
	}
	return true
}

func (s *Server) reject(reason, what string, kv ...any) {
	s.h.Metrics.LinkRejectedTotal.WithLabelValues(
		metrics.Label(reason, rejectUnopened, rejectStaleTurn, rejectMalformed)).Inc()
	level := slog.LevelWarn
	if reason == rejectStaleTurn {
		level = slog.LevelDebug // events racing a turn's end are expected
	}
	slog.Log(context.Background(), level, "link: frame rejected", append([]any{"reason", reason, "what", what}, kv...)...)
}

// serveRPC runs a memory rpc and answers it on the link it came on. Anything
// but a result is upstream_error; the error is logged with ids only.
func (s *Server) serveRPC(c *conn, f protocol.RPC) {
	res := protocol.RPCResult{RPCID: f.RPCID, OK: true, Result: &protocol.MemoryResult{Status: "disabled"}}
	if s.h.RPC != nil {
		params, err := json.Marshal(f.Params)
		var r protocol.MemoryResult
		if err == nil {
			r, err = s.h.RPC(c.ctx, f.SessionID, f.TurnID, f.Method, params)
		}
		if err != nil {
			slog.Error("link: memory rpc failed", "session", f.SessionID, "turn", f.TurnID, "method", f.Method, "err", err)
			res = protocol.RPCResult{RPCID: f.RPCID, Error: upstreamError()}
		} else {
			res.Result = &r
		}
	}
	s.answer(c, res)
}

func (s *Server) answer(c *conn, res protocol.RPCResult) {
	if err := s.write(c.ctx, c, res); err != nil {
		slog.Warn("link: rpc.result not delivered", "rpc", res.RPCID, "err", err)
	}
}

func upstreamError() *protocol.ErrorInfo {
	return &protocol.ErrorInfo{Code: protocol.CodeUpstreamError, Message: protocol.ErrorCopy[protocol.CodeUpstreamError]}
}

// ---- memory rpc routing ----

// SessionRows reads a session row; *store.Store implements it.
type SessionRows interface {
	Session(ctx context.Context, id string) (store.Session, error)
}

// Memory is the memory service (Task 13) as the rpc routing calls it.
type Memory interface {
	Remember(ctx context.Context, sess store.Session, turnID, text string, flagged bool) (protocol.MemoryResult, error)
	Forget(ctx context.Context, sess store.Session, turnID, query string, flagged bool) (protocol.MemoryResult, error)
}

// MemoryRPC routes memory.remember and memory.forget to mem. The person is
// the session row's, never anything in params (which carry only text and
// untrusted). An unknown session (its row deleted), an incognito one, and one
// whose agent has no person memory answer `disabled`, and mem is not called.
func MemoryRPC(rows SessionRows, mem Memory) RPCHandler {
	return func(ctx context.Context, sessionID, turnID, method string, params json.RawMessage) (protocol.MemoryResult, error) {
		if method != "memory.remember" && method != "memory.forget" {
			return protocol.MemoryResult{}, fmt.Errorf("link: unknown rpc method %q", method)
		}
		var p protocol.RPCParams
		if err := json.Unmarshal(params, &p); err != nil {
			return protocol.MemoryResult{}, fmt.Errorf("link: rpc params: %w", err)
		}
		disabled := protocol.MemoryResult{Status: "disabled"}
		sess, err := rows.Session(ctx, sessionID)
		if errors.Is(err, store.ErrNotFound) {
			return disabled, nil
		}
		if err != nil {
			return protocol.MemoryResult{}, err
		}
		if sess.Incognito {
			return disabled, nil
		}
		var spec protocol.AgentSpec
		if err := json.Unmarshal(sess.AgentSnapshot, &spec); err != nil {
			return protocol.MemoryResult{}, fmt.Errorf("link: agent snapshot of %s: %w", sessionID, err)
		}
		if spec.Memory != "person" {
			return disabled, nil
		}
		if method == "memory.forget" {
			return mem.Forget(ctx, sess, turnID, p.Text, p.Untrusted)
		}
		return mem.Remember(ctx, sess, turnID, p.Text, p.Untrusted)
	}
}
