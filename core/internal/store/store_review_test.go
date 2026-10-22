package store_test

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

// S38: appends to one session are serialised by an advisory lock held to
// commit. Without it a reader could see id N+1 before id N commits.
func TestAppendEventsSerialisedPerSession(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	newSession(t, s, "ses_b")
	su := storetest.ConnAs(t, s, storetest.Superuser)

	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.SetAppendHook(func(context.Context, pgx.Tx) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release // the first append now holds its lock, uncommitted
		}
	})
	body := protocol.MessageTextDelta{MessageID: "m", Text: "x"}
	type res struct {
		id  int64
		err error
	}
	first, second := make(chan res, 1), make(chan res, 1)
	go func() {
		id, err := s.AppendEvent(ctx, "ses_a", body.EventType(), body, store.Durable)
		first <- res{id, err}
	}()
	<-entered
	go func() {
		id, err := s.AppendEvent(ctx, "ses_a", body.EventType(), body, store.Durable)
		second <- res{id, err}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for count(t, su, `select count(*) from pg_locks where locktype = 'advisory' and not granted`) == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the second append to ses_a never waited on the advisory lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case r := <-second:
		t.Fatalf("second append finished while the first was uncommitted: %+v", r)
	default:
	}

	// Another session is not blocked.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.AppendEvent(cctx, "ses_b", body.EventType(), body, store.Durable); err != nil {
		t.Fatalf("append to ses_b while ses_a is locked: %v", err)
	}

	close(release)
	r1, r2 := <-first, <-second
	if r1.err != nil || r2.err != nil {
		t.Fatalf("appends failed: %v, %v", r1.err, r2.err)
	}
	if r2.id <= r1.id {
		t.Fatalf("second id %d is not after first id %d", r2.id, r1.id)
	}
}

func TestAppendEventRejectsMismatchedType(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	body := protocol.MessageTextDelta{MessageID: "m", Text: "x"}
	if _, err := s.AppendEvent(ctx, "ses_a", "tool.started", body, store.Durable); err == nil {
		t.Fatal("a body of one type was stored under another type")
	}
	if n := count(t, storetest.ConnAs(t, s, storetest.Superuser), `select count(*) from events`); n != 0 {
		t.Fatalf("%d events stored", n)
	}
}

// SET LOCAL must end with its transaction, on the very same connection.
func TestRelaxedDoesNotLeakOnPinnedConnection(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	show := func() string {
		var v string
		if err := conn.QueryRow(ctx, `show synchronous_commit`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := show(); got != "on" {
		t.Fatalf("baseline synchronous_commit = %q", got)
	}
	body := protocol.MessageTextDelta{MessageID: "m", Text: "x"}
	if _, err := s.AppendEventOn(ctx, conn.Conn(), "ses_a", body.EventType(), body, store.Relaxed); err != nil {
		t.Fatal(err)
	}
	if got := show(); got != "on" {
		t.Fatalf("synchronous_commit = %q after a Relaxed append on the same connection", got)
	}
}

func TestMigrateDownThenUp(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	if err := s.MigrateDownUp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "ses_a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("data survived the down migration: %v", err)
	}
	newSession(t, s, "ses_a") // the schema works again
	if err := s.GrantConductor(ctx, storetest.ConductorRole); err != nil {
		t.Fatalf("grants after a round trip: %v", err)
	}
}

func TestDeleteTreeLeavesSiblingTreeWhole(t *testing.T) {
	s := storetest.New(t)
	su := storetest.ConnAs(t, s, storetest.Superuser)
	newSession(t, s, "ses_gone")
	newSession(t, s, "ses_keep")
	keep := "ses_keep"
	if _, err := s.InsertMessage(ctx, protocol.Message{ID: "msg_k", SessionID: keep, TurnID: "t", Role: "user", Status: "complete"}); err != nil {
		t.Fatal(err)
	}
	body := protocol.SessionStatus{Status: "idle"}
	if _, err := s.AppendEvent(ctx, keep, body.EventType(), body, store.Durable); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAttachment(ctx, store.Attachment{ID: "att_k", PersonID: "chris", RootID: &keep, MediaType: "text/plain",
		Filename: "k", SizeBytes: 1, SHA256: "00", Data: []byte("k")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordLedger(ctx, store.LedgerRow{SessionID: &keep, PersonID: "chris", AgentID: "luma", Tier: "fast", Source: "web",
		APIWorkspace: "interactive", Model: "m", TurnID: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMemoryWrite(ctx, store.MemoryWrite{ID: "mem_k", PersonID: "chris", SessionID: &keep, TurnID: "t",
		Op: "remember", Text: "x", Status: "applied"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordClientEvent(ctx, keep, "6f1c1f0e-7c1a-4d63-9a43-5d2f6f2f0002", "msg_k"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAnomaly(ctx, store.Anomaly{SessionID: &keep, TurnID: "t", Reason: "negative", Payload: map[string]int{}}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteTree(ctx, "chris", "ses_gone"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`select count(*) from sessions where id = 'ses_keep'`,
		`select count(*) from messages where session_id = 'ses_keep'`,
		`select count(*) from events where session_id = 'ses_keep'`,
		`select count(*) from attachments where root_id = 'ses_keep'`,
		`select count(*) from cost_ledger where session_id = 'ses_keep'`,
		`select count(*) from memory_writes where session_id = 'ses_keep'`,
		`select count(*) from client_events where session_id = 'ses_keep'`,
		`select count(*) from usage_anomalies where session_id = 'ses_keep'`,
	} {
		if n := count(t, su, q); n != 1 {
			t.Errorf("%s = %d, want 1", q, n)
		}
	}
}

func TestConductorHasExactlyTheC4Privileges(t *testing.T) {
	s := storetest.New(t)
	if err := s.GrantConductor(ctx, storetest.ConductorRole); err != nil {
		t.Fatal(err)
	}
	su := storetest.ConnAs(t, s, storetest.Superuser)
	collect := func(q string) []string {
		t.Helper()
		rows, err := su.Query(ctx, q, storetest.ConductorRole)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		sort.Strings(out)
		return out
	}
	eq := func(what string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", what, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s = %v, want %v", what, got, want)
				return
			}
		}
	}
	eq("table grants", collect(`select table_name || ':' || privilege_type from information_schema.role_table_grants
		where grantee = $1`), []string{"sdk_transcript_entries:DELETE", "sdk_transcript_entries:INSERT", "sdk_transcript_entries:SELECT"})
	eq("usage grants", collect(`select object_type || ':' || object_name || ':' || privilege_type
		from information_schema.role_usage_grants where grantee = $1 and object_schema = 'public'`),
		[]string{"SEQUENCE:sdk_transcript_entries_seq_seq:USAGE"})
	eq("column grants", collect(`select table_name || '.' || column_name || ':' || privilege_type
		from information_schema.role_column_grants where grantee = $1 and table_name <> 'sdk_transcript_entries'`), nil)
	// Whatever the views say, ask the server about every table.
	eq("tables with any privilege", collect(`select tablename from pg_tables
		where schemaname = 'public' and has_table_privilege($1, quote_ident(tablename), 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')`),
		[]string{"sdk_transcript_entries"})
	eq("database privileges", collect(`select p from unnest(array['CONNECT','CREATE','TEMPORARY']) p
		where has_database_privilege($1, current_database(), p)`), []string{"CONNECT"})
	eq("schema privileges", collect(`select p from unnest(array['USAGE','CREATE']) p
		where has_schema_privilege($1, 'public', p)`), []string{"USAGE"})
}

// REVOKE and GRANT only warn when the caller does not own the object;
// GrantConductor must notice that its work did not take effect.
func TestGrantConductorFailsWhenNotOwner(t *testing.T) {
	s := storetest.New(t)
	su := storetest.ConnAs(t, s, storetest.Superuser)
	if _, err := su.Exec(ctx, `create role intruder login`); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	cfg.User = "intruder"
	other, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.GrantConductor(ctx, storetest.ConductorRole); err == nil {
		t.Fatal("GrantConductor reported success without owning the database")
	}
}

func TestBindAttachmentsDuplicateIDs(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	if err := s.InsertAttachment(ctx, store.Attachment{ID: "att_1", PersonID: "chris", MediaType: "text/plain",
		Filename: "f", SizeBytes: 1, SHA256: "00", Data: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindAttachments(ctx, "chris", "ses_a", []string{"att_1", "att_1"}); err != nil {
		t.Fatalf("a repeated id failed the bind: %v", err)
	}
	if n, _ := s.TreeAttachmentBytes(ctx, "ses_a"); n != 1 {
		t.Fatalf("tree bytes = %d", n)
	}
}
