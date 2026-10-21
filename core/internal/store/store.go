// Package store is Luma Core's Postgres store: the C4 schema, its migrations
// and typed access to every table.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a row a method was asked for does not exist
// (or, for tree and person-scoped methods, is not the caller's).
var ErrNotFound = errors.New("store: not found")

// PGConfig is the connection. An empty field falls back to the libpq
// environment variable of the same meaning (PGHOST, PGPORT, PGDATABASE,
// PGUSER, PGPASSWORD, PGSSLMODE), so PGSSLMODE is honoured as libpq does.
type PGConfig struct {
	Host     string // a directory means a unix socket
	Port     string
	Database string
	User     string
	Password string
	SSLMode  string
}

// PGConfigFromEnv reads the C6 database variables.
func PGConfigFromEnv() PGConfig {
	return PGConfig{
		Host:     os.Getenv("PGHOST"),
		Port:     os.Getenv("PGPORT"),
		Database: os.Getenv("PGDATABASE"),
		User:     os.Getenv("PGUSER"),
		Password: os.Getenv("PGPASSWORD"),
		SSLMode:  os.Getenv("PGSSLMODE"),
	}
}

// dsn renders the config as a libpq key/value string, omitting empty fields
// so the environment fills them.
func (c PGConfig) dsn() string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+quoteDSN(v))
		}
	}
	add("host", c.Host)
	add("port", c.Port)
	add("dbname", c.Database)
	add("user", c.User)
	add("password", c.Password)
	add("sslmode", c.SSLMode)
	return strings.Join(parts, " ")
}

func quoteDSN(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}

// Durability is how hard an event append is flushed.
type Durability int

const (
	// Durable waits for the WAL flush: the default, for every event whose
	// loss would be visible (message boundaries, usage, errors).
	Durable Durability = iota
	// Relaxed runs SET LOCAL synchronous_commit = off (P24): for delta-only
	// events (text deltas) whose content the final message also carries.
	Relaxed
)

// Store is the Postgres store. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	cfg  PGConfig

	// appendHook, set only by tests (export_test.go), runs inside
	// AppendEvent's transaction after the insert.
	appendHook func(ctx context.Context, tx pgx.Tx)
}

// Open connects a pool. It does not migrate; call Migrate.
func Open(ctx context.Context, cfg PGConfig) (*Store, error) {
	pc, err := pgxpool.ParseConfig(cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("store: parse connection: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool, cfg: cfg}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// Config returns the connection config the store was opened with.
func (s *Store) Config() PGConfig { return s.cfg }

// Migrate applies the embedded migrations with goose v3. It is idempotent,
// and a session-level advisory lock keeps concurrent replicas from racing.
func (s *Store) Migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(s.pool)
	defer db.Close()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("store: migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// inTx runs fn in a transaction, rolling back on error.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// isUnique reports a unique-constraint violation, optionally on a named constraint.
func isUnique(err error, constraint string) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return constraint == "" || pe.ConstraintName == constraint
	}
	return false
}
