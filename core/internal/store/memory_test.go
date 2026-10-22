package store_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

var errAbort = errors.New("abort")

// A memory transaction's profile change and write row commit together, or
// neither does.
func TestInMemoryTxCommitsTogetherOrNotAtAll(t *testing.T) {
	s := storetest.New(t)
	ses := newSession(t, s, "ses_a")
	change := func(id string, fail bool) error {
		return s.InMemoryTx(ctx, "chris", func(m *store.MemoryTx) error {
			p, err := m.Profile(ctx)
			if err != nil {
				return err
			}
			if _, err := m.SetProfile(ctx, p.Content+"- likes tea\n", p.Version); err != nil {
				return err
			}
			if _, err := m.InsertWrite(ctx, store.MemoryWrite{ID: id, SessionID: &ses.ID, TurnID: "turn_1",
				Op: "remember", Text: "likes tea", Status: "applied"}); err != nil {
				return err
			}
			if fail {
				return errAbort
			}
			return nil
		})
	}
	if err := change("mem_1", true); !errors.Is(err, errAbort) {
		t.Fatalf("InMemoryTx = %v, want fn's error", err)
	}
	if p, err := s.Profile(ctx, "chris"); err != nil || p.Version != 0 || p.Content != "" {
		t.Fatalf("profile after a rolled-back change = %+v, %v; want none", p, err)
	}
	if _, err := s.MemoryWrite(ctx, "mem_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("write after a rolled-back change: %v, want ErrNotFound", err)
	}
	if err := change("mem_2", false); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Profile(ctx, "chris"); err != nil || p.Version != 1 || p.Content != "- likes tea\n" {
		t.Fatalf("profile after a committed change = %+v, %v", p, err)
	}
	if w, err := s.MemoryWrite(ctx, "mem_2"); err != nil || w.PersonID != "chris" || w.DecidedAt == nil {
		t.Fatalf("write after a committed change = %+v, %v; want chris's, decided", w, err)
	}
}

// One person's memory transactions run one at a time; another person's do
// not wait for them.
func TestInMemoryTxSerialisesOnePerson(t *testing.T) {
	s := storetest.New(t)
	su := storetest.ConnAs(t, s, storetest.Superuser)
	waiting := func() int {
		var n int
		if err := su.QueryRow(ctx, `select count(*) from pg_locks where locktype = 'advisory' and not granted`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	hold, held := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.InMemoryTx(ctx, "chris", func(*store.MemoryTx) error {
			close(held)
			<-hold
			return nil
		})
	}()
	<-held
	var entered []string
	second := make(chan error, 1)
	go func() {
		second <- s.InMemoryTx(ctx, "chris", func(*store.MemoryTx) error {
			entered = append(entered, "chris again")
			return nil
		})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for waiting() != 1 {
		if time.Now().After(deadline) {
			close(hold)
			t.Fatal("the second transaction for chris never waited on the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.InMemoryTx(ctx, "erin", func(*store.MemoryTx) error { return nil }); err != nil {
		t.Fatalf("erin's transaction while chris's is held: %v", err)
	}
	if len(entered) != 0 {
		t.Fatalf("entered = %v while the first transaction holds the lock", entered)
	}
	close(hold)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(entered) != "[chris again]" {
		t.Fatalf("entered = %v", entered)
	}
}

// A memory transaction sees and settles only its person's writes, and counts
// the turn's held ones, decided or not (S58). A pending write is held.
func TestMemoryTxScopesWritesToThePerson(t *testing.T) {
	s := storetest.New(t)
	ses := newSession(t, s, "ses_a")
	other := "ses_b"
	newSession(t, s, other)
	for _, w := range []store.MemoryWrite{
		{ID: "mem_e", PersonID: "erin", SessionID: &other, TurnID: "turn_9", Status: "pending"},
		{ID: "mem_1", PersonID: "chris", SessionID: &ses.ID, TurnID: "turn_1", Status: "pending", Matched: []string{"- tea"}},
		{ID: "mem_2", PersonID: "chris", SessionID: &ses.ID, TurnID: "turn_1", Status: "applied"},
		{ID: "mem_3", PersonID: "chris", SessionID: &ses.ID, TurnID: "turn_2", Status: "pending"},
		{ID: "mem_4", PersonID: "chris", SessionID: &other, TurnID: "turn_1", Status: "pending"},
		{ID: "mem_5", PersonID: "chris", SessionID: &ses.ID, TurnID: "turn_1", Status: "pending"},
	} {
		w.Op, w.Text = "forget", "tea"
		if _, err := s.InsertMemoryWrite(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	err := s.InMemoryTx(ctx, "chris", func(m *store.MemoryTx) error {
		held := func(want int) {
			t.Helper()
			if n, err := m.HeldInTurn(ctx, ses.ID, "turn_1"); err != nil || n != want {
				t.Errorf("HeldInTurn = %d, %v; want %d (mem_1, mem_5: held, this session and turn)", n, err, want)
			}
		}
		held(2)
		if _, err := m.Write(ctx, "mem_e"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Write(erin's) = %v, want ErrNotFound", err)
		}
		if _, err := m.DecideWrite(ctx, "mem_e", "applied"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("DecideWrite(erin's) = %v, want ErrNotFound", err)
		}
		if _, err := m.DecideWrite(ctx, "mem_2", "rejected"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("DecideWrite(applied) = %v, want ErrNotFound", err)
		}
		if w, err := m.DecideWrite(ctx, "mem_1", "applied"); err != nil || w.Status != "applied" || !w.Held ||
			!slices.Equal(w.Matched, []string{"- tea"}) || w.DecidedAt == nil {
			t.Errorf("DecideWrite = %+v, %v; want applied, still held, matched kept", w, err)
		}
		held(2) // a decided write still counts
		if w, err := m.DecideWrite(ctx, "mem_3", "rejected"); err != nil || w.Status != "rejected" || w.Matched != nil {
			t.Errorf("DecideWrite(rejected) = %+v, %v; want rejected, matched kept (null)", w, err)
		}
		w, err := m.InsertWrite(ctx, store.MemoryWrite{ID: "mem_6", PersonID: "erin", SessionID: &ses.ID, TurnID: "turn_3",
			Op: "remember", Text: "likes kites", Status: "pending"})
		if err != nil || w.PersonID != "chris" || !w.Held || w.DecidedAt != nil {
			t.Errorf("InsertWrite(pending) = %+v, %v; want chris's (the transaction's person), held, undecided", w, err)
		}
		w, err = m.InsertWrite(ctx, store.MemoryWrite{ID: "mem_7", SessionID: &ses.ID, TurnID: "turn_1",
			Op: "remember", Text: "likes tea", Status: "applied"})
		if err != nil || w.Held || w.DecidedAt == nil {
			t.Errorf("InsertWrite(applied) = %+v, %v; want not held, decided", w, err)
		}
		held(2)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InMemoryTx(ctx, "", func(*store.MemoryTx) error { return nil }); err == nil {
		t.Fatal("InMemoryTx with no person succeeded")
	}
	ws, err := s.PendingMemoryWrites(ctx, "chris")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, w := range ws {
		ids = append(ids, w.ID)
	}
	if !slices.Equal(ids, []string{"mem_4", "mem_5", "mem_6"}) {
		t.Fatalf("PendingMemoryWrites(chris) = %v; want chris's pending writes, oldest first", ids)
	}
}
