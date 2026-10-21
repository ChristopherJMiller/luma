// Package storetest starts a throwaway Postgres for store tests.
package storetest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ChristopherJMiller/luma/core/internal/store"
)

const (
	// Superuser is the bootstrap role of the throwaway server.
	Superuser = "postgres"
	// ConductorRole is created (LOGIN, no grants) by New; tests call
	// GrantConductor to give it the C4 grants.
	ConductorRole = "luma_conductor"
	// Database is the database New creates and migrates.
	Database = "luma"
)

// New starts a Postgres (initdb into t.TempDir(), a unix socket, a random
// port), creates the luma database and the luma_conductor role, opens a store
// as the superuser and migrates it. It skips under -short and stops the
// server when the test ends. initdb and pg_ctl must be on PATH (nix develop).
func New(t testing.TB) *store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a Postgres server; skipped under -short")
	}
	for _, bin := range []string{"initdb", "pg_ctl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("storetest: %s not on PATH (run inside `nix develop`): %v", bin, err)
		}
	}
	data := filepath.Join(t.TempDir(), "data")
	// A unix socket path is limited to ~100 bytes, which t.TempDir() can exceed.
	sock, err := os.MkdirTemp("", "lumapg")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(20000 + rand.IntN(40000)) //nolint:gosec // a socket file name, not a secret

	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(context.Background(), name, args...).CombinedOutput(); err != nil { //nolint:gosec // fixed binaries, test-only
			t.Fatalf("storetest: %s %v: %v\n%s", name, args, err, out)
		}
	}
	run("initdb", "-D", data, "-U", Superuser, "--auth=trust", "-E", "UTF8", "--no-sync")
	run("pg_ctl", "start", "-w", "-t", "60", "-D", data, "-l", filepath.Join(sock, "log"),
		"-o", fmt.Sprintf("-p %s -k %s -c listen_addresses= -c fsync=off", port, sock))
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "pg_ctl", "stop", "-m", "immediate", "-D", data).Run() //nolint:gosec // test-only; data is our temp dir
		_ = os.RemoveAll(sock)
	})

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%s user=%s dbname=postgres sslmode=disable", sock, port, Superuser))
	if err != nil {
		t.Fatalf("storetest: connect: %v", err)
	}
	for _, q := range []string{`create database ` + Database, `create role ` + ConductorRole + ` login`} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("storetest: %s: %v", q, err)
		}
	}
	_ = admin.Close(ctx)

	cfg := store.PGConfig{Host: sock, Port: port, Database: Database, User: Superuser, SSLMode: "disable"}
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := store.Open(octx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

// ConnAs opens a plain connection to the store's server and database as
// another role (trust auth), for tests of what that role may do.
func ConnAs(t testing.TB, s *store.Store, role string) *pgx.Conn {
	t.Helper()
	conn, err := TryConnAs(s, role)
	if err != nil {
		t.Fatalf("storetest: connect as %s: %v", role, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// TryConnAs is ConnAs returning the error, for tests that expect a refusal.
func TryConnAs(s *store.Store, role string) (*pgx.Conn, error) {
	c := s.Config()
	return pgx.Connect(context.Background(),
		fmt.Sprintf("host=%s port=%s user=%s dbname=%s sslmode=disable", c.Host, c.Port, role, c.Database))
}
