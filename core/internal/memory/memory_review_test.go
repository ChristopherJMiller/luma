package memory

import (
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// ---- fail closed ----

// When the taint can't be written (a flagged rpc) or read (an unflagged
// one), the rpc fails and nothing is written: an unknown taint never lets a
// write apply.
func TestTaintFailureFailsTheWriteClosed(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes tea.")
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "hello")
	broken := errors.New("synthetic taint failure")
	for _, c := range []struct {
		name              string
		taintErr, readErr error
		flagged           bool
	}{
		{"Taint fails", broken, nil, true},
		{"Tainted fails", nil, broken, false},
	} {
		h.late.fail(c.taintErr, c.readErr)
		if r, err := h.mem.Remember(bg, sess, turn, "Likes kites.", c.flagged); !errors.Is(err, broken) {
			t.Fatalf("%s: Remember = %+v, %v; want the taint error", c.name, r, err)
		}
		if r, err := h.mem.Forget(bg, sess, turn, "tea", c.flagged); !errors.Is(err, broken) {
			t.Fatalf("%s: Forget = %+v, %v; want the taint error", c.name, r, err)
		}
	}
	h.late.fail(nil, nil)
	if ws := h.writes(erin); len(ws) != 0 {
		t.Fatalf("writes = %+v, want none", ws)
	}
	if p := h.profile(erin); p.Content != "- Likes tea." || p.Version != 1 {
		t.Fatalf("profile = %+v, want untouched", p)
	}
	if evs := h.memoryEvents(sess.ID); len(evs) != 0 {
		t.Fatalf("memory events = %+v, want none", evs)
	}
	if n := h.counted("remember", "error"); n != 2 {
		t.Fatalf("remember errors counted = %v, want 2", n)
	}
}

// ---- metrics ----

// luma_memory_writes_total{op,status} counts every outcome (C7): an rpc's
// result status or error, and a person's decision.
func TestWriteOutcomesAreCounted(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes tea.")
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "hello")
	h.remember(sess, turn, "Likes kites.", false) // remember applied
	h.forget(sess, turn, "ab")                    // forget rejected
	h.forget(sess, turn, "kites")                 // forget applied
	if _, err := h.mem.Remember(bg, sess, turn, "", false); err == nil {
		t.Fatal("an empty remember succeeded") // remember error
	}
	inc := h.session(erin, true)
	h.remember(inc, turn, "Likes kites.", false) // remember disabled

	h.link(sess.ID, turn, fetch())
	keep := h.remember(sess, turn, "Has a dog.", false) // remember pending
	drop := h.forget(sess, turn, "tea")                 // forget pending
	late := h.remember(sess, turn, "Has a cat.", false) // remember pending
	if _, err := h.mem.Decide(bg, erin, keep.WriteID, true); err != nil {
		t.Fatal(err) // remember accepted
	}
	for range 2 { // forget discarded; the second is a retried click, not counted
		if _, err := h.mem.Decide(bg, erin, drop.WriteID, false); err != nil {
			t.Fatal(err)
		}
	}
	h.setProfile(erin, strings.Repeat("x", protocol.MaxProfileBytes))
	if r := h.remember(sess, turn, "A", false); r.Status != "full" {
		t.Fatalf("remember into a full profile = %+v", r) // remember full
	}
	if _, err := h.mem.Decide(bg, erin, late.WriteID, true); !errors.Is(err, ErrFull) {
		t.Fatalf("accept into a full profile = %v", err) // remember full
	}
	for _, c := range []struct {
		op, status string
		want       float64
	}{
		{"remember", "applied", 1}, {"remember", "pending", 2}, {"remember", "disabled", 1},
		{"remember", "error", 1}, {"remember", "full", 2}, {"remember", "accepted", 1},
		{"remember", "discarded", 0},
		{"forget", "applied", 1}, {"forget", "pending", 1}, {"forget", "rejected", 1},
		{"forget", "discarded", 1}, {"forget", "accepted", 0},
	} {
		if got := h.counted(c.op, c.status); got != c.want {
			t.Errorf("luma_memory_writes_total{op=%s,status=%s} = %v, want %v", c.op, c.status, got, c.want)
		}
	}
}
