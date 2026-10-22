package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

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
	p := Profile{PersonID: personID}
	err := s.pool.QueryRow(ctx, `select content, version, updated_at from memory_profiles where person_id = $1`, personID).
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
	var v int
	var err error
	if expectedVersion == 0 {
		err = s.pool.QueryRow(ctx, `insert into memory_profiles (person_id, content, version) values ($1, $2, 1)
			on conflict (person_id) do nothing returning version`, personID, content).Scan(&v)
	} else {
		err = s.pool.QueryRow(ctx, `update memory_profiles set content = $2, version = version + 1, updated_at = now()
			where person_id = $1 and version = $3 returning version`, personID, content, expectedVersion).Scan(&v)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrProfileConflict
	}
	return v, err
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
	return scanMemoryWrite(s.pool.QueryRow(ctx, `insert into memory_writes (id, person_id, session_id, turn_id,
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
	return scanMemoryWrite(s.pool.QueryRow(ctx, `update memory_writes set status = $2, decided_at = now()
		where id = $1 and status = 'pending' returning `+memoryWriteCols, id, status))
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
