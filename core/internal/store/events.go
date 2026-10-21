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
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("store: event payload: %w", err)
	}
	var id int64
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if d == Relaxed {
			if _, err := tx.Exec(ctx, `set local synchronous_commit = off`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, sessionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `insert into events (session_id, type, payload) values ($1, $2, $3) returning id`,
			sessionID, typ, b).Scan(&id); err != nil {
			return err
		}
		if s.appendHook != nil {
			s.appendHook(ctx, tx)
		}
		return nil
	})
	return id, err
}

// EventsSince returns up to limit events of a session with id > since, oldest
// first, decoded into protocol events.
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
			return nil, err
		}
		decoded, err := protocol.DecodeEvent(full)
		if err != nil {
			return nil, fmt.Errorf("store: event %d: %w", id, err)
		}
		out = append(out, decoded)
	}
	return out, rows.Err()
}

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
