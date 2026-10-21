package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Setting returns the JSON value of a settings key (maintenance mode lives
// here). A missing key is ErrNotFound.
func (s *Store) Setting(ctx context.Context, key string) (json.RawMessage, error) {
	var v []byte
	err := s.pool.QueryRow(ctx, `select value from settings where key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// SetSetting stores a value under a key.
func (s *Store) SetSetting(ctx context.Context, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `insert into settings (key, value) values ($1, $2)
		on conflict (key) do update set value = excluded.value, updated_at = now()`, key, b)
	return err
}
