package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

const messageCols = `id, session_id, parent_message_id, turn_id, role, content, sdk_uuid, model, status, error_code, created_at`

func scanMessage(row pgx.Row) (protocol.Message, error) {
	var m protocol.Message
	var content []byte
	var code *string
	err := row.Scan(&m.ID, &m.SessionID, &m.ParentMessageID, &m.TurnID, &m.Role, &content,
		&m.SDKUUID, &m.Model, &m.Status, &code, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return protocol.Message{}, ErrNotFound
	}
	if err != nil {
		return protocol.Message{}, err
	}
	if err := json.Unmarshal(content, &m.Content); err != nil {
		return protocol.Message{}, err
	}
	if code != nil {
		c := protocol.ErrorCode(*code)
		m.ErrorCode = &c
	}
	return m, nil
}

// marshalContent encodes parts, with nil as [] (containers never null, C1).
func marshalContent(parts []protocol.MessagePart) ([]byte, error) {
	if parts == nil {
		parts = []protocol.MessagePart{}
	}
	return json.Marshal(parts)
}

func errorCodeArg(c *protocol.ErrorCode) *string {
	if c == nil {
		return nil
	}
	s := string(*c)
	return &s
}

// InsertMessage stores a message and returns the stored row (created_at is
// the database's).
func (s *Store) InsertMessage(ctx context.Context, m protocol.Message) (protocol.Message, error) {
	content, err := marshalContent(m.Content)
	if err != nil {
		return protocol.Message{}, err
	}
	return scanMessage(s.pool.QueryRow(ctx, `
		insert into messages (`+messageCols+`)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, coalesce($11, now()))
		returning `+messageCols,
		m.ID, m.SessionID, m.ParentMessageID, m.TurnID, m.Role, content, m.SDKUUID, m.Model,
		m.Status, errorCodeArg(m.ErrorCode), nilTime(m.CreatedAt)))
}

// Message returns one message.
func (s *Store) Message(ctx context.Context, id string) (protocol.Message, error) {
	return scanMessage(s.pool.QueryRow(ctx, `select `+messageCols+` from messages where id = $1`, id))
}

// Messages returns a session's messages, oldest first.
func (s *Store) Messages(ctx context.Context, sessionID string) ([]protocol.Message, error) {
	rows, err := s.pool.Query(ctx, `select `+messageCols+` from messages where session_id = $1
		order by created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FinishMessage settles a message: its final content, status, SDK uuid,
// model and error code.
func (s *Store) FinishMessage(ctx context.Context, m protocol.Message) error {
	content, err := marshalContent(m.Content)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `update messages set content = $2, status = $3, sdk_uuid = $4,
		model = $5, error_code = $6 where id = $1`,
		m.ID, content, m.Status, m.SDKUUID, m.Model, errorCodeArg(m.ErrorCode))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordClientEvent makes a client's user.message idempotent: the first call
// for (session, client_id) stores messageID and reports inserted; a repeat
// returns the message id stored the first time and inserted false.
func (s *Store) RecordClientEvent(ctx context.Context, sessionID, clientID, messageID string) (stored string, inserted bool, err error) {
	err = s.pool.QueryRow(ctx, `insert into client_events (session_id, client_id, message_id)
		values ($1, $2, $3) on conflict (session_id, client_id) do nothing returning message_id`,
		sessionID, clientID, messageID).Scan(&stored)
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	err = s.pool.QueryRow(ctx, `select message_id from client_events where session_id = $1 and client_id = $2`,
		sessionID, clientID).Scan(&stored)
	return stored, false, err
}
