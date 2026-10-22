// Package sessions is Core's session service: one actor per session owns its
// turns and is the single writer of its event log (P23, P24), taint (P9), the
// pinned model and the resume state (S2). The HTTP API and the WebSocket
// stream (Task 9b) and fork (Task 11) sit on top of it.
package sessions

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// Conductor is the conductor link (Task 10).
type Conductor interface {
	Open(ctx context.Context, spec protocol.HostSpec) error
	SendTurn(ctx context.Context, t protocol.TurnSend) error
	Interrupt(ctx context.Context, sessionID string) error
	Close(ctx context.Context, sessionID, reason string) error
	// PurgeTree returns after the conductor's tree.purged ack, link-down or
	// 30 s (S39, Task 10).
	PurgeTree(ctx context.Context, rootID, personRef string) error
	Connected() bool
}

// Gate refuses new work: maintenance (Task 10) and the budget (Task 14). It
// is checked on Create and on Send (S1).
type Gate interface {
	CheckNewTurn(ctx context.Context) *protocol.ErrorInfo
}

// ProfileSource returns a person's memory profile, already fenced (S13);
// Task 13 implements it.
type ProfileSource interface {
	Snapshot(ctx context.Context, personID string) (string, error)
}

// AttachmentResolver checks that the attachments are the person's, binds them
// to the tree, applies the caps and returns their content blocks (Task 12).
type AttachmentResolver interface {
	Bind(ctx context.Context, p auth.Person, rootID string, ids []string, tier string) ([]protocol.ContentBlock, error)
}

// EventSink appends an event to a session's log through the session's actor,
// so it is serialised with the turn's own events. payload must be the
// protocol.EventBody of type typ.
type EventSink interface {
	Emit(ctx context.Context, sessionID, typ string, payload any) (int64, error)
}

// Ledger records spend (S3); Task 14's *ledger.Ledger implements it.
type Ledger interface {
	RecordTurn(ctx context.Context, sess store.Session, turnID string, u protocol.TurnUsage, costBasis *string) error
	RecordMainRequest(sess store.Session, u protocol.MainRequestUsage)
}

// NopLedger records nothing; it stands in until Task 14.
type NopLedger struct{}

// RecordTurn records nothing.
func (NopLedger) RecordTurn(context.Context, store.Session, string, protocol.TurnUsage, *string) error {
	return nil
}

// RecordMainRequest records nothing.
func (NopLedger) RecordMainRequest(store.Session, protocol.MainRequestUsage) {}

// Config is the site configuration the service needs. The helper model is
// not here: it comes from Create's catalog snapshot (S11).
type Config struct {
	Location            *time.Location // LUMA_TIMEZONE; nil is UTC
	WebFetchDenyDomains []string
	TurnMax             time.Duration // LUMA_TURN_MAX_MINUTES: a turn's wall-clock cap from turn.send; zero is 15 minutes (S48)
}

// The turn backstops (S48).
const (
	defaultTurnMax = 15 * time.Minute  // a turn's wall-clock cap, unless Config.TurnMax says otherwise
	interruptGrace = 30 * time.Second  // after the cap's interrupt, the wait for its turn_result
	errorGrace     = 120 * time.Second // after a link error, the wait for its turn_result
)

var (
	// ErrNotFound: the session, or the agent, does not exist or is not the
	// person's (404).
	ErrNotFound = errors.New("sessions: not found")
	// ErrBusy: the session already runs a turn (409 busy).
	ErrBusy = errors.New("sessions: a turn is already running")
	// ErrInvalid: the request is malformed or asks for something the person
	// may not have (400). It is wrapped with the reason.
	ErrInvalid = errors.New("sessions: invalid request")
)

// RefusedError is a Gate refusal: maintenance (503) or budget_exhausted (402).
type RefusedError struct {
	Info protocol.ErrorInfo
}

func (e *RefusedError) Error() string { return "sessions: refused: " + string(e.Info.Code) }

var validEfforts = []string{"low", "medium", "high"}

var clientIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Service owns every session's turns.
type Service struct {
	st    *store.Store
	live  *agents.Live
	cond  Conductor
	prof  ProfileSource
	att   AttachmentResolver
	gate  Gate
	led   Ledger
	m     *metrics.Core
	clock func() time.Time
	cfg   Config

	// appendEvent is st.AppendEvent: every event of a session is written
	// through it, by that session's actor (a test counts the writers).
	appendEvent func(ctx context.Context, sessionID, typ string, payload any, d store.Durability) (int64, error)
	// afterFunc runs f once d has passed and returns its stop function: the
	// turn backstops' timers (time.AfterFunc; tests inject a fake clock).
	afterFunc func(d time.Duration, f func()) (stop func() bool)

	hub hub
	// streams indexes the open WebSockets by web session and person, so a
	// session end closes them (Task 9b).
	streams streams
	// deletions are the roots whose tree is being deleted: Send and Fork on
	// any of their sessions answer 404 (S39, Task 9b).
	deletions deletions

	mu      sync.Mutex
	actors  map[string]*actor // live actors by session id
	running map[string]string // session id -> running turn id
}

// NewService builds the service. Call SweepOnStart before serving.
func NewService(st *store.Store, live *agents.Live, c Conductor, prof ProfileSource, att AttachmentResolver,
	gate Gate, led Ledger, m *metrics.Core, clock func() time.Time, cfg Config) *Service {
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.TurnMax <= 0 {
		cfg.TurnMax = defaultTurnMax
	}
	cfg.WebFetchDenyDomains = append([]string{}, cfg.WebFetchDenyDomains...)
	return &Service{
		st: st, live: live, cond: c, prof: prof, att: att, gate: gate, led: led, m: m, clock: clock, cfg: cfg,
		appendEvent: st.AppendEvent,
		afterFunc:   func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop },
		hub:         hub{subs: map[string]map[chan protocol.Event]struct{}{}},
		actors:      map[string]*actor{},
		running:     map[string]string{},
	}
}

// Create starts a conversation. The Gate runs first, and everything the
// session snapshots (agent, spec, helper model, bound models) comes from one
// catalog snapshot. The source is derived, never read: a Create here is the
// cookie path, so it is web, and its API workspace is interactive (S1, S5).
func (s *Service) Create(ctx context.Context, p auth.Person, req protocol.CreateSession) (protocol.Session, error) {
	if refusal := s.gate.CheckNewTurn(ctx); refusal != nil {
		return protocol.Session{}, &RefusedError{Info: *refusal}
	}
	cat := s.live.Load()
	a, ok := cat.Agent(req.Agent)
	if !ok || a.Kind != "agent" || !a.Admits(p.Tier) {
		return protocol.Session{}, ErrNotFound
	}
	tier := req.Params.Tier
	if tier == "" {
		tier = a.Tiers[0]
	}
	if !slices.Contains(a.Tiers, tier) {
		return protocol.Session{}, fmt.Errorf("%w: agent %s does not offer tier %q", ErrInvalid, a.Name, tier)
	}
	if tier == "deep" && p.Tier != "admin" {
		return protocol.Session{}, fmt.Errorf("%w: tier deep is for admins", ErrInvalid)
	}
	if w := req.Params.Workspace; w != "" && w != "none" {
		return protocol.Session{}, fmt.Errorf("%w: workspace %q is not available in phase 1 (only none)", ErrInvalid, w)
	}
	spec, err := cat.Compile(a.Name, tier)
	if err != nil {
		return protocol.Session{}, fmt.Errorf("sessions: compile %s for %s: %w", a.Name, tier, err)
	}
	effort := req.Params.Effort
	if effort == "" {
		effort = spec.TierBinding.Effort
	}
	if !slices.Contains(validEfforts, effort) {
		return protocol.Session{}, fmt.Errorf("%w: effort %q must be low, medium or high", ErrInvalid, effort)
	}
	incognito := req.Params.Incognito != nil && *req.Params.Incognito
	var profile string
	if !incognito && spec.Memory != "none" {
		if profile, err = s.prof.Snapshot(ctx, p.ID); err != nil {
			return protocol.Session{}, fmt.Errorf("sessions: profile snapshot: %w", err)
		}
	}
	helper := cat.Models().HelperModel
	snapshot, err := json.Marshal(spec)
	if err != nil {
		return protocol.Session{}, err
	}
	row, err := s.st.CreateSession(ctx, store.Session{
		ID: newID("ses_", s.clock()), PersonID: p.ID,
		AgentID: spec.ID, AgentVersion: spec.Version, AgentSnapshot: snapshot,
		Tier: tier, Model: spec.TierBinding.Model, HelperModel: helper, Effort: effort,
		CacheTTL: spec.TierBinding.CacheTTL, Workspace: "none", APIWorkspace: "interactive", Source: "web",
		Incognito: incognito, BoundModels: boundModels(spec, helper),
		ProfileSnapshot: profile, Status: "idle",
	})
	if err != nil {
		return protocol.Session{}, err
	}
	slog.Info("sessions: created", "session", row.ID, "agent", row.AgentID, "tier", row.Tier, "incognito", row.Incognito)
	return view(row), nil
}

// boundModels is the session's model, the helper, then the subagents' models
// (P29), each once.
func boundModels(spec protocol.AgentSpec, helper string) []string {
	out := []string{spec.TierBinding.Model}
	add := func(m string) {
		if m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	add(helper)
	names := make([]string, 0, len(spec.Subagents))
	for name := range spec.Subagents {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		add(spec.Subagents[name].Model)
	}
	return out
}

// Send handles a client event. A user.message starts a turn and returns the
// stored user message; a repeated client_id returns the message stored the
// first time and starts nothing. A user.interrupt returns no message.
func (s *Service) Send(ctx context.Context, p auth.Person, sessionID string, ev protocol.ClientEvent) (*protocol.Message, error) {
	switch e := ev.(type) {
	case protocol.UserMessage:
		return s.sendMessage(ctx, p, sessionID, e)
	case *protocol.UserMessage:
		if e != nil {
			return s.sendMessage(ctx, p, sessionID, *e)
		}
	case protocol.UserInterrupt, *protocol.UserInterrupt:
		return nil, s.call(ctx, sessionID, func(ctx context.Context, a *actor) error { return a.interrupt(ctx, p) })
	}
	return nil, fmt.Errorf("%w: unsupported client event %T", ErrInvalid, ev)
}

func (s *Service) sendMessage(ctx context.Context, p auth.Person, sessionID string, um protocol.UserMessage) (*protocol.Message, error) {
	if um.ClientID != "" && !clientIDRE.MatchString(um.ClientID) {
		return nil, fmt.Errorf("%w: client_id must be a UUID", ErrInvalid)
	}
	var text strings.Builder
	for _, b := range um.Content {
		text.WriteString(b.Text)
	}
	if strings.TrimSpace(text.String()) == "" && len(um.AttachmentIDs) == 0 {
		return nil, fmt.Errorf("%w: the message is empty", ErrInvalid)
	}
	var out protocol.Message
	if err := s.call(ctx, sessionID, func(ctx context.Context, a *actor) error {
		m, err := a.send(ctx, p, um)
		out = m
		return err
	}); err != nil {
		return nil, err
	}
	return &out, nil
}

// OnLinkEvent hands a conductor event of a session's turn to its actor. The
// link (Task 10) calls it in frame order; the actor drops events of a turn
// that is not running.
func (s *Service) OnLinkEvent(sessionID, turnID string, ev protocol.LinkEvent) {
	s.post(sessionID, func(ctx context.Context, a *actor) { a.onLink(ctx, turnID, ev) })
}

// OnHostState records a host's state: any sdk_session_id is persisted (S2),
// and a closed host fails a pending turn.
func (s *Service) OnHostState(hs protocol.HostState) {
	s.post(hs.SessionID, func(ctx context.Context, a *actor) { a.onHostState(ctx, hs) })
}

// OnConductorLost forgets the hosts of a lost link and fails their running
// turns with conductor_restarted.
func (s *Service) OnConductorLost(sessionIDs []string) {
	for _, id := range sessionIDs {
		s.post(id, func(ctx context.Context, a *actor) { a.onConductorLost(ctx) })
	}
}

// SweepOnStart settles every turn a previous Core left running: the session
// gets error{conductor_restarted}, its running message is marked error, and
// it goes idle (a turn whose message had already settled only goes idle). A
// session it cannot settle is logged and counted, and the sweep goes on; only
// failing to list the running sessions is an error.
func (s *Service) SweepOnStart(ctx context.Context) error {
	ids, err := s.st.RunningSessions(ctx)
	if err != nil {
		return err
	}
	failed := 0
	for _, id := range ids {
		if err := s.call(ctx, id, func(ctx context.Context, a *actor) error { return a.sweep(ctx) }); err != nil {
			failed++
			slog.Error("sessions: startup sweep could not settle a session", "session", id, "err", err)
		}
	}
	slog.Info("sessions: startup sweep", "running", len(ids), "unsettled", failed)
	return nil
}

// Sink is the EventSink other packages (memory, Task 13) write through.
func (s *Service) Sink() EventSink { return sink{s} }

// Tainted reports whether the session is tainted (P9). It is answered by the
// session's actor, so every event the link delivered before the call (a
// WebFetch tool.started, say) has been applied.
func (s *Service) Tainted(ctx context.Context, sessionID string) (bool, error) {
	var tainted bool
	if err := s.call(ctx, sessionID, func(ctx context.Context, a *actor) error {
		if err := a.reload(ctx); err != nil {
			return err
		}
		tainted = a.tainted()
		return nil
	}); err != nil {
		return false, err
	}
	return tainted, nil
}

// Taint marks the session tainted (P9) through its actor, the only taint
// writer: the memory rpc calls it when the conductor's untrusted flag is set
// (S47). If the write fails the session stays tainted in memory (S46) and the
// error is returned.
func (s *Service) Taint(ctx context.Context, sessionID string) error {
	return s.call(ctx, sessionID, func(ctx context.Context, a *actor) error {
		if err := a.load(ctx); err != nil {
			return err
		}
		return a.taint(ctx)
	})
}

// RunningTurn returns the session's running turn id. The link accepts events
// and rpcs only for it.
func (s *Service) RunningTurn(sessionID string) (turnID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turnID, ok = s.running[sessionID]
	return turnID, ok
}

func (s *Service) setRunning(sessionID, turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turnID == "" {
		delete(s.running, sessionID)
		return
	}
	s.running[sessionID] = turnID
}

// ---- actors and their mailboxes ----

type actorKey struct{}

// enqueue puts f in the session's mailbox, starting its actor if none runs.
// An actor stops only when its mailbox is empty and it has neither a host
// nor a turn, under s.mu, so f always reaches a running actor.
func (s *Service) enqueue(sessionID string, f func(*actor)) {
	s.mu.Lock()
	a := s.actors[sessionID]
	if a == nil {
		a = newActor(s, sessionID)
		s.actors[sessionID] = a
		go a.run()
	}
	a.pending++
	s.mu.Unlock()
	a.mbox <- f
}

// call runs fn on the session's actor and waits for it. The work is not
// cancelled with ctx: a client that goes away does not stop a send half way.
// A call made from inside the same actor's work (its context says so) runs
// inline instead of deadlocking.
func (s *Service) call(ctx context.Context, sessionID string, fn func(context.Context, *actor) error) error {
	if a, ok := ctx.Value(actorKey{}).(*actor); ok && a.id == sessionID {
		return fn(ctx, a)
	}
	done := make(chan error, 1)
	s.enqueue(sessionID, func(a *actor) { done <- fn(a.opContext(ctx), a) })
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// post runs fn on the session's actor without waiting.
func (s *Service) post(sessionID string, fn func(context.Context, *actor)) {
	s.enqueue(sessionID, func(a *actor) { fn(a.opContext(context.Background()), a) })
}

type sink struct{ s *Service }

func (k sink) Emit(ctx context.Context, sessionID, typ string, payload any) (int64, error) {
	body, ok := payload.(protocol.EventBody)
	if !ok {
		return 0, fmt.Errorf("sessions: event payload %T is not a protocol event body", payload)
	}
	if body.EventType() != typ {
		return 0, fmt.Errorf("sessions: event type %q does not match its body's %q", typ, body.EventType())
	}
	var id int64
	if err := k.s.call(ctx, sessionID, func(ctx context.Context, a *actor) error {
		if err := a.load(ctx); err != nil {
			return err
		}
		var err error
		id, err = a.emit(ctx, body, store.Durable)
		return err
	}); err != nil {
		return 0, err
	}
	return id, nil
}

// ---- fan-out ----

// hub fans a session's appended events out to its live subscribers. Publish
// never blocks an actor: a subscriber whose buffer is full is dropped (its
// channel closed) and catches up with since=<id> (Task 9b).
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan protocol.Event]struct{}
}

func (h *hub) subscribe(sessionID string, buf int) (<-chan protocol.Event, func()) {
	ch := make(chan protocol.Event, buf)
	h.mu.Lock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = map[chan protocol.Event]struct{}{}
	}
	h.subs[sessionID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.drop(sessionID, ch)
	}
}

// drop unregisters and closes ch if it is still registered; h.mu is held.
func (h *hub) drop(sessionID string, ch chan protocol.Event) {
	set := h.subs[sessionID]
	if _, ok := set[ch]; !ok {
		return
	}
	delete(set, ch)
	close(ch)
	if len(set) == 0 {
		delete(h.subs, sessionID)
	}
}

func (h *hub) publish(ev protocol.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[ev.SessionID] {
		select {
		case ch <- ev:
		default:
			h.drop(ev.SessionID, ch)
		}
	}
}

// ---- helpers ----

// view is the client's Session (C1): sdk_session_id, last_totals, the helper
// model and the snapshots stay server-side.
func view(s store.Session) protocol.Session {
	return protocol.Session{
		ID: s.ID, RootID: s.RootID, ParentID: s.ParentID, ForkAtMessageID: s.ForkAtMessageID,
		AgentID: s.AgentID, AgentVersion: s.AgentVersion, Title: s.Title, Tier: s.Tier, Model: s.Model,
		Effort: s.Effort, Incognito: s.Incognito, Workspace: s.Workspace, Source: s.Source, Status: s.Status,
		Tainted: s.TaintedAt != nil, ArchivedAt: s.ArchivedAt,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, LastActivityAt: s.LastActivityAt,
	}
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID returns prefix plus a ULID: 48 bits of Unix milliseconds and 80
// random bits in Crockford base32, the shape of every C4 id
// ("ses_01J9ZQ3M5K7N8P2R4T6V8W0XYA").
func newID(prefix string, t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	_, _ = rand.Read(b[6:]) // crypto/rand.Read never fails (Go 1.24+)
	hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return prefix + string(out[:])
}
