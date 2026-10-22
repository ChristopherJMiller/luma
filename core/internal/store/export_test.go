package store

import (
	"context"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// SetAppendHook installs a function that AppendEvent runs inside its
// transaction, after the insert. Tests only.
func (s *Store) SetAppendHook(f func(ctx context.Context, tx pgx.Tx)) { s.appendHook = f }

// SetBindHook installs a function that BindAttachments runs inside its
// transaction, after its checks and before the update. Tests only.
func (s *Store) SetBindHook(f func(ctx context.Context, tx pgx.Tx)) { s.bindHook = f }

// Pool exposes the pool to tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// AppendEventOn is AppendEvent on one given connection.
func (s *Store) AppendEventOn(ctx context.Context, conn *pgx.Conn, sessionID, typ string, payload any, d Durability) (int64, error) {
	return s.appendEvent(ctx, conn, sessionID, typ, payload, d)
}

// MigrateDownUp rolls every migration back, then applies them again.
func (s *Store) MigrateDownUp(ctx context.Context) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(s.pool)
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		return err
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		return err
	}
	return s.Migrate(ctx)
}

// The unscoped memory writes, for store tests only: production code writes
// memory through InMemoryTx, under the person's lock and scoped to them.

// SetProfile replaces a profile if its version is still expectedVersion (0
// for a person with none), and returns the new version.
func (s *Store) SetProfile(ctx context.Context, personID, content string, expectedVersion int) (int, error) {
	p, err := setProfileOn(ctx, s.pool, personID, content, expectedVersion)
	return p.Version, err
}

// InsertMemoryWrite records a write as given (any person).
func (s *Store) InsertMemoryWrite(ctx context.Context, w MemoryWrite) (MemoryWrite, error) {
	return insertMemoryWriteOn(ctx, s.pool, w)
}

// DecideMemoryWrite settles a pending write of any person; one that is not
// pending is ErrNotFound.
func (s *Store) DecideMemoryWrite(ctx context.Context, id, status string) (MemoryWrite, error) {
	w, err := s.MemoryWrite(ctx, id)
	if err != nil {
		return MemoryWrite{}, err
	}
	return decideMemoryWriteOn(ctx, s.pool, id, w.PersonID, status)
}
