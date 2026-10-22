package store_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

const mib = 1 << 20

// insertSized stores an unbound attachment of chris's. The tree cap sums
// size_bytes, so the bytes themselves stay small.
func insertSized(t *testing.T, s *store.Store, id string, size int64) {
	t.Helper()
	if err := s.InsertAttachment(ctx, store.Attachment{ID: id, PersonID: "chris", MediaType: "application/pdf",
		Filename: id + ".pdf", SizeBytes: size, SHA256: "00", Data: []byte("%PDF-")}); err != nil {
		t.Fatal(err)
	}
}

func rootOf(t *testing.T, s *store.Store, id string) *string {
	t.Helper()
	a, err := s.AttachmentMeta(ctx, "chris", id)
	if err != nil {
		t.Fatal(err)
	}
	return a.RootID
}

func TestBindAttachmentsTreeCap(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	newSession(t, s, "ses_b")
	insertSized(t, s, "att_12", 12*mib)
	insertSized(t, s, "att_8", 8*mib)
	insertSized(t, s, "att_1", 1)
	insertSized(t, s, "att_small", 1)
	const limit = 20 * mib

	if err := s.BindAttachments(ctx, "chris", "ses_a", []string{"att_12"}, limit); err != nil {
		t.Fatal(err)
	}
	// Exactly at the cap is allowed.
	if err := s.BindAttachments(ctx, "chris", "ses_a", []string{"att_8"}, limit); err != nil {
		t.Fatalf("binding up to exactly 20 MiB: %v", err)
	}
	// One byte more is not, and nothing of a refused bind is bound.
	if err := s.BindAttachments(ctx, "chris", "ses_a", []string{"att_small", "att_1"}, limit); !errors.Is(err, store.ErrTreeFull) {
		t.Fatalf("one byte over the cap: %v, want ErrTreeFull", err)
	}
	for _, id := range []string{"att_1", "att_small"} {
		if r := rootOf(t, s, id); r != nil {
			t.Fatalf("%s bound to %s by a refused bind", id, *r)
		}
	}
	// Ids already bound to this tree are not counted twice.
	if err := s.BindAttachments(ctx, "chris", "ses_a", []string{"att_12", "att_8"}, limit); err != nil {
		t.Fatalf("re-binding the tree's own attachments: %v", err)
	}
	if n, _ := s.TreeAttachmentBytes(ctx, "ses_a"); n != 20*mib {
		t.Fatalf("tree bytes = %d, want %d", n, 20*mib)
	}
	// The refused upload still binds to another tree.
	if err := s.BindAttachments(ctx, "chris", "ses_b", []string{"att_1"}, limit); err != nil {
		t.Fatal(err)
	}
	// Another tree's attachment is not found, whatever the cap.
	if err := s.BindAttachments(ctx, "chris", "ses_b", []string{"att_8"}, limit); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another tree's attachment: %v, want ErrNotFound", err)
	}
}

// Two sessions of one tree (a fork and its parent) send through different
// actors, so two binds can run at once. Each must see the other's bytes:
// binds into one tree are serialised until commit.
func TestBindAttachmentsSerialisedPerTree(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	insertSized(t, s, "att_x", 12*mib)
	insertSized(t, s, "att_y", 12*mib)
	su := storetest.ConnAs(t, s, storetest.Superuser)

	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.SetBindHook(func(context.Context, pgx.Tx) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release // the first bind is checked and written, not committed
		}
	})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- s.BindAttachments(ctx, "chris", "ses_a", []string{"att_x"}, 20*mib) }()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("the first bind returned (%v) without running the bind hook", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the first bind never reached the bind hook")
	}
	go func() { second <- s.BindAttachments(ctx, "chris", "ses_a", []string{"att_y"}, 20*mib) }()

	deadline := time.Now().Add(10 * time.Second)
	for count(t, su, `select count(*) from pg_locks where locktype = 'advisory' and not granted`) == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the second bind into the tree never waited for the first")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The tree lock is not the root session's event lock (S38).
	body := protocol.MessageTextDelta{MessageID: "m", Text: "x"}
	if _, err := s.AppendEvent(ctx, "ses_a", body.EventType(), body, store.Durable); err != nil {
		close(release)
		t.Fatalf("an append to the root while a bind holds the tree: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := <-second; !errors.Is(err, store.ErrTreeFull) {
		t.Fatalf("second bind = %v, want ErrTreeFull (12 + 12 MiB > 20 MiB)", err)
	}
	if r := rootOf(t, s, "att_y"); r != nil {
		t.Fatalf("att_y bound to %s", *r)
	}
}

func TestDeleteUnboundAttachment(t *testing.T) {
	s := storetest.New(t)
	newSession(t, s, "ses_a")
	insertSized(t, s, "att_free", 1)
	insertSized(t, s, "att_bound", 1)
	if err := s.BindAttachments(ctx, "chris", "ses_a", []string{"att_bound"}, 20*mib); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUnboundAttachment(ctx, "erin", "att_free"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another person's delete: %v, want ErrNotFound", err)
	}
	if err := s.DeleteUnboundAttachment(ctx, "chris", "att_bound"); !errors.Is(err, store.ErrBound) {
		t.Fatalf("deleting a bound attachment: %v, want ErrBound", err)
	}
	if err := s.DeleteUnboundAttachment(ctx, "chris", "att_free"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attachment(ctx, "att_free"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v, want ErrNotFound", err)
	}
	if err := s.DeleteUnboundAttachment(ctx, "chris", "att_free"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second delete: %v, want ErrNotFound", err)
	}
	if _, err := s.Attachment(ctx, "att_bound"); err != nil {
		t.Fatalf("the bound attachment: %v", err)
	}
}

// InsertAttachment copies the row in with COPY, one encoding of the bytes
// (an INSERT encodes a bytea parameter twice); the defaults still apply.
func TestInsertAttachmentRoundTrip(t *testing.T) {
	s := storetest.New(t)
	w, h := 3, 2
	data := bytes.Repeat([]byte{0xab}, 3*mib)
	in := store.Attachment{ID: "att_rt", PersonID: "chris", MediaType: "image/png", Filename: "kite.png",
		SizeBytes: int64(len(data)), Width: &w, Height: &h, SHA256: "s", Data: data}
	before := time.Now().Add(-time.Minute)
	if err := s.InsertAttachment(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.Attachment(ctx, "att_rt")
	if err != nil {
		t.Fatal(err)
	}
	if got.PersonID != "chris" || got.RootID != nil || got.MediaType != "image/png" || got.Filename != "kite.png" ||
		got.SizeBytes != int64(len(data)) || *got.Width != 3 || *got.Height != 2 || got.Pages != nil ||
		got.SHA256 != "s" || !bytes.Equal(got.Data, data) || got.CreatedAt.Before(before) {
		t.Fatalf("round trip = %+v", got)
	}
	if err := s.InsertAttachment(ctx, in); err == nil {
		t.Fatal("a duplicate id was inserted")
	}
}
