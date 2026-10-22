// Package memory is a person's memory (§12.2 phase 1): profile.md, at most
// 6000 UTF-8 bytes; the remember and forget tools the conductor calls over
// the link's memory rpc; the profile editor's API; and held writes. Memory
// is data, not instructions: the snapshot a session takes is fenced, every
// write records its provenance, and a write made while its session is
// tainted (P9), or that the conductor flags untrusted, waits for the
// person's decision. Incognito, and an agent without person memory, read
// and write nothing.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// The bounds of P8.
const (
	// MaxRememberChars bounds one remembered fact, in characters (runes),
	// as the conductor's tool schema does.
	MaxRememberChars = 300
	// MinForgetChars is the shortest forget query, after trimming.
	MinForgetChars = 3
	// MaxForgetApplied is the most lines a forget removes without the
	// person's review; one that matches more is held.
	MaxForgetApplied = 5
	// MaxPendingPerTurn bounds the writes one turn can leave waiting.
	MaxPendingPerTurn = 3
)

// emitWait bounds the wait for a memory.write event after its write has
// committed. The event is emitted even when the caller has gone (a closed
// link, a client that left).
const emitWait = 10 * time.Second

var (
	// ErrNotFound: no such pending write of the person's (404 not_found).
	ErrNotFound = errors.New("memory: not found")
	// ErrFull: accepting the write would take the profile past
	// MaxProfileBytes (409 memory_full); it stays pending.
	ErrFull = errors.New("memory: the profile is full")
	// ErrInvalid: a remember with no text, or more than MaxRememberChars.
	// The conductor's schema refuses those, so it is a broken rpc.
	ErrInvalid = errors.New("memory: invalid request")
)

// TaintSource is the session service's taint (P9): *sessions.Service. Both
// calls run through the session's actor, so Tainted reflects every event the
// link delivered before the rpc, and Taint is the one taint writer (S47).
type TaintSource interface {
	Tainted(ctx context.Context, sessionID string) (bool, error)
	Taint(ctx context.Context, sessionID string) error
}

// Memory is the memory service.
type Memory struct {
	st      *store.Store
	sink    sessions.EventSink
	taint   TaintSource
	metrics *metrics.Core // nil counts nothing
}

// Option configures a Memory.
type Option func(*Memory)

// WithMetrics counts every write's outcome in c's
// luma_memory_writes_total{op,status} (C7): a remember or forget as its
// result's status (applied, pending, full, rejected, disabled) or error; a
// person's decision as accepted or discarded, or full for an accept that no
// longer fits.
func WithMetrics(c *metrics.Core) Option {
	return func(m *Memory) { m.metrics = c }
}

// New returns the memory service. memory.write events go through sink, the
// session service's EventSink, so they are serialised with the turn's own.
func New(st *store.Store, sink sessions.EventSink, taint TaintSource, opts ...Option) *Memory {
	m := &Memory{st: st, sink: sink, taint: taint}
	for _, o := range opts {
		o(m)
	}
	return m
}

// writeStatuses is the fixed set of luma_memory_writes_total's status label.
var writeStatuses = []string{"applied", "pending", "full", "rejected", "disabled", "error", "accepted", "discarded"}

// count records one write outcome.
func (m *Memory) count(op, status string) {
	if m.metrics == nil {
		return
	}
	m.metrics.MemoryWritesTotal.WithLabelValues(metrics.Label(op, "remember", "forget"),
		metrics.Label(status, writeStatuses...)).Inc()
}

// counted records a remember or forget's outcome: its result's status, or
// error.
func (m *Memory) counted(op string, res protocol.MemoryResult, err error) (protocol.MemoryResult, error) {
	status := res.Status
	if err != nil {
		status = "error"
	}
	m.count(op, status)
	return res, err
}

// Snapshot returns the person's profile, fenced; "" for an empty profile
// (S13). It implements sessions.ProfileSource: a session stores it verbatim
// as profile_snapshot at create, and never calls it for incognito or an
// agent without person memory.
func (m *Memory) Snapshot(ctx context.Context, personID string) (string, error) {
	p, err := m.st.Profile(ctx, personID)
	if err != nil {
		return "", err
	}
	return Fence(p.Content), nil
}

var (
	disabled    = protocol.MemoryResult{Status: "disabled", Code: protocol.CodeMemoryDisabled}
	full        = protocol.MemoryResult{Status: "full", Code: protocol.CodeMemoryFull}
	pendingFull = protocol.MemoryResult{Status: "rejected", Code: protocol.CodeMemoryPendingLimit}
	tooShort    = protocol.MemoryResult{Status: "rejected", Code: protocol.CodeForgetTooShort}
)

// Remember appends text to the session's person's profile as one bullet.
// It is held (pending) when the session is tainted or flagged is set
// (flagged also taints the session, S47), and is `full` when the bullet
// would take the profile past MaxProfileBytes. A `full` write and a held
// write past the turn's MaxPendingPerTurn leave no row; every other write
// is recorded with its provenance and emitted as memory.write. The row's
// text is text as the conductor sent it; the bullet is text on one line.
func (m *Memory) Remember(ctx context.Context, sess store.Session, turnID, text string, flagged bool) (protocol.MemoryResult, error) {
	res, err := m.remember(ctx, sess, turnID, text, flagged)
	return m.counted("remember", res, err)
}

func (m *Memory) remember(ctx context.Context, sess store.Session, turnID, text string, flagged bool) (protocol.MemoryResult, error) {
	off, err := memoryOff(sess)
	if err != nil {
		return protocol.MemoryResult{}, err
	}
	if off {
		return disabled, nil
	}
	untrusted, err := m.untrusted(ctx, sess, flagged)
	if err != nil {
		return protocol.MemoryResult{}, err
	}
	fact, ok := oneLine(text)
	if !ok {
		return protocol.MemoryResult{}, fmt.Errorf("%w: a remember needs 1 to %d characters", ErrInvalid, MaxRememberChars)
	}
	var (
		res protocol.MemoryResult
		row store.MemoryWrite
	)
	err = m.st.InMemoryTx(ctx, sess.PersonID, func(tx *store.MemoryTx) error {
		p, err := tx.Profile(ctx)
		if err != nil {
			return err
		}
		next := appendBullet(p.Content, fact)
		if len(next) > protocol.MaxProfileBytes {
			res = full
			return nil
		}
		w := store.MemoryWrite{SessionID: &sess.ID, TurnID: turnID, Op: "remember", Text: text, Untrusted: untrusted}
		if res, err = hold(ctx, tx, untrusted, &w); err != nil || res.Status == "rejected" {
			return err
		}
		if w.Status == "applied" {
			if _, err := tx.SetProfile(ctx, next, p.Version); err != nil {
				return err
			}
		}
		row, err = tx.InsertWrite(ctx, w)
		res.WriteID = row.ID
		return err
	})
	if err != nil {
		return protocol.MemoryResult{}, err
	}
	if row.ID != "" {
		m.emit(ctx, row)
	}
	return res, nil
}

// Forget removes the bullet lines of the session's person's profile whose
// fact contains query (trimmed, case-insensitive). A query under
// MinForgetChars is rejected. It is held (pending, with the lines it
// matches now) when the session is tainted, flagged is set, or it matches
// more than MaxForgetApplied lines; a held one past the turn's limit is
// rejected. Rejected forgets leave no row; the rest are recorded and
// emitted as Remember's are.
func (m *Memory) Forget(ctx context.Context, sess store.Session, turnID, query string, flagged bool) (protocol.MemoryResult, error) {
	res, err := m.forget(ctx, sess, turnID, query, flagged)
	return m.counted("forget", res, err)
}

func (m *Memory) forget(ctx context.Context, sess store.Session, turnID, query string, flagged bool) (protocol.MemoryResult, error) {
	off, err := memoryOff(sess)
	if err != nil {
		return protocol.MemoryResult{}, err
	}
	if off {
		return disabled, nil
	}
	untrusted, err := m.untrusted(ctx, sess, flagged)
	if err != nil {
		return protocol.MemoryResult{}, err
	}
	q := strings.TrimSpace(query)
	if utf8.RuneCountInString(q) < MinForgetChars {
		return tooShort, nil
	}
	var (
		res protocol.MemoryResult
		row store.MemoryWrite
	)
	err = m.st.InMemoryTx(ctx, sess.PersonID, func(tx *store.MemoryTx) error {
		p, err := tx.Profile(ctx)
		if err != nil {
			return err
		}
		kept, matched := forgetLines(p.Content, q)
		w := store.MemoryWrite{SessionID: &sess.ID, TurnID: turnID, Op: "forget", Text: query,
			Matched: matched, Untrusted: untrusted}
		if res, err = hold(ctx, tx, untrusted || len(matched) > MaxForgetApplied, &w); err != nil || res.Status == "rejected" {
			return err
		}
		if w.Status == "applied" {
			n := len(matched)
			res.Removed = &n
			if len(matched) > 0 {
				if _, err := tx.SetProfile(ctx, kept, p.Version); err != nil {
					return err
				}
			}
		}
		row, err = tx.InsertWrite(ctx, w)
		res.WriteID = row.ID
		return err
	})
	if err != nil {
		return protocol.MemoryResult{}, err
	}
	if row.ID != "" {
		m.emit(ctx, row)
	}
	return res, nil
}

// hold settles whether w applies now or waits for the person: a held write
// is pending if its turn has held fewer than MaxPendingPerTurn, decided or
// not (S58: deciding one does not make room), and otherwise rejected (no
// row). It gives w its id and status, and returns the result so far.
func hold(ctx context.Context, tx *store.MemoryTx, held bool, w *store.MemoryWrite) (protocol.MemoryResult, error) {
	if !held {
		w.ID, w.Status = newID("mem_", time.Now()), "applied"
		return protocol.MemoryResult{Status: "applied"}, nil
	}
	n, err := tx.HeldInTurn(ctx, *w.SessionID, w.TurnID)
	if err != nil {
		return protocol.MemoryResult{}, err
	}
	if n >= MaxPendingPerTurn {
		return pendingFull, nil
	}
	w.ID, w.Status = newID("mem_", time.Now()), "pending"
	return protocol.MemoryResult{Status: "pending"}, nil
}

// Decide settles one of the person's pending writes: accept applies it to
// the profile as it is now, and reject leaves the profile alone. An accepted
// forget removes only its held `matched` lines that are still in the
// profile, what the person saw, never a fresh match (S59); matched stays as
// held. Another person's write, or one already decided the other way, is
// ErrNotFound; the same decision again returns the write unchanged (a
// retried click). An accept that would take the profile past the cap is
// ErrFull, and the write stays pending.
func (m *Memory) Decide(ctx context.Context, p auth.Person, writeID string, accept bool) (protocol.MemoryWrite, error) {
	want, outcome := "rejected", "discarded"
	if accept {
		want, outcome = "applied", "accepted"
	}
	var (
		out     store.MemoryWrite
		decided bool
	)
	err := m.st.InMemoryTx(ctx, p.ID, func(tx *store.MemoryTx) error {
		w, err := tx.Write(ctx, writeID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if w.Status != "pending" {
			if w.Status != want {
				return ErrNotFound
			}
			out = w
			return nil
		}
		if accept {
			err := apply(ctx, tx, w)
			if errors.Is(err, ErrFull) {
				m.count(w.Op, "full")
			}
			if err != nil {
				return err
			}
		}
		out, err = tx.DecideWrite(ctx, w.ID, want)
		decided = err == nil
		return err
	})
	if err != nil {
		return protocol.MemoryWrite{}, err
	}
	if decided {
		m.count(out.Op, outcome)
		m.emit(ctx, out)
	}
	return out.Protocol(), nil
}

// apply changes the profile as accepting w does.
func apply(ctx context.Context, tx *store.MemoryTx, w store.MemoryWrite) error {
	p, err := tx.Profile(ctx)
	if err != nil {
		return err
	}
	var next string
	switch w.Op {
	case "remember":
		fact, ok := oneLine(w.Text)
		if !ok {
			return fmt.Errorf("memory: stored write %s has no valid fact", w.ID)
		}
		if next = appendBullet(p.Content, fact); len(next) > protocol.MaxProfileBytes {
			return ErrFull
		}
	case "forget":
		next = removeLines(p.Content, w.Matched)
	default:
		return fmt.Errorf("memory: stored write %s has op %q", w.ID, w.Op)
	}
	if next != p.Content {
		if _, err := tx.SetProfile(ctx, next, p.Version); err != nil {
			return err
		}
	}
	return nil
}

// untrusted reports whether a write in sess is held for taint: the
// conductor flagged it (which taints the session first, S47; failing that
// fails the write, closed) or the session is tainted.
func (m *Memory) untrusted(ctx context.Context, sess store.Session, flagged bool) (bool, error) {
	if flagged {
		if err := m.taint.Taint(ctx, sess.ID); err != nil {
			return false, fmt.Errorf("memory: tainting %s: %w", sess.ID, err)
		}
		return true, nil
	}
	if sess.TaintedAt != nil {
		return true, nil
	}
	tainted, err := m.taint.Tainted(ctx, sess.ID)
	if err != nil {
		return false, fmt.Errorf("memory: taint of %s: %w", sess.ID, err)
	}
	return tainted, nil
}

// emit appends the write's memory.write to its session's log through the
// sink. The write has committed, so a failure is logged, not returned: the
// row is the record, and the client sees it in /tree and the profile.
//
// Emitting after the commit, outside the person's lock, lets two events of
// one write land out of order: a decision made within milliseconds of the
// held write can emit `applied` before `pending`. That is accepted: the
// row stays the truth, and the PWA's reducer keys each write by id and
// lets a decided status win over `pending` (Tasks 21/24).
func (m *Memory) emit(ctx context.Context, w store.MemoryWrite) {
	slog.Info("memory: write", "write", w.ID, "op", w.Op, "status", w.Status, "untrusted", w.Untrusted)
	if w.SessionID == nil { // its conversation was deleted
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), emitWait)
	defer cancel()
	if _, err := m.sink.Emit(ctx, *w.SessionID, "memory.write", protocol.MemoryWriteEvent{Write: w.Protocol()}); err != nil {
		slog.Warn("memory: memory.write not emitted", "session", *w.SessionID, "write", w.ID, "err", err)
	}
}

// memoryOff reports whether the session reads and writes no memory:
// incognito, or an agent whose memory is not `person`.
func memoryOff(sess store.Session) (bool, error) {
	if sess.Incognito {
		return true, nil
	}
	var spec struct {
		Memory string `json:"memory"`
	}
	if err := json.Unmarshal(sess.AgentSnapshot, &spec); err != nil {
		return false, fmt.Errorf("memory: agent snapshot of %s: %w", sess.ID, err)
	}
	return spec.Memory != "person", nil
}

// ---- profile.md ----

// oneLine is a remembered fact as its bullet holds it: every whitespace run
// (newlines included) one space, trimmed. text must have 1 to
// MaxRememberChars characters.
func oneLine(text string) (string, bool) {
	if utf8.RuneCountInString(text) > MaxRememberChars {
		return "", false
	}
	fact := strings.Join(strings.Fields(text), " ")
	return fact, fact != ""
}

// appendBullet adds "- fact" as the profile's last line.
func appendBullet(content, fact string) string {
	switch {
	case content == "":
		return "- " + fact
	case strings.HasSuffix(content, "\n"):
		return content + "- " + fact
	default:
		return content + "\n- " + fact
	}
}

// forgetLines removes the bullet lines whose fact contains query, ignoring
// case, and returns what is kept (every other line byte for byte) and the
// removed lines (never nil).
func forgetLines(content, query string) (kept string, matched []string) {
	q := strings.ToLower(query)
	matched = []string{}
	var b strings.Builder
	for _, line := range strings.SplitAfter(content, "\n") {
		text := strings.TrimRight(line, "\r\n")
		if fact, ok := bulletFact(text); ok && strings.Contains(strings.ToLower(fact), q) {
			matched = append(matched, text)
			continue
		}
		b.WriteString(line)
	}
	return b.String(), matched
}

// bulletFact returns the fact of a Markdown bullet line ("- ", "* " or "+ ",
// indented or not).
func bulletFact(line string) (string, bool) {
	t := strings.TrimLeft(line, " \t")
	if len(t) < 2 || !strings.ContainsRune("-*+", rune(t[0])) || (t[1] != ' ' && t[1] != '\t') {
		return "", false
	}
	return t[2:], true
}

// removeLines removes, top to bottom, one profile line for each of lines
// (compared without its line ending): the held lines of an accepted forget
// that are still there (S59). Every other line is kept byte for byte.
func removeLines(content string, lines []string) string {
	left := make(map[string]int, len(lines))
	for _, l := range lines {
		left[l]++
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(content, "\n") {
		if text := strings.TrimRight(line, "\r\n"); left[text] > 0 {
			left[text]--
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID returns prefix plus a ULID (48 bits of Unix milliseconds, 80 random
// bits, Crockford base32): the shape of every C4 id, as sessions makes them.
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
