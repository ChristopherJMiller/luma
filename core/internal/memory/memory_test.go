package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/link"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// ---- taint and hold ----

func TestRememberAppliesWhenUntainted(t *testing.T) {
	h := newHarness(t)
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "I like kites and I have a dog")
	r := h.remember(sess, turn, "Likes kites.", false)
	if r.Status != "applied" || !strings.HasPrefix(r.WriteID, "mem_") || r.Code != "" || r.Removed != nil {
		t.Fatalf("result = %+v, want applied with a write id", r)
	}
	h.remember(sess, turn, "Has a dog named Biscuit.", false)
	if p := h.profile(erin); p.Content != "- Likes kites.\n- Has a dog named Biscuit." || p.Version != 2 {
		t.Fatalf("profile = %q v%d, want two bullets at v2", p.Content, p.Version)
	}
	ws := h.writes(erin)
	if len(ws) != 2 || ws[0].ID != r.WriteID || ws[0].Status != "applied" || ws[0].Untrusted {
		t.Fatalf("writes = %+v, want two applied, trusted", ws)
	}
}

func TestRememberHeldWhenTainted(t *testing.T) {
	h := newHarness(t)
	sess := h.session(erin, false) // read before the taint: Memory must ask the session, not trust a stale row
	turn := h.taintedTurn(erin, sess.ID)
	r := h.remember(sess, turn, "Likes kites.", false)
	if r.Status != "pending" || !strings.HasPrefix(r.WriteID, "mem_") || r.Code != "" {
		t.Fatalf("result = %+v, want pending with a write id", r)
	}
	if p := h.profile(erin); p.Content != "" || p.Version != 0 {
		t.Fatalf("profile = %+v, want untouched", p)
	}
	if w := h.write(r.WriteID); w.Status != "pending" || !w.Untrusted || w.DecidedAt != nil {
		t.Fatalf("write = %+v, want pending, untrusted, undecided", w)
	}
}

// P9: taint is per session, not per turn — injected text stays in context.
func TestRememberHeldTwoTurnsAfterWebFetch(t *testing.T) {
	h := newHarness(t)
	sess := h.session(erin, false)
	h.finish(sess.ID, h.taintedTurn(erin, sess.ID))
	_, turn2 := h.turn(erin, sess.ID, "thanks")
	h.finish(sess.ID, turn2)
	_, turn3 := h.turn(erin, sess.ID, "remember that I like kites")
	if r := h.remember(h.row(sess.ID), turn3, "Likes kites.", false); r.Status != "pending" {
		t.Fatalf("turn 3's remember = %+v, want pending (tainted in turn 1)", r)
	}
	if p := h.profile(erin); p.Content != "" {
		t.Fatalf("profile = %q, want untouched", p.Content)
	}
}

func TestForkOfTaintedSessionHolds(t *testing.T) {
	h := newHarness(t)
	src := h.session(erin, false)
	h.finish(src.ID, h.taintedTurn(erin, src.ID))
	m2, turn2 := h.turn(erin, src.ID, "and another thing")
	h.finish(src.ID, turn2)
	f, err := h.svc.Fork(bg, erin, src.ID, m2.ID)
	if err != nil {
		t.Fatal(err)
	}
	fork := h.row(f.ID)
	if fork.TaintedAt == nil {
		t.Fatal("the fork is not tainted")
	}
	_, turn := h.turn(erin, fork.ID, "remember that I like kites")
	if r := h.remember(fork, turn, "Likes kites.", false); r.Status != "pending" {
		t.Fatalf("remember on the fork = %+v, want pending", r)
	}
	clean := h.session(erin, false)
	_, cturn := h.turn(erin, clean.ID, "remember that I like tea")
	if r := h.remember(clean, cturn, "Likes tea.", false); r.Status != "applied" {
		t.Fatalf("remember on an untainted session = %+v, want applied", r)
	}
}

func TestForgetHeldWhenTainted(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes kites.\n- Likes tea.")
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	r := h.forget(sess, turn, "kites")
	if r.Status != "pending" || r.WriteID == "" || r.Removed != nil {
		t.Fatalf("result = %+v, want pending with a write id", r)
	}
	if p := h.profile(erin); p.Content != "- Likes kites.\n- Likes tea." || p.Version != 1 {
		t.Fatalf("profile = %q v%d, want untouched", p.Content, p.Version)
	}
	w := h.write(r.WriteID)
	if w.Op != "forget" || w.Status != "pending" || !w.Untrusted || !slices.Equal(w.Matched, []string{"- Likes kites."}) {
		t.Fatalf("write = %+v, want a pending untrusted forget matching the kites line", w)
	}
}

// S47: the conductor's untrusted flag taints the session through its actor,
// so the flagged write and every later one are held.
func TestFlaggedWriteTaintsTheSession(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes kites.")
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "hello")
	if r := h.remember(sess, turn, "Likes tea.", true); r.Status != "pending" {
		t.Fatalf("flagged remember = %+v, want pending", r)
	}
	if _, taints, _ := h.late.calls(); taints != 1 {
		t.Fatalf("Taint calls = %d, want 1", taints)
	}
	if h.row(sess.ID).TaintedAt == nil {
		t.Fatal("the flagged rpc did not taint the session")
	}
	if r := h.forget(sess, turn, "kites"); r.Status != "pending" {
		t.Fatalf("unflagged forget after the flag = %+v, want pending", r)
	}
}

// ---- forget limits ----

func TestForgetEmptyOrShortRejected(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- ab cd\n- éé")
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "forget it")
	for _, q := range []string{"", "  ", "ab", " ab\n", "éé"} {
		r := h.forget(sess, turn, q)
		if r.Status != "rejected" || r.Code != protocol.CodeForgetTooShort || r.WriteID != "" {
			t.Fatalf("Forget(%q) = %+v, want rejected forget_too_short, no write", q, r)
		}
	}
	if ws := h.writes(erin); len(ws) != 0 {
		t.Fatalf("writes = %+v, want none for rejected forgets", ws)
	}
	if evs := h.memoryEvents(sess.ID); len(evs) != 0 {
		t.Fatalf("memory events = %+v, want none", evs)
	}
	if r := h.forget(sess, turn, " éé "); r.Status != "rejected" {
		t.Fatalf("Forget(2 runes in 4 bytes) = %+v, want rejected: runes count, not bytes", r)
	}
	if r := h.forget(sess, turn, "  b c  "); r.Status != "applied" || r.Removed == nil || *r.Removed != 1 {
		t.Fatalf("Forget(3 runes after trimming) = %+v, want applied, 1 removed", r)
	}
}

func TestForgetMoreThanFiveMatchesHeld(t *testing.T) {
	h := newHarness(t)
	six := "- tea 1\n- tea 2\n- tea 3\n- tea 4\n- tea 5\n- tea 6\n- coffee"
	h.setProfile(erin, six)
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "forget the tea")
	r := h.forget(sess, turn, "tea")
	if r.Status != "pending" || r.WriteID == "" {
		t.Fatalf("forget matching 6 = %+v, want pending", r)
	}
	w := h.write(r.WriteID)
	want := []string{"- tea 1", "- tea 2", "- tea 3", "- tea 4", "- tea 5", "- tea 6"}
	if w.Untrusted || w.Status != "pending" || !slices.Equal(w.Matched, want) {
		t.Fatalf("write = %+v, want pending, trusted, matched = the six lines", w)
	}
	if p := h.profile(erin); p.Content != six {
		t.Fatalf("profile = %q, want untouched", p.Content)
	}
	h.setProfile(erin, "- tea 1\n- tea 2\n- tea 3\n- tea 4\n- tea 5\n- coffee")
	if r := h.forget(sess, turn, "tea"); r.Status != "applied" || *r.Removed != 5 {
		t.Fatalf("forget matching 5 = %+v, want applied, 5 removed", r)
	}
	if p := h.profile(erin); p.Content != "- coffee" {
		t.Fatalf("profile = %q, want only coffee left", p.Content)
	}
}

func TestForgetRemovesMatchingLinesCaseInsensitive(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, strings.Join([]string{
		"# About me",
		"- Likes TEA in the morning",
		"  * green tea, no sugar",
		"Tea is not a bullet here",
		"- Drinks coffee",
		"+ Steeps tEa for 3 minutes",
		"-tea without a space is not a bullet",
	}, "\n"))
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "forget tea")
	r := h.forget(sess, turn, "Tea")
	if r.Status != "applied" || r.Removed == nil || *r.Removed != 3 {
		t.Fatalf("result = %+v, want applied, 3 removed", r)
	}
	if p := h.profile(erin); p.Content != "# About me\nTea is not a bullet here\n- Drinks coffee\n-tea without a space is not a bullet" || p.Version != 2 {
		t.Fatalf("profile = %q v%d", p.Content, p.Version)
	}
	w := h.write(r.WriteID)
	if !slices.Equal(w.Matched, []string{"- Likes TEA in the morning", "  * green tea, no sugar", "+ Steeps tEa for 3 minutes"}) {
		t.Fatalf("matched = %q, want the three removed lines", w.Matched)
	}
	// The match is against the fact, not its bullet marker; no match removes
	// nothing and leaves the version alone.
	if r := h.forget(sess, turn, "- Drinks"); r.Status != "applied" || *r.Removed != 0 {
		t.Fatalf("forget of a marker = %+v, want applied, 0 removed", r)
	}
	if p := h.profile(erin); p.Version != 2 {
		t.Fatalf("version after removing nothing = %d, want 2", p.Version)
	}
}

// ---- remember text ----

// A remember is one bullet line: whitespace runs (newlines included) become
// one space. Over 300 characters, or nothing at all, is a broken request
// (the conductor's schema caps it), so no write happens.
func TestRememberIsOneLine(t *testing.T) {
	h := newHarness(t)
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "remember")
	if r := h.remember(sess, turn, "  Likes\nkites,\r\n\tand  tea too ", false); r.Status != "applied" {
		t.Fatalf("result = %+v", r)
	}
	if p := h.profile(erin); p.Content != "- Likes kites, and tea too" {
		t.Fatalf("profile = %q, want one bullet line", p.Content)
	}
	if r := h.remember(sess, turn, strings.Repeat("é", 300), false); r.Status != "applied" {
		t.Fatalf("300 characters = %+v, want applied", r)
	}
	for _, text := range []string{strings.Repeat("a", 301), "", " \n\t "} {
		if r, err := h.mem.Remember(bg, sess, turn, text, false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Remember(%d chars) = %+v, %v; want ErrInvalid", len(text), r, err)
		}
	}
	if ws := h.writes(erin); len(ws) != 2 {
		t.Fatalf("writes = %d, want 2 (none for broken requests)", len(ws))
	}
}

// ---- pending ----

func TestPendingLimitPerTurn(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes tea.")
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	for i := range 3 {
		if r := h.remember(sess, turn, fmt.Sprintf("Fact %d.", i), false); r.Status != "pending" {
			t.Fatalf("held write %d = %+v, want pending", i+1, r)
		}
	}
	limit := protocol.MemoryResult{Status: "rejected", Code: protocol.CodeMemoryPendingLimit}
	if r := h.remember(sess, turn, "Fact 4.", false); r != limit {
		t.Fatalf("4th held remember = %+v, want %+v", r, limit)
	}
	if r := h.forget(sess, turn, "tea"); r != limit {
		t.Fatalf("4th held forget = %+v, want %+v", r, limit)
	}
	if ws := h.writes(erin); len(ws) != 3 {
		t.Fatalf("writes = %d, want 3 (no row for a refused one)", len(ws))
	}
	if evs := h.memoryEvents(sess.ID); len(evs) != 3 {
		t.Fatalf("memory events = %d, want 3", len(evs))
	}
	h.finish(sess.ID, turn)
	_, next := h.turn(erin, sess.ID, "again")
	if r := h.remember(sess, next, "Fact 5.", false); r.Status != "pending" {
		t.Fatalf("a held write in the next turn = %+v, want pending (the limit is per turn)", r)
	}

	// Writes that apply don't count: only held ones do.
	h.setProfile(erin, "- tea 1\n- tea 2\n- tea 3\n- tea 4\n- tea 5\n- tea 6")
	clean := h.session(erin, false)
	_, cturn := h.turn(erin, clean.ID, "forget tea")
	for i := range 3 {
		if r := h.forget(clean, cturn, "tea"); r.Status != "pending" {
			t.Fatalf("broad forget %d = %+v, want pending", i+1, r)
		}
	}
	if r := h.forget(clean, cturn, "tea"); r != limit {
		t.Fatalf("4th broad forget = %+v, want %+v", r, limit)
	}
	if r := h.remember(clean, cturn, "Likes kites.", false); r.Status != "applied" {
		t.Fatalf("an untainted remember past the limit = %+v, want applied", r)
	}
}

// Concurrent held writes of one turn cannot slip past the limit.
func TestConcurrentHeldWritesKeepTheLimit(t *testing.T) {
	h := newHarness(t)
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	var wg sync.WaitGroup
	results := make([]protocol.MemoryResult, 8)
	errs := make([]error, 8)
	for i := range 8 {
		wg.Go(func() {
			results[i], errs[i] = h.mem.Remember(bg, sess, turn, fmt.Sprintf("Fact %d.", i), false)
		})
	}
	wg.Wait()
	pending, refused := 0, 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		switch {
		case r.Status == "pending":
			pending++
		case r.Status == "rejected" && r.Code == protocol.CodeMemoryPendingLimit:
			refused++
		}
	}
	if pending != 3 || refused != 5 {
		t.Fatalf("pending = %d, refused = %d; want 3 and 5", pending, refused)
	}
}

func TestAcceptPendingApplies(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes tea.")
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	rem := h.remember(sess, turn, "Likes kites.", false)
	fgt := h.forget(sess, turn, "tea")
	if rem.Status != "pending" || fgt.Status != "pending" {
		t.Fatalf("held writes = %+v, %+v", rem, fgt)
	}
	h.setProfile(erin, "- Likes tea.\n- Iced tea in summer") // the person edits while they wait

	w, err := h.mem.Decide(bg, erin, rem.WriteID, true)
	if err != nil || w.Status != "applied" || w.DecidedAt == nil || w.Op != "remember" {
		t.Fatalf("accept remember = %+v, %v; want applied", w, err)
	}
	if p := h.profile(erin); p.Content != "- Likes tea.\n- Iced tea in summer\n- Likes kites." {
		t.Fatalf("profile after the remember = %q", p.Content)
	}
	w, err = h.mem.Decide(bg, erin, fgt.WriteID, true)
	if err != nil || w.Status != "applied" {
		t.Fatalf("accept forget = %+v, %v; want applied", w, err)
	}
	// Accepting a forget removes the lines that match now.
	if p := h.profile(erin); p.Content != "- Likes kites." {
		t.Fatalf("profile after the forget = %q, want both tea lines gone", p.Content)
	}
	if !slices.Equal(w.Matched, []string{"- Likes tea.", "- Iced tea in summer"}) {
		t.Fatalf("matched = %q, want what the accepted forget removed", w.Matched)
	}
	// The same decision again is a retried click: the write comes back as is.
	if again, err := h.mem.Decide(bg, erin, rem.WriteID, true); err != nil || again.Status != "applied" || !again.DecidedAt.Equal(*h.write(rem.WriteID).DecidedAt) {
		t.Fatalf("accept again = %+v, %v; want the applied write unchanged", again, err)
	}
	if p := h.profile(erin); p.Content != "- Likes kites." {
		t.Fatalf("profile after a repeated accept = %q, want unchanged", p.Content)
	}
	if _, err := h.mem.Decide(bg, erin, rem.WriteID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reject after accept = %v, want ErrNotFound (nothing pending to decide)", err)
	}
}

func TestRejectPendingLeavesProfile(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes tea.")
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	rem := h.remember(sess, turn, "Likes kites.", false)
	fgt := h.forget(sess, turn, "tea")
	for _, id := range []string{rem.WriteID, fgt.WriteID} {
		w, err := h.mem.Decide(bg, erin, id, false)
		if err != nil || w.Status != "rejected" || w.DecidedAt == nil {
			t.Fatalf("reject = %+v, %v", w, err)
		}
	}
	if p := h.profile(erin); p.Content != "- Likes tea." || p.Version != 1 {
		t.Fatalf("profile = %q v%d, want untouched", p.Content, p.Version)
	}
	if w := h.write(fgt.WriteID); !slices.Equal(w.Matched, []string{"- Likes tea."}) {
		t.Fatalf("a rejected forget's matched = %q, want kept as held", w.Matched)
	}
	if _, err := h.mem.Decide(bg, erin, rem.WriteID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accept after reject = %v, want ErrNotFound", err)
	}
}

// A pending remember that no longer fits is not applied: it stays pending
// until the person makes room.
func TestAcceptOfAFullRememberStaysPending(t *testing.T) {
	h := newHarness(t)
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	rem := h.remember(sess, turn, "Likes kites.", false)
	full := strings.Repeat("x", protocol.MaxProfileBytes-5)
	h.setProfile(erin, full)
	if _, err := h.mem.Decide(bg, erin, rem.WriteID, true); !errors.Is(err, ErrFull) {
		t.Fatalf("accept into a full profile = %v, want ErrFull", err)
	}
	if w := h.write(rem.WriteID); w.Status != "pending" {
		t.Fatalf("write = %+v, want still pending", w)
	}
	if p := h.profile(erin); p.Content != full {
		t.Fatal("the profile changed")
	}
}

// ---- ownership ----

func TestDecideOtherPersonsWrite404(t *testing.T) {
	w := newWeb(t)
	h := w.harness
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	rem := h.remember(sess, turn, "Likes kites.", false)
	for _, accept := range []bool{true, false} {
		if _, err := h.mem.Decide(bg, chris, rem.WriteID, accept); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Decide(chris, erin's write, %v) = %v, want ErrNotFound", accept, err)
		}
	}
	c := w.login(chris)
	for _, path := range []string{"/accept", "/reject"} {
		w.errorOf(w.do("POST", "/v1/memory/writes/"+rem.WriteID+path, c, ""), 404, protocol.CodeNotFound)
	}
	w.errorOf(w.do("POST", "/v1/memory/writes/mem_01J9ZQ3M5K7N8P2R4T6V8W0XYZ/accept", c, ""), 404, protocol.CodeNotFound)
	if got := h.write(rem.WriteID); got.Status != "pending" {
		t.Fatalf("erin's write = %+v, want still pending", got)
	}
	if p := h.profile(erin); p.Version != 0 {
		t.Fatalf("erin's profile = %+v, want untouched", p)
	}
	if p := w.profileOf(w.do("GET", "/v1/memory/profile", c, "")); len(p.Pending) != 0 {
		t.Fatalf("chris's pending = %+v, want none of erin's", p.Pending)
	}
}

// ---- disabled ----

// Incognito (or an agent without person memory) reads and writes nothing:
// no profile read, no taint, no row, no event. A cancelled context proves
// no query runs.
func TestIncognitoNoReadNoWrite(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes tea.")
	inc := h.session(erin, true)
	if inc.ProfileSnapshot != "" {
		t.Fatalf("incognito profile_snapshot = %q, want \"\"", inc.ProfileSnapshot)
	}
	_, turn := h.turn(erin, inc.ID, "remember that I like kites")
	none := h.session(erin, false)
	var spec map[string]any
	if err := json.Unmarshal(none.AgentSnapshot, &spec); err != nil {
		t.Fatal(err)
	}
	spec["memory"] = "none"
	none.AgentSnapshot, _ = json.Marshal(spec)

	dead, cancel := context.WithCancel(bg)
	cancel()
	want := protocol.MemoryResult{Status: "disabled", Code: protocol.CodeMemoryDisabled}
	for _, sess := range []store.Session{inc, none} {
		for _, flagged := range []bool{false, true} {
			if r, err := h.mem.Remember(dead, sess, turn, "Likes kites.", flagged); err != nil || r != want {
				t.Fatalf("Remember = %+v, %v; want %+v", r, err, want)
			}
			if r, err := h.mem.Forget(dead, sess, turn, "tea", flagged); err != nil || r != want {
				t.Fatalf("Forget = %+v, %v; want %+v", r, err, want)
			}
		}
	}
	if emits, taints, queries := h.late.calls(); len(emits) != 0 || taints != 0 || queries != 0 {
		t.Fatalf("emits %v, Taint %d, Tainted %d; want nothing asked of the session", emits, taints, queries)
	}
	if ws := h.writes(erin); len(ws) != 0 {
		t.Fatalf("writes = %+v, want none", ws)
	}
	if p := h.profile(erin); p.Content != "- Likes tea." || p.Version != 1 {
		t.Fatalf("profile = %+v, want untouched", p)
	}
	if h.row(inc.ID).TaintedAt != nil {
		t.Fatal("a flagged rpc in an incognito session tainted it")
	}
}

// ---- caps ----

func TestProfileCap6000Bytes(t *testing.T) {
	w := newWeb(t)
	h := w.harness
	c := w.login(erin)
	// The editor: 6000 bytes is the cap, counted in UTF-8 bytes ("é" is 2).
	atCap := strings.Repeat("é", protocol.MaxProfileBytes/2)
	p := w.profileOf(w.do("PUT", "/v1/memory/profile", c, putBody(atCap, 0)))
	if p.Content != atCap || p.Version != 1 {
		t.Fatalf("PUT of 6000 bytes = %d bytes v%d, want saved", len(p.Content), p.Version)
	}
	w.errorOf(w.do("PUT", "/v1/memory/profile", c, putBody(atCap+"x", 1)), 413, protocol.CodeRequestTooLarge)
	if p := h.profile(erin); p.Version != 1 {
		t.Fatalf("version after a refused PUT = %d, want 1", p.Version)
	}

	// remember: "\n- Likes kites." is 15 bytes, so 5985 bytes leave room for
	// exactly it.
	h.setProfile(erin, strings.Repeat("x", protocol.MaxProfileBytes-15))
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "remember")
	if r := h.remember(sess, turn, "Likes kites.", false); r.Status != "applied" {
		t.Fatalf("remember to exactly 6000 bytes = %+v, want applied", r)
	}
	if p := h.profile(erin); len(p.Content) != protocol.MaxProfileBytes {
		t.Fatalf("profile = %d bytes, want 6000", len(p.Content))
	}
	before := len(h.writes(erin))
	full := protocol.MemoryResult{Status: "full", Code: protocol.CodeMemoryFull}
	if r := h.remember(sess, turn, "A", false); r != full {
		t.Fatalf("remember past the cap = %+v, want %+v", r, full)
	}
	h.link(sess.ID, turn, fetch())
	if r := h.remember(sess, turn, "A", false); r != full {
		t.Fatalf("held remember past the cap = %+v, want %+v (not held)", r, full)
	}
	if n := len(h.writes(erin)); n != before {
		t.Fatalf("writes = %d, want %d (no row for a full one)", n, before)
	}
	if p := h.profile(erin); len(p.Content) != protocol.MaxProfileBytes {
		t.Fatal("the profile changed")
	}
}

// ---- provenance and events ----

func TestProvenanceRecorded(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes tea.")
	sess := h.session(erin, false)
	_, turn1 := h.turn(erin, sess.ID, "remember kites")
	start := time.Now().Add(-time.Minute)
	rem := h.remember(sess, turn1, "  Likes   kites.  ", false)
	h.finish(sess.ID, turn1)
	turn2 := h.taintedTurn(erin, sess.ID)
	fgt := h.forget(sess, turn2, "tea ")

	r := h.write(rem.WriteID)
	if r.PersonID != erin.ID || r.SessionID == nil || *r.SessionID != sess.ID || r.TurnID != turn1 ||
		r.Op != "remember" || r.Text != "  Likes   kites.  " || r.Matched != nil || r.Untrusted ||
		r.Status != "applied" || r.CreatedAt.Before(start) || r.DecidedAt == nil {
		t.Fatalf("remember row = %+v", r)
	}
	f := h.write(fgt.WriteID)
	if f.PersonID != erin.ID || f.SessionID == nil || *f.SessionID != sess.ID || f.TurnID != turn2 ||
		f.Op != "forget" || f.Text != "tea " || !slices.Equal(f.Matched, []string{"- Likes tea."}) || !f.Untrusted ||
		f.Status != "pending" || f.CreatedAt.Before(start) || f.DecidedAt != nil {
		t.Fatalf("forget row = %+v", f)
	}
	if p := h.profile(erin); p.Content != "- Likes tea.\n- Likes kites." {
		t.Fatalf("profile = %q", p.Content)
	}
}

// memory.write goes through the session's actor (the sink), so a live
// subscriber sees it in order, on create and on every status change.
func TestEventsGoThroughSink(t *testing.T) {
	h := newHarness(t)
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	since, err := h.st.LastEventID(bg, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	live, err := h.svc.Subscribe(ctx, erin, sess.ID, since)
	if err != nil {
		t.Fatal(err)
	}
	next := func() protocol.MemoryWrite {
		t.Helper()
		for {
			select {
			case ev, ok := <-live:
				if !ok {
					t.Fatal("the subscription closed")
				}
				if b, ok := ev.Body.(protocol.MemoryWriteEvent); ok {
					return b.Write
				}
			case <-ctx.Done():
				t.Fatal("no memory.write event arrived")
			}
		}
	}
	rem := h.remember(sess, turn, "Likes kites.", false)
	if ev := next(); ev.ID != rem.WriteID || ev.Status != "pending" || ev.Text != "Likes kites." || ev.Op != "remember" {
		t.Fatalf("event on create = %+v", ev)
	}
	if _, err := h.mem.Decide(bg, erin, rem.WriteID, true); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.ID != rem.WriteID || ev.Status != "applied" || ev.DecidedAt == nil {
		t.Fatalf("event on accept = %+v", ev)
	}
	if emits, _, _ := h.late.calls(); !slices.Equal(emits, []string{sess.ID + ":memory.write", sess.ID + ":memory.write"}) {
		t.Fatalf("sink calls = %v, want two memory.write emits", emits)
	}
	if evs := h.memoryEvents(sess.ID); len(evs) != 2 {
		t.Fatalf("memory.write events in the log = %d, want 2", len(evs))
	}
}

// A write whose conversation was deleted is still the person's to decide;
// it has no log to emit to.
func TestDecideAfterTheConversationIsDeleted(t *testing.T) {
	h := newHarness(t)
	sess := h.session(erin, false)
	turn := h.taintedTurn(erin, sess.ID)
	rem := h.remember(sess, turn, "Likes kites.", false)
	h.finish(sess.ID, turn)
	if err := h.svc.DeleteTree(bg, erin.ID, sess.RootID); err != nil {
		t.Fatal(err)
	}
	w, err := h.mem.Decide(bg, erin, rem.WriteID, true)
	if err != nil || w.Status != "applied" || w.SessionID != nil {
		t.Fatalf("accept = %+v, %v; want applied, session_id null", w, err)
	}
	if p := h.profile(erin); p.Content != "- Likes kites." {
		t.Fatalf("profile = %q", p.Content)
	}
	if emits, _, _ := h.late.calls(); len(emits) != 1 {
		t.Fatalf("sink calls = %v, want only the create's", emits)
	}
}

// ---- the rpc, routed by the link ----

// The conductor's rpc reaches Memory through link.MemoryRPC with the
// session row's person, and its untrusted flag taints the session (S47).
func TestMemoryRPCThroughTheLink(t *testing.T) {
	h := newHarness(t)
	h.setProfile(erin, "- Likes tea.")
	rpc := link.MemoryRPC(h.st, h.mem)
	sess := h.session(erin, false)
	_, turn := h.turn(erin, sess.ID, "hello")
	r, err := rpc(bg, sess.ID, turn, "memory.remember", json.RawMessage(`{"text":"Likes kites.","untrusted":false}`))
	if err != nil || r.Status != "applied" {
		t.Fatalf("remember rpc = %+v, %v", r, err)
	}
	r, err = rpc(bg, sess.ID, turn, "memory.forget", json.RawMessage(`{"text":"tea","untrusted":true}`))
	if err != nil || r.Status != "pending" {
		t.Fatalf("flagged forget rpc = %+v, %v; want pending", r, err)
	}
	if h.row(sess.ID).TaintedAt == nil {
		t.Fatal("the flagged rpc did not taint the session")
	}
	if p := h.profile(erin); p.Content != "- Likes tea.\n- Likes kites." {
		t.Fatalf("profile = %q", p.Content)
	}
}
