package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// SetAppendHook installs a function that AppendEvent runs inside its
// transaction, after the insert. Tests only.
func (s *Store) SetAppendHook(f func(ctx context.Context, tx pgx.Tx)) { s.appendHook = f }
