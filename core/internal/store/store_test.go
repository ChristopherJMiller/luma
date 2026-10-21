package store_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

var ctx = context.Background()

func newSession(t *testing.T, s *store.Store, id string) store.Session {
	t.Helper()
	ses, err := s.CreateSession(ctx, store.Session{
		ID: id, PersonID: "chris", AgentID: "luma", AgentVersion: "abc123",
		Tier: "fast", Model: "claude-haiku-5-5", HelperModel: "claude-haiku-5-5",
		Effort: "low", CacheTTL: "1h", APIWorkspace: "interactive", Source: "web",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ses
}

func newFork(t *testing.T, s *store.Store, id, root, parent string) {
	t.Helper()
	_, err := s.CreateSession(ctx, store.Session{
		ID: id, RootID: root, ParentID: &parent, PersonID: "chris", AgentID: "luma", AgentVersion: "abc123",
		Tier: "fast", Model: "claude-haiku-5-5", HelperModel: "claude-haiku-5-5",
		Effort: "low", CacheTTL: "1h", APIWorkspace: "interactive", Source: "web",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, conn *pgx.Conn, q string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestMigrateIdempotent(t *testing.T) {
	s := storetest.New(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("third Migrate: %v", err)
	}
	su := storetest.ConnAs(t, s, storetest.Superuser)
	if n := count(t, su, `select count(*) from goose_db_version where version_id = 1`); n != 1 {
		t.Fatalf("migration 1 recorded %d times, want 1", n)
	}
}

func TestEventIDsIncreasePerSession(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	newSession(t, s, "ses_b")

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				d := store.Durable
				if i%2 == 0 {
					d = store.Relaxed
				}
				if _, err := s.AppendEvent(ctx, "ses_a", "message.text_delta",
					protocol.MessageTextDelta{MessageID: "msg_1", Text: "x"}, d); err != nil {
					errs <- err
					return
				}
				// interleave another session so ids are not contiguous
				if _, err := s.AppendEvent(ctx, "ses_b", "session.status", protocol.SessionStatus{Status: "running"}, store.Durable); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	evs, err := s.EventsSince(ctx, "ses_a", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 200 {
		t.Fatalf("got %d events, want 200", len(evs))
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].ID <= evs[i-1].ID {
			t.Fatalf("ids not increasing at %d: %d then %d", i, evs[i-1].ID, evs[i].ID)
		}
	}
	for _, e := range evs {
		if e.SessionID != "ses_a" || e.Body.EventType() != "message.text_delta" || e.CreatedAt.IsZero() {
			t.Fatalf("bad event %+v", e)
		}
	}
	// since is exclusive, limit applies, and the other session is separate.
	mid := evs[99].ID
	rest, err := s.EventsSince(ctx, "ses_a", mid, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 100 || rest[0].ID != evs[100].ID {
		t.Fatalf("since=%d returned %d events starting at %d", mid, len(rest), rest[0].ID)
	}
	lim, err := s.EventsSince(ctx, "ses_a", 0, 5)
	if err != nil || len(lim) != 5 || lim[4].ID != evs[4].ID {
		t.Fatalf("limit 5: %d events, err %v", len(lim), err)
	}
	other, err := s.EventsSince(ctx, "ses_b", 0, 1000)
	if err != nil || len(other) != 200 {
		t.Fatalf("ses_b has %d events (err %v), want 200", len(other), err)
	}
}

func TestEventBodyRoundTrips(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	want := protocol.ToolStarted{MessageID: "msg_1", ToolUseID: "toolu_1", Name: "WebSearch", Summary: "searching"}
	if _, err := s.AppendEvent(ctx, "ses_a", want.EventType(), want, store.Durable); err != nil {
		t.Fatal(err)
	}
	evs, err := s.EventsSince(ctx, "ses_a", 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %d events, err %v", len(evs), err)
	}
	if got, ok := evs[0].Body.(protocol.ToolStarted); !ok || got != want {
		t.Fatalf("body = %#v, want %#v", evs[0].Body, want)
	}
}

func TestRelaxedAppendCommitsAsync(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	var seen string
	s.SetAppendHook(func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `select current_setting('synchronous_commit')`).Scan(&seen); err != nil {
			t.Error(err)
		}
	})
	body := protocol.MessageTextDelta{MessageID: "m", Text: "x"}
	if _, err := s.AppendEvent(ctx, "ses_a", body.EventType(), body, store.Relaxed); err != nil {
		t.Fatal(err)
	}
	if seen != "off" {
		t.Fatalf("Relaxed ran with synchronous_commit = %q, want off", seen)
	}
	if _, err := s.AppendEvent(ctx, "ses_a", body.EventType(), body, store.Durable); err != nil {
		t.Fatal(err)
	}
	if seen != "on" {
		t.Fatalf("Durable ran with synchronous_commit = %q, want on", seen)
	}
	// SET LOCAL must not leak to the next use of the pooled connection.
	if _, err := s.AppendEvent(ctx, "ses_a", body.EventType(), body, store.Durable); err != nil {
		t.Fatal(err)
	}
	if seen != "on" {
		t.Fatalf("after Relaxed, Durable ran with %q", seen)
	}
}

func insertTranscript(t *testing.T, conn *pgx.Conn, project, sdk, uuid, entry string) {
	t.Helper()
	var u *string
	if uuid != "" {
		u = &uuid
	}
	if _, err := conn.Exec(ctx, `insert into sdk_transcript_entries (project_key, session_id, uuid, entry)
		values ($1, $2, $3, $4)`, project, sdk, u, entry); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTreeRemovesTranscriptsByProjectKey(t *testing.T) {
	s := storetest.New(t)
	su := storetest.ConnAs(t, s, storetest.Superuser)

	newSession(t, s, "ses_root")
	newFork(t, s, "ses_fork", "ses_root", "ses_root")
	newSession(t, s, "ses_sibling")
	if _, err := s.InsertMessage(ctx, protocol.Message{ID: "msg_1", SessionID: "ses_fork", TurnID: "t1", Role: "user",
		Content: []protocol.MessagePart{{Type: "text", Text: "hi"}}, Status: "complete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "ses_fork", "session.status", protocol.SessionStatus{Status: "idle"}, store.Durable); err != nil {
		t.Fatal(err)
	}
	root := "ses_root"
	if err := s.InsertAttachment(ctx, store.Attachment{ID: "att_1", PersonID: "chris", RootID: &root,
		MediaType: "text/plain", Filename: "a.txt", SizeBytes: 3, SHA256: "00", Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	fork := "ses_fork"
	if _, err := s.InsertMemoryWrite(ctx, store.MemoryWrite{ID: "mem_1", PersonID: "chris", SessionID: &fork,
		TurnID: "t1", Op: "remember", Text: "likes tea", Status: "applied"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordLedger(ctx, store.LedgerRow{SessionID: &fork, PersonID: "chris", AgentID: "luma", Tier: "fast",
		Source: "web", APIWorkspace: "interactive", Model: "claude-haiku-5-5", TurnID: "t1", InputTokens: 10, CostUSD: 0.01}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAnomaly(ctx, store.Anomaly{SessionID: &fork, TurnID: "t1", Reason: "negative", Payload: map[string]int{"n": 1}}); err != nil {
		t.Fatal(err)
	}

	// Two sdk sessions under the root's project key (the fork has its own sdk
	// session), one under the sibling's.
	insertTranscript(t, su, "ses_root", "sdk-1", "u1", `{"a":1}`)
	insertTranscript(t, su, "ses_root", "sdk-2", "u2", `{"a":2}`)
	insertTranscript(t, su, "ses_sibling", "sdk-3", "u3", `{"a":3}`)

	if err := s.DeleteTree(ctx, "someone-else", "ses_root"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another person's DeleteTree = %v, want ErrNotFound", err)
	}
	if err := s.DeleteTree(ctx, "chris", "ses_fork"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteTree of a non-root = %v, want ErrNotFound", err)
	}
	if err := s.DeleteTree(ctx, "chris", "ses_root"); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		q    string
		want int
	}{
		{`select count(*) from sdk_transcript_entries where project_key = 'ses_root'`, 0},
		{`select count(*) from sdk_transcript_entries where project_key = 'ses_sibling'`, 1},
		{`select count(*) from sessions where root_id = 'ses_root'`, 0},
		{`select count(*) from sessions where id = 'ses_sibling'`, 1},
		{`select count(*) from messages`, 0},
		{`select count(*) from events`, 0},
		{`select count(*) from attachments`, 0},
		{`select count(*) from memory_writes where session_id is null`, 1},
		{`select count(*) from cost_ledger where session_id is null`, 1},
		{`select count(*) from usage_anomalies where session_id is null`, 1},
	} {
		if got := count(t, su, c.q); got != c.want {
			t.Errorf("%s = %d, want %d", c.q, got, c.want)
		}
	}
	if err := s.DeleteTree(ctx, "chris", "ses_root"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second DeleteTree = %v, want ErrNotFound", err)
	}
}

func TestConductorRoleGrants(t *testing.T) {
	s := storetest.New(t)
	if err := s.GrantConductor(ctx, storetest.ConductorRole); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantConductor(ctx, storetest.ConductorRole); err != nil {
		t.Fatalf("GrantConductor is not idempotent: %v", err)
	}
	newSession(t, s, "ses_root")
	c := storetest.ConnAs(t, s, storetest.ConductorRole)

	insertTranscript(t, c, "ses_root", "sdk-1", "u1", `{"x":1}`)
	if n := count(t, c, `select count(*) from sdk_transcript_entries`); n != 1 {
		t.Fatalf("conductor sees %d transcript rows, want 1", n)
	}
	// The partial unique index is usable by the role (append's ON CONFLICT).
	tag, err := c.Exec(ctx, `insert into sdk_transcript_entries (project_key, session_id, uuid, entry)
		values ('ses_root', 'sdk-1', 'u1', '{}') on conflict (project_key, session_id, subpath, uuid) where uuid is not null do nothing`)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("conflicting append: %v, %d rows", err, tag.RowsAffected())
	}
	if tag, err := c.Exec(ctx, `delete from sdk_transcript_entries where project_key = 'ses_root'`); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete: %v, %d rows", err, tag.RowsAffected())
	}

	denied := func(q string) {
		t.Helper()
		_, err := c.Exec(ctx, q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42501" {
			t.Errorf("%q: err = %v, want permission denied (42501)", q, err)
		}
	}
	denied(`select * from messages`)
	denied(`select * from sessions`)
	denied(`select * from attachments`)
	denied(`update sdk_transcript_entries set entry = 'x'`)
	denied(`create table sneaky (id int)`)
}

func TestRevokePublic(t *testing.T) {
	s := storetest.New(t)
	su := storetest.ConnAs(t, s, storetest.Superuser)
	if _, err := su.Exec(ctx, `create role stranger login`); err != nil {
		t.Fatal(err)
	}
	// Before the grants, PUBLIC may connect (Postgres's default).
	if c, err := storetest.TryConnAs(s, "stranger"); err != nil {
		t.Fatalf("precondition: stranger cannot connect before GrantConductor: %v", err)
	} else {
		_ = c.Close(ctx)
	}
	if err := s.GrantConductor(ctx, storetest.ConductorRole); err != nil {
		t.Fatal(err)
	}
	c, err := storetest.TryConnAs(s, "stranger")
	if err == nil {
		_ = c.Close(ctx)
		t.Fatal("a role without grants can still CONNECT to luma")
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "42501" {
		t.Fatalf("stranger connect err = %v, want permission denied (42501)", err)
	}
	if n := count(t, su, `select count(*) from pg_namespace n, aclexplode(n.nspacl) a
		where n.nspname = 'public' and a.grantee = 0`); n != 0 {
		t.Fatalf("PUBLIC still holds %d privileges on schema public", n)
	}
	if c, err := storetest.TryConnAs(s, storetest.ConductorRole); err != nil {
		t.Fatalf("the conductor lost CONNECT: %v", err)
	} else {
		_ = c.Close(ctx)
	}
}

func TestTranscriptEntryTextPreservedExactly(t *testing.T) {
	s := storetest.New(t)
	if err := s.GrantConductor(ctx, storetest.ConductorRole); err != nil {
		t.Fatal(err)
	}
	c := storetest.ConnAs(t, s, storetest.ConductorRole)
	// Key order z,a,m, duplicate spacing, an escaped quote, an astral character
	// and a combining sequence: jsonb would reorder and respace all of it.
	entry := `{"z":1,  "a":"café \"q\" 😀 é","m":[3,2,1], "z2":1.50}`
	insertTranscript(t, c, "ses_root", "sdk-1", "u1", entry)
	var typ string
	if err := c.QueryRow(ctx, `select data_type from information_schema.columns
		where table_name = 'sdk_transcript_entries' and column_name = 'entry'`).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "text" {
		t.Fatalf("entry column type = %q, want text", typ)
	}
	var got string
	if err := c.QueryRow(ctx, `select entry from sdk_transcript_entries where uuid = 'u1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != entry {
		t.Fatalf("entry changed:\n got %q\nwant %q", got, entry)
	}
}

func TestClientEventIdempotencyUnique(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	cid := "6f1c1f0e-7c1a-4d63-9a43-5d2f6f2f0001"
	got, inserted, err := s.RecordClientEvent(ctx, "ses_a", cid, "msg_1")
	if err != nil || !inserted || got != "msg_1" {
		t.Fatalf("first = %q, %v, %v", got, inserted, err)
	}
	got, inserted, err = s.RecordClientEvent(ctx, "ses_a", cid, "msg_2")
	if err != nil || inserted || got != "msg_1" {
		t.Fatalf("repeat = %q, %v, %v; want msg_1, false", got, inserted, err)
	}
	newSession(t, s, "ses_b")
	if got, inserted, err = s.RecordClientEvent(ctx, "ses_b", cid, "msg_3"); err != nil || !inserted || got != "msg_3" {
		t.Fatalf("same client id in another session = %q, %v, %v", got, inserted, err)
	}
	su := storetest.ConnAs(t, s, storetest.Superuser)
	_, err = su.Exec(ctx, `insert into client_events values ('ses_a', $1, 'msg_9')`, cid)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23505" {
		t.Fatalf("raw duplicate insert err = %v, want unique violation", err)
	}
}

func TestPersonRefStableAndOpaque(t *testing.T) {
	s := storetest.New(t)
	re := regexp.MustCompile(`^u_[0-9a-f]{10}$`)
	a, err := s.PersonRef(ctx, "chris")
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString(a) {
		t.Fatalf("ref %q does not match %s", a, re)
	}
	again, err := s.PersonRef(ctx, "chris")
	if err != nil || again != a {
		t.Fatalf("second call = %q, %v; want %q", again, err, a)
	}
	b, err := s.PersonRef(ctx, "erin")
	if err != nil || !re.MatchString(b) || b == a {
		t.Fatalf("erin = %q, %v; want a different well-formed ref than %q", b, err, a)
	}
	// Concurrent first use of one person converges on one ref.
	var wg sync.WaitGroup
	refs := make([]string, 6)
	for i := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs[i], _ = s.PersonRef(ctx, "newcomer")
		}()
	}
	wg.Wait()
	for _, r := range refs {
		if r == "" || r != refs[0] {
			t.Fatalf("concurrent refs differ: %v", refs)
		}
	}
}

func TestAttachmentsCascadeWithRoot(t *testing.T) {
	s := storetest.New(t)
	su := storetest.ConnAs(t, s, storetest.Superuser)
	newSession(t, s, "ses_root")
	newSession(t, s, "ses_other")
	root, other := "ses_root", "ses_other"
	for _, a := range []store.Attachment{
		{ID: "att_1", PersonID: "chris", RootID: &root, SizeBytes: 3, Data: []byte("abc")},
		{ID: "att_2", PersonID: "chris", RootID: &other, SizeBytes: 5, Data: []byte("abcde")},
		{ID: "att_3", PersonID: "chris", SizeBytes: 1, Data: []byte("z")}, // unbound
	} {
		a.MediaType, a.Filename, a.SHA256 = "text/plain", "f.txt", "00"
		if err := s.InsertAttachment(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.TreeAttachmentBytes(ctx, "ses_root"); err != nil || n != 3 {
		t.Fatalf("tree bytes = %d, %v; want 3", n, err)
	}
	// Deleting the root session row alone (no DeleteTree) takes its attachments.
	if _, err := su.Exec(ctx, `delete from sessions where id = 'ses_root'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attachment(ctx, "att_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("att_1 after root delete: %v, want ErrNotFound", err)
	}
	for _, id := range []string{"att_2", "att_3"} {
		if _, err := s.Attachment(ctx, id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// Binding: the person's unbound attachment binds; someone else's does not.
	if err := s.BindAttachments(ctx, "chris", "ses_other", []string{"att_3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindAttachments(ctx, "erin", "ses_other", []string{"att_2"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding another person's attachment: %v, want ErrNotFound", err)
	}
	if n, _ := s.TreeAttachmentBytes(ctx, "ses_other"); n != 6 {
		t.Fatalf("tree bytes = %d, want 6", n)
	}
}

func TestSDKSessionIDAndLastTotalsPersist(t *testing.T) {
	s := storetest.New(t)
	fresh := newSession(t, s, "ses_a")
	if fresh.SDKSessionID != nil || fresh.LastTotals != nil || fresh.HelperModel != "claude-haiku-5-5" ||
		fresh.APIWorkspace != "interactive" || fresh.Workspace != "none" {
		t.Fatalf("fresh session = %+v", fresh)
	}
	if err := s.SetSDKSessionID(ctx, "ses_a", "sdk-uuid-1"); err != nil {
		t.Fatal(err)
	}
	want := protocol.Totals{
		Models: map[string]protocol.ModelUsage{
			"claude-haiku-5-5": {Input: 10, Output: 20, CacheRead: 300, CacheCreation: 40, WebSearchRequests: 2, CostUSD: 0.0123},
		},
		CostUSD: 0.0123,
	}
	if err := s.SetLastTotals(ctx, "ses_a", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(ctx, "ses_a")
	if err != nil {
		t.Fatal(err)
	}
	if got.SDKSessionID == nil || *got.SDKSessionID != "sdk-uuid-1" {
		t.Fatalf("sdk_session_id = %v", got.SDKSessionID)
	}
	if got.LastTotals == nil || got.LastTotals.CostUSD != 0.0123 || got.LastTotals.Models["claude-haiku-5-5"] != want.Models["claude-haiku-5-5"] {
		t.Fatalf("last_totals = %+v", got.LastTotals)
	}
	if err := s.SetSDKSessionID(ctx, "ses_missing", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session: %v, want ErrNotFound", err)
	}
	// workspace is the sandbox and stays 'none' (S5).
	su := storetest.ConnAs(t, s, storetest.Superuser)
	if _, err := su.Exec(ctx, `update sessions set workspace = 'sandbox' where id = 'ses_a'`); err == nil {
		t.Fatal("workspace accepted a value other than none")
	}
}

func TestRunningSessionsAndStatus(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	newSession(t, s, "ses_b")
	if err := s.SetSessionStatus(ctx, "ses_b", "running"); err != nil {
		t.Fatal(err)
	}
	ids, err := s.RunningSessions(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "ses_b" {
		t.Fatalf("running = %v, %v", ids, err)
	}
	if err := s.TaintSession(ctx, "ses_a"); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Session(ctx, "ses_a")
	time.Sleep(5 * time.Millisecond)
	if err := s.TaintSession(ctx, "ses_a"); err != nil {
		t.Fatal(err)
	}
	second, _ := s.Session(ctx, "ses_a")
	if first.TaintedAt == nil || !first.TaintedAt.Equal(*second.TaintedAt) {
		t.Fatalf("taint moved: %v then %v", first.TaintedAt, second.TaintedAt)
	}
	if err := s.AddBoundModel(ctx, "ses_a", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBoundModel(ctx, "ses_a", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	ses, _ := s.Session(ctx, "ses_a")
	if fmt.Sprint(ses.BoundModels) != "[claude-sonnet-5-5]" {
		t.Fatalf("bound_models = %v", ses.BoundModels)
	}
}

func TestMessagesRoundTripWithTurnID(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	in := protocol.Message{ID: "msg_1", SessionID: "ses_a", TurnID: "turn_1", Role: "assistant",
		Content: []protocol.MessagePart{{Type: "text", Text: "héllo"}, {Type: "tool", ToolUseID: "t1", Name: "remember", Summary: "ok"}},
		Status:  "running"}
	if _, err := s.InsertMessage(ctx, in); err != nil {
		t.Fatal(err)
	}
	uuid, model, code := "sdk-u", "claude-haiku-5-5", protocol.ErrorCode("rate_limited")
	in.Status, in.SDKUUID, in.Model, in.ErrorCode = "error", &uuid, &model, &code
	if err := s.FinishMessage(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.Message(ctx, "msg_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TurnID != "turn_1" || got.Status != "error" || *got.SDKUUID != uuid || *got.ErrorCode != code ||
		len(got.Content) != 2 || got.Content[0].Text != "héllo" || got.Content[1].Name != "remember" || got.CreatedAt.IsZero() {
		t.Fatalf("message = %+v", got)
	}
	// An empty content list stays [] (C1: containers never null).
	if _, err := s.InsertMessage(ctx, protocol.Message{ID: "msg_2", SessionID: "ses_a", TurnID: "turn_1", Role: "user", Status: "complete"}); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx, "ses_a")
	if err != nil || len(msgs) != 2 || msgs[1].Content == nil {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
}

func TestLedgerReconcileEvalsSettingsMemory(t *testing.T) {
	s := storetest.New(t)
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, r := range []store.LedgerRow{
		{APIWorkspace: "interactive", CostUSD: 0.25, CreatedAt: day.Add(time.Hour)},
		{APIWorkspace: "ci", CostUSD: 0.5, CreatedAt: day.Add(2 * time.Hour)},
		{APIWorkspace: "interactive", CostUSD: 9, CreatedAt: day.Add(25 * time.Hour)},
	} {
		r.PersonID, r.AgentID, r.Tier, r.Source, r.Model, r.TurnID = "chris", "luma", "fast", "web", "m", "t"
		if _, err := s.RecordLedger(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if usd, err := s.LedgerUSDForDay(ctx, day, "interactive"); err != nil || usd != 0.25 {
		t.Fatalf("interactive day = %v, %v", usd, err)
	}
	if usd, err := s.LedgerUSDForDay(ctx, day, ""); err != nil || usd != 0.75 {
		t.Fatalf("all workspaces day = %v, %v", usd, err)
	}
	for _, api := range []float64{1, 2} { // second call replaces the first
		if err := s.UpsertReconciliation(ctx, store.Reconciliation{Day: day, Workspace: "luma-ci", APIUSD: api, LedgerUSD: 0.5}); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := s.Reconciliations(ctx, day)
	if err != nil || len(rec) != 1 || rec[0].APIUSD != 2 || rec[0].Workspace != "luma-ci" {
		t.Fatalf("reconciliations = %+v, %v", rec, err)
	}

	run := protocol.EvalRun{Suite: "nightly", SDKVersion: "1", CLIVersion: "2", Image: "img@sha256:x",
		StartedAt: day, FinishedAt: day.Add(time.Minute), Passed: 3, Failed: 1, CostUSD: 1.5,
		Results: []protocol.EvalResult{}}
	if _, err := s.InsertEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastEvalRun(ctx, "nightly")
	if err != nil || got.Passed != 3 || got.Failed != 1 || got.CostUSD != 1.5 || got.Results == nil {
		t.Fatalf("last eval run = %+v, %v", got, err)
	}
	if _, err := s.LastEvalRun(ctx, "weekly"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing suite: %v", err)
	}

	if _, err := s.Setting(ctx, "maintenance"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing setting: %v", err)
	}
	if err := s.SetSetting(ctx, "maintenance", map[string]bool{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Setting(ctx, "maintenance"); err != nil || string(v) != `{"enabled": true}` {
		t.Fatalf("setting = %s, %v", v, err)
	}

	if p, err := s.Profile(ctx, "chris"); err != nil || p.Version != 0 || p.Content != "" {
		t.Fatalf("empty profile = %+v, %v", p, err)
	}
	if v, err := s.SetProfile(ctx, "chris", "likes tea", 0); err != nil || v != 1 {
		t.Fatalf("first SetProfile = %d, %v", v, err)
	}
	if _, err := s.SetProfile(ctx, "chris", "stale", 0); !errors.Is(err, store.ErrProfileConflict) {
		t.Fatalf("stale SetProfile: %v", err)
	}
	if v, err := s.SetProfile(ctx, "chris", "likes tea, hates coffee", 1); err != nil || v != 2 {
		t.Fatalf("second SetProfile = %d, %v", v, err)
	}
	w, err := s.InsertMemoryWrite(ctx, store.MemoryWrite{ID: "mem_1", PersonID: "chris", TurnID: "t", Op: "forget", Text: "tea", Status: "pending", Untrusted: true})
	if err != nil || w.DecidedAt != nil {
		t.Fatalf("pending write = %+v, %v", w, err)
	}
	if w, err = s.DecideMemoryWrite(ctx, "mem_1", "applied"); err != nil || w.Status != "applied" || w.DecidedAt == nil {
		t.Fatalf("decided = %+v, %v", w, err)
	}
	if _, err = s.DecideMemoryWrite(ctx, "mem_1", "rejected"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deciding twice: %v", err)
	}
}

func TestWebSessions(t *testing.T) {
	s := storetest.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	h := []byte("hash-1")
	if err := s.CreateWebSession(ctx, store.WebSession{IDHash: h, PersonID: "chris", ConnectorID: "google", Email: "c@example.test", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	w, err := s.WebSession(ctx, h, now)
	if err != nil || w.PersonID != "chris" || w.ConnectorID != "google" {
		t.Fatalf("web session = %+v, %v", w, err)
	}
	if _, err := s.WebSession(ctx, h, now.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired session: %v", err)
	}
	if n, err := s.DeleteExpiredWebSessions(ctx, now.Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("expired delete = %d, %v", n, err)
	}
}
