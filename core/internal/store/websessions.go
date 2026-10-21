package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// WebSession is a browser login session. IDHash is the hash of the cookie's
// session id; the id itself is never stored.
type WebSession struct {
	IDHash      []byte
	PersonID    string
	ConnectorID string
	Email       string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
}

// CreateWebSession stores a login session.
func (s *Store) CreateWebSession(ctx context.Context, w WebSession) error {
	_, err := s.pool.Exec(ctx, `insert into web_sessions (id_hash, person_id, connector_id, email, expires_at)
		values ($1, $2, $3, $4, $5)`, w.IDHash, w.PersonID, w.ConnectorID, w.Email, w.ExpiresAt)
	return err
}

// WebSession returns a login session that has not expired at now.
func (s *Store) WebSession(ctx context.Context, idHash []byte, now time.Time) (WebSession, error) {
	var w WebSession
	err := s.pool.QueryRow(ctx, `select id_hash, person_id, connector_id, email, created_at, last_seen_at, expires_at
		from web_sessions where id_hash = $1 and expires_at > $2`, idHash, now).
		Scan(&w.IDHash, &w.PersonID, &w.ConnectorID, &w.Email, &w.CreatedAt, &w.LastSeenAt, &w.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebSession{}, ErrNotFound
	}
	return w, err
}

// TouchWebSession records activity at now.
func (s *Store) TouchWebSession(ctx context.Context, idHash []byte, now time.Time) error {
	_, err := s.pool.Exec(ctx, `update web_sessions set last_seen_at = $2 where id_hash = $1`, idHash, now)
	return err
}

// DeleteWebSession logs a session out.
func (s *Store) DeleteWebSession(ctx context.Context, idHash []byte) error {
	_, err := s.pool.Exec(ctx, `delete from web_sessions where id_hash = $1`, idHash)
	return err
}

// DeleteExpiredWebSessions removes sessions that expired at or before now and
// returns how many.
func (s *Store) DeleteExpiredWebSessions(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `delete from web_sessions where expires_at <= $1`, now)
	return tag.RowsAffected(), err
}
