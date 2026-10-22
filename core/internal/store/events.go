package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// AppendEvent stores an event of a session and returns its id, which
// increases per session. payload is the event body (a protocol.EventBody, or
// any value that marshals to a JSON object); the header fields (id,
// session_id, type, created_at) come from the row.
//
// Appends to one session are serialised by a transaction-scoped advisory lock
// held until commit, so commit order equals id order and a reader that has
// seen id N can never miss a later-committed lower id.
//
// Relaxed runs SET LOCAL synchronous_commit = off (P24) for delta-only events.
func (s *Store) AppendEvent(ctx context.Context, sessionID, typ string, payload any, d Durability) (int64, error) {
	return s.appendEvent(ctx, s.pool, sessionID, typ, payload, d)
}

// txBeginner is a pool or a single connection.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func (s *Store) appendEvent(ctx context.Context, db txBeginner, sessionID, typ string, payload any, d Durability) (int64, error) {
	if body, ok := payload.(protocol.EventBody); ok && body.EventType() != typ {
		return 0, fmt.Errorf("store: event type %q does not match its body's %q", typ, body.EventType())
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("store: event payload: %w", err)
	}
	mode := "on"
	if d == Relaxed {
		mode = "off"
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `set local synchronous_commit = `+mode); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, sessionID); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, `insert into events (session_id, type, payload) values ($1, $2, $3) returning id`,
		sessionID, typ, b).Scan(&id); err != nil {
		return 0, err
	}
	if s.appendHook != nil {
		s.appendHook(ctx, tx)
	}
	return id, tx.Commit(ctx)
}

// LastEventID returns the id of a session's newest event, or 0 when it has
// none: a stream opened with since=<it> replays every later event (C1 /tree).
func (s *Store) LastEventID(ctx context.Context, sessionID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `select coalesce(max(id), 0) from events where session_id = $1`, sessionID).Scan(&id)
	return id, err
}

// EventsSince returns up to limit events of a session with id > since, oldest
// first, decoded into protocol events. A stored event that cannot be decoded
// ends the read with an *EventDecodeError, returned together with the events
// before it.
func (s *Store) EventsSince(ctx context.Context, sessionID string, since int64, limit int) ([]protocol.Event, error) {
	rows, err := s.pool.Query(ctx, `select id, type, payload, created_at from events
		where session_id = $1 and id > $2 order by id limit $3`, sessionID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Event
	for rows.Next() {
		var (
			id      int64
			typ     string
			payload []byte
			created time.Time
		)
		if err := rows.Scan(&id, &typ, &payload, &created); err != nil {
			return nil, err
		}
		full, err := withHeader(payload, id, sessionID, typ, created)
		if err != nil {
			return out, &EventDecodeError{EventID: id, Err: err}
		}
		decoded, err := protocol.DecodeEvent(full)
		if err != nil {
			return out, &EventDecodeError{EventID: id, Err: err}
		}
		out = append(out, decoded)
	}
	return out, rows.Err()
}

// EventDecodeError is a stored event this build cannot decode: an unknown
// type after a rollback, or a payload that is not an object. Reading it again
// fails the same way, so a replay stops for good instead of retrying.
type EventDecodeError struct {
	EventID int64
	Err     error
}

func (e *EventDecodeError) Error() string {
	return fmt.Sprintf("store: event %d: %v", e.EventID, e.Err)
}
func (e *EventDecodeError) Unwrap() error { return e.Err }

// withHeader merges the row's header fields over the stored body.
func withHeader(body []byte, id int64, sessionID, typ string, created any) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	set := func(k string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		m[k] = b
		return nil
	}
	for k, v := range map[string]any{"id": id, "session_id": sessionID, "type": typ, "created_at": created} {
		if err := set(k, v); err != nil {
			return nil, err
		}
	}
	return json.Marshal(m)
}
