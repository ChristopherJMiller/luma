package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// querier is what the memory SQL runs on: the pool, or one transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Profile is a person's memory profile.
type Profile struct {
	PersonID  string
	Content   string
	Version   int
	UpdatedAt time.Time
}

// Profile returns a person's profile; a person with none yet gets an empty
// profile at version 0 (not an error).
func (s *Store) Profile(ctx context.Context, personID string) (Profile, error) {
	return profileOn(ctx, s.pool, personID)
}

func profileOn(ctx context.Context, db querier, personID string) (Profile, error) {
	p := Profile{PersonID: personID}
	err := db.QueryRow(ctx, `select content, version, updated_at from memory_profiles where person_id = $1`, personID).
		Scan(&p.Content, &p.Version, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	return p, err
}

// ErrProfileConflict means the profile changed since the caller read it.
var ErrProfileConflict = errors.New("store: profile version conflict")

// SetProfile replaces a profile if its version is still expectedVersion (0
// for a person with none), and returns the new version.
func (s *Store) SetProfile(ctx context.Context, personID, content string, expectedVersion int) (int, error) {
	p, err := setProfileOn(ctx, s.pool, personID, content, expectedVersion)
	return p.Version, err
}

func setProfileOn(ctx context.Context, db querier, personID, content string, expectedVersion int) (Profile, error) {
	p := Profile{PersonID: personID, Content: content}
	var err error
	if expectedVersion == 0 {
		err = db.QueryRow(ctx, `insert into memory_profiles (person_id, content, version) values ($1, $2, 1)
			on conflict (person_id) do nothing returning version, updated_at`, personID, content).
			Scan(&p.Version, &p.UpdatedAt)
	} else {
		err = db.QueryRow(ctx, `update memory_profiles set content = $2, version = version + 1, updated_at = now()
			where person_id = $1 and version = $3 returning version, updated_at`, personID, content, expectedVersion).
			Scan(&p.Version, &p.UpdatedAt)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrProfileConflict
	}
	return p, err
}

// MemoryWrite is a C4 `memory_writes` row. SessionID is nil once its
// conversation is deleted.
type MemoryWrite struct {
	ID        string
	PersonID  string
	SessionID *string
	TurnID    string
	Op        string // remember | forget
	Text      string
	Matched   []string
	Untrusted bool
	Status    string // applied | pending | rejected
	CreatedAt time.Time
	DecidedAt *time.Time
}

// Protocol is the client's MemoryWrite (C1): the row without person_id.
func (w MemoryWrite) Protocol() protocol.MemoryWrite {
	return protocol.MemoryWrite{
		ID: w.ID, SessionID: w.SessionID, TurnID: w.TurnID, Op: w.Op, Text: w.Text, Matched: w.Matched,
		Untrusted: w.Untrusted, Status: w.Status, CreatedAt: w.CreatedAt, DecidedAt: w.DecidedAt,
	}
}

const memoryWriteCols = `id, person_id, session_id, turn_id, op, text, matched, untrusted, status, created_at, decided_at`

func scanMemoryWrite(row pgx.Row) (MemoryWrite, error) {
	var w MemoryWrite
	err := row.Scan(&w.ID, &w.PersonID, &w.SessionID, &w.TurnID, &w.Op, &w.Text, &w.Matched,
		&w.Untrusted, &w.Status, &w.CreatedAt, &w.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryWrite{}, ErrNotFound
	}
	return w, err
}

// InsertMemoryWrite records a remember/forget request with its provenance. A
// non-pending write is stamped decided.
func (s *Store) InsertMemoryWrite(ctx context.Context, w MemoryWrite) (MemoryWrite, error) {
	return insertMemoryWriteOn(ctx, s.pool, w)
}

func insertMemoryWriteOn(ctx context.Context, db querier, w MemoryWrite) (MemoryWrite, error) {
	return scanMemoryWrite(db.QueryRow(ctx, `insert into memory_writes (id, person_id, session_id, turn_id,
		op, text, matched, untrusted, status, decided_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9, case when $9 = 'pending' then null else now() end)
		returning `+memoryWriteCols,
		w.ID, w.PersonID, w.SessionID, w.TurnID, w.Op, w.Text, w.Matched, w.Untrusted, w.Status))
}

// MemoryWrite returns one write.
func (s *Store) MemoryWrite(ctx context.Context, id string) (MemoryWrite, error) {
	return scanMemoryWrite(s.pool.QueryRow(ctx, `select `+memoryWriteCols+` from memory_writes where id = $1`, id))
}

// DecideMemoryWrite settles a pending write as applied or rejected. It
// returns ErrNotFound if the write is not pending.
func (s *Store) DecideMemoryWrite(ctx context.Context, id, status string) (MemoryWrite, error) {
	return decideMemoryWriteOn(ctx, s.pool, id, "", status, nil)
}

// decideMemoryWriteOn settles a pending write. A non-empty personID also
// requires the write to be that person's; a non-nil matched replaces the
// stored one (the lines an accepted forget removed).
func decideMemoryWriteOn(ctx context.Context, db querier, id, personID, status string, matched []string) (MemoryWrite, error) {
	return scanMemoryWrite(db.QueryRow(ctx, `update memory_writes
		set status = $2, decided_at = now(), matched = coalesce($4::text[], matched)
		where id = $1 and status = 'pending' and ($3 = '' or person_id = $3) returning `+memoryWriteCols,
		id, status, personID, matched))
}

// MemoryWrites lists a person's writes, newest first.
func (s *Store) MemoryWrites(ctx context.Context, personID string, limit int) ([]MemoryWrite, error) {
	rows, err := s.pool.Query(ctx, `select `+memoryWriteCols+` from memory_writes where person_id = $1
		order by created_at desc, id desc limit $2`, personID, limit)
	if err != nil {
		return nil, err
	}
	return scanMemoryWrites(rows)
}

// PendingMemoryWrites lists a person's pending writes, oldest first: the
// profile page's (C1 Profile.pending), those of deleted conversations
// included.
func (s *Store) PendingMemoryWrites(ctx context.Context, personID string) ([]MemoryWrite, error) {
	rows, err := s.pool.Query(ctx, `select `+memoryWriteCols+` from memory_writes
		where person_id = $1 and status = 'pending' order by created_at, id`, personID)
	if err != nil {
		return nil, err
	}
	return scanMemoryWrites(rows)
}

// PendingTreeWrites lists the pending writes of a conversation tree's
// sessions, oldest first (C1 /tree's pending_writes).
func (s *Store) PendingTreeWrites(ctx context.Context, rootID string) ([]MemoryWrite, error) {
	rows, err := s.pool.Query(ctx, `select `+memoryWriteCols+` from memory_writes
		where status = 'pending' and session_id in (select id from sessions where root_id = $1)
		order by created_at, id`, rootID)
	if err != nil {
		return nil, err
	}
	return scanMemoryWrites(rows)
}

func scanMemoryWrites(rows pgx.Rows) ([]MemoryWrite, error) {
	defer rows.Close()
	var out []MemoryWrite
	for rows.Next() {
		w, err := scanMemoryWrite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---- one person's memory, changed atomically (Task 13) ----

// MemoryTx is a transaction holding one person's memory lock. Every change
// to a person's profile or memory writes runs in one: a remember or forget,
// a decision on a held write, an editor save. So a write's row and its
// profile change commit together, and two writes of one turn cannot both
// pass the per-turn pending limit. It is valid only inside InMemoryTx's
// callback, and only for that person.
type MemoryTx struct {
	tx       pgx.Tx
	personID string
}

// InMemoryTx runs fn in a transaction holding the person's memory lock (a
// transaction-scoped advisory lock, released at commit or rollback). An
// error from fn rolls everything back.
func (s *Store) InMemoryTx(ctx context.Context, personID string, fn func(m *MemoryTx) error) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('memory:' || $1, 0))`, personID); err != nil {
			return err
		}
		return fn(&MemoryTx{tx: tx, personID: personID})
	})
}

// Profile returns the person's profile (empty, version 0, if none).
func (m *MemoryTx) Profile(ctx context.Context) (Profile, error) {
	return profileOn(ctx, m.tx, m.personID)
}

// SetProfile replaces the person's profile if it is still at
// expectedVersion (0 for none), and returns it as stored.
func (m *MemoryTx) SetProfile(ctx context.Context, content string, expectedVersion int) (Profile, error) {
	return setProfileOn(ctx, m.tx, m.personID, content, expectedVersion)
}

// PendingInTurn counts the turn's writes that are pending now.
func (m *MemoryTx) PendingInTurn(ctx context.Context, sessionID, turnID string) (int, error) {
	var n int
	err := m.tx.QueryRow(ctx, `select count(*) from memory_writes
		where session_id = $1 and turn_id = $2 and status = 'pending'`, sessionID, turnID).Scan(&n)
	return n, err
}

// InsertWrite records a write of the person's (its PersonID is the
// transaction's); a non-pending one is stamped decided.
func (m *MemoryTx) InsertWrite(ctx context.Context, w MemoryWrite) (MemoryWrite, error) {
	w.PersonID = m.personID
	return insertMemoryWriteOn(ctx, m.tx, w)
}

// Write returns one of the person's writes; another person's is ErrNotFound.
func (m *MemoryTx) Write(ctx context.Context, id string) (MemoryWrite, error) {
	return scanMemoryWrite(m.tx.QueryRow(ctx, `select `+memoryWriteCols+` from memory_writes
		where id = $1 and person_id = $2`, id, m.personID))
}

// DecideWrite settles one of the person's pending writes as applied or
// rejected; a non-nil matched replaces the stored one. A write that is not
// pending, or not the person's, is ErrNotFound.
func (m *MemoryTx) DecideWrite(ctx context.Context, id, status string, matched []string) (MemoryWrite, error) {
	return decideMemoryWriteOn(ctx, m.tx, id, m.personID, status, matched)
}
