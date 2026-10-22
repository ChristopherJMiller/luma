package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// ---- helpers ----

// exchange runs one whole turn on a session: the user message, then the
// assistant's start and its turn_result with sdkUUID. It returns both stored
// messages.
func (h *harness) exchange(p auth.Person, sessionID, text, sdkUUID string) (user, asst protocol.Message) {
	h.t.Helper()
	user = h.send(p, sessionID, text)
	h.link(sessionID, protocol.LinkMessageStarted{Model: haiku}, result(sdkUUID, "re: "+text, 0.01))
	all := h.messages(sessionID, "assistant")
	asst = all[len(all)-1]
	if asst.SDKUUID == nil || *asst.SDKUUID != sdkUUID {
		h.t.Fatalf("assistant message %+v, want sdk_uuid %s", asst, sdkUUID)
	}
	return user, asst
}

// ready reports the session's host ready with its SDK session id, as the
// conductor does once the SDK reports it (C2).
func (h *harness) ready(sessionID, sdkSessionID string) {
	h.t.Helper()
	h.hostState(protocol.HostState{SessionID: sessionID, State: "ready", SDKSessionID: sdkSessionID})
}

// forkAt forks a session at a message and fails the test on error.
func (h *harness) forkAt(p auth.Person, sessionID, atMessageID string) protocol.Session {
	h.t.Helper()
	f, err := h.svc.Fork(bg, p, sessionID, atMessageID)
	if err != nil {
		h.t.Fatalf("Fork(%s at %s): %v", sessionID, atMessageID, err)
	}
	return f
}

// openOf returns the session's newest host.open spec.
func (h *harness) openOf(sessionID string) protocol.HostSpec {
	h.t.Helper()
	opens := h.cond.Opens()
	for i := len(opens) - 1; i >= 0; i-- {
		if opens[i].SessionID == sessionID {
			return opens[i]
		}
	}
	h.t.Fatalf("no host.open for %s", sessionID)
	return protocol.HostSpec{}
}

// chain is the history that ends at a message: the ids from the first
// message down to id, following parent_message_id across sessions.
func (h *harness) chain(id string) []string {
	h.t.Helper()
	var out []string
	for next := &id; next != nil; {
		m, err := h.st.Message(bg, *next)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append([]string{m.ID}, out...)
		next = m.ParentMessageID
	}
	return out
}

// lastEventID is the id of the session's newest stored event.
func (h *harness) lastEventID(sessionID string) int64 {
	h.t.Helper()
	evs := h.events(sessionID, "")
	if len(evs) == 0 {
		return 0
	}
	return evs[len(evs)-1].ID
}

func (h *harness) tree(p auth.Person, rootID string) protocol.Tree {
	h.t.Helper()
	tr, err := h.svc.Tree(bg, p, rootID)
	if err != nil {
		h.t.Fatalf("Tree(%s): %v", rootID, err)
	}
	return tr
}

func str(s string) *string { return &s }

// ---- Fork ----

// P4: the fork's history is everything before M. Its first message follows
// the last message before M, and its first open resumes the owner of that
// assistant message at its sdk_uuid; once the SDK reports the fork's own
// session, later opens resume that, with no fork.
func TestForkHistoryEndsBeforeMessage(t *testing.T) {
	h := newHarness(t)
	root := h.newSession(family)
	u1, a1 := h.exchange(family, root.ID, "one", "uuid-a1")
	h.ready(root.ID, "sdk-root")
	u2, _ := h.exchange(family, root.ID, "two", "uuid-a2")
	h.exchange(family, root.ID, "three", "uuid-a3")
	rootTotals := h.row(root.ID).LastTotals

	f := h.forkAt(family, root.ID, u2.ID)
	if f.ID == root.ID || f.RootID != root.ID || f.ParentID == nil || *f.ParentID != root.ID ||
		f.ForkAtMessageID == nil || *f.ForkAtMessageID != u2.ID || f.Status != "idle" {
		t.Fatalf("fork = %+v; want a new idle session of the tree, parent %s, at %s", f, root.ID, u2.ID)
	}
	if row := h.row(f.ID); row.SDKSessionID != nil || row.LastTotals != nil {
		t.Fatalf("fork row sdk_session_id = %v, last_totals = %+v; want neither (the SDK has not seen it)", row.SDKSessionID, row.LastTotals)
	}
	if n := len(h.events(f.ID, "")); n != 0 {
		t.Fatalf("the fork has %d events before its first turn, want 0", n)
	}

	first := h.send(family, f.ID, "two, edited")
	if first.ParentMessageID == nil || *first.ParentMessageID != a1.ID {
		t.Fatalf("first message parent = %v, want the last message before M (%s)", first.ParentMessageID, a1.ID)
	}
	if got, want := h.chain(first.ID), []string{u1.ID, a1.ID, first.ID}; !slices.Equal(got, want) {
		t.Fatalf("fork history = %v, want %v", got, want)
	}
	want := &protocol.Resume{SDKSessionID: "sdk-root", Fork: &protocol.ResumeFork{AtSDKUUID: "uuid-a1"}, BaselineTotals: rootTotals}
	if got := h.openOf(f.ID).Resume; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork's first open resume = %+v, want %+v", got, want)
	}
	send := h.lastSend()
	if send.SessionID != f.ID || len(send.Content) != 2 || send.Content[0].Text != "two, edited" {
		t.Fatalf("turn.send = %+v; want only the edited message (and <luma-now>)", send)
	}
	if all, err := h.st.Messages(bg, root.ID); err != nil || len(all) != 6 {
		t.Fatalf("the source has %d messages after the fork (%v), want its 6", len(all), err)
	}

	h.ready(f.ID, "sdk-fork")
	h.link(f.ID, protocol.LinkMessageStarted{Model: haiku}, result("uuid-f1", "re: edited", 0.01))
	h.hostState(protocol.HostState{SessionID: f.ID, State: "closed", Reason: "idle"})
	fa := h.messages(f.ID, "assistant")[0]
	second := h.send(family, f.ID, "and then?")
	if second.ParentMessageID == nil || *second.ParentMessageID != fa.ID {
		t.Fatalf("second message parent = %v, want the fork's own answer %s", second.ParentMessageID, fa.ID)
	}
	if got := h.openOf(f.ID).Resume; got == nil || got.SDKSessionID != "sdk-fork" || got.Fork != nil {
		t.Fatalf("fork's reopen resume = %+v, want the fork's own sdk session and no fork", got)
	}
}

// The fork copies its source's snapshot (agent, model, helper model, effort,
// TTL, workspaces, source, bound models, title) and its profile_snapshot byte
// for byte; neither the live catalog nor the live profile is read again.
func TestForkKeepsModelAndSnapshotBytes(t *testing.T) {
	h := newHarness(t)
	spec, err := h.live.Compile("luma", "fast")
	if err != nil {
		t.Fatal(err)
	}
	spec.Version, spec.SystemPrompt, spec.TierBinding.Model = "v-old", "An older synthetic prompt.", "claude-old-5-0"
	snapshot, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	profile := "<luma-context>\n  Likes kites\t& <b>tea</b> é\r\n</luma-context>\n"
	src, err := h.st.CreateSession(bg, store.Session{
		ID: newID("ses_", testNow), PersonID: family.ID, AgentID: "luma", AgentVersion: "v-old", AgentSnapshot: snapshot,
		Tier: "smart", Model: "claude-old-5-0", HelperModel: "claude-old-helper", Effort: "high", CacheTTL: "5m",
		APIWorkspace: "ci", Source: "ci", BoundModels: []string{"claude-old-5-0", "claude-old-helper", "claude-old-sub"},
		Title: "Kites and tea", ProfileSnapshot: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	u1, err := h.st.InsertMessage(bg, protocol.Message{ID: newID("msg_", testNow), SessionID: src.ID, TurnID: "turn_old",
		Role: "user", Content: []protocol.MessagePart{{Type: "text", Text: "kites?"}}, Status: "complete"})
	if err != nil {
		t.Fatal(err)
	}
	h.prof.text = "<luma-context>\na newer profile\n</luma-context>"
	calls := h.prof.Calls()

	f := h.forkAt(family, src.ID, u1.ID)
	row := h.row(f.ID)
	if !bytes.Equal(row.AgentSnapshot, src.AgentSnapshot) || row.ProfileSnapshot != profile {
		t.Fatalf("fork snapshots = %s / %q; want the source's bytes %s / %q", row.AgentSnapshot, row.ProfileSnapshot, src.AgentSnapshot, profile)
	}
	if row.AgentID != "luma" || row.AgentVersion != "v-old" || row.Tier != "smart" || row.Model != "claude-old-5-0" ||
		row.HelperModel != "claude-old-helper" || row.Effort != "high" || row.CacheTTL != "5m" || row.Workspace != "none" ||
		row.APIWorkspace != "ci" || row.Source != "ci" || row.Incognito || row.Title != "Kites and tea" ||
		!slices.Equal(row.BoundModels, src.BoundModels) || row.PersonID != family.ID {
		t.Fatalf("fork row = %+v; want the source's settings", row)
	}
	if h.prof.Calls() != calls {
		t.Error("Fork read the live profile; the fork keeps its source's snapshot")
	}

	h.send(family, f.ID, "kites, again")
	open := h.openOf(f.ID)
	if open.Model != "claude-old-5-0" || open.HelperModel != "claude-old-helper" || open.Effort != "high" ||
		open.CacheTTL != "5m" || open.Workspace != "ci" || open.Source != "ci" || open.ProfileContext != profile ||
		open.Agent.Version != "v-old" || open.Agent.SystemPrompt != "An older synthetic prompt." {
		t.Fatalf("fork's host.open = %+v; want the source's snapshot", open)
	}
	if got := h.row(f.ID).Title; got != "Kites and tea" {
		t.Errorf("fork title after its first message = %q, want the copied title", got)
	}

	yes := true
	incog := h.create(family, protocol.CreateSession{Agent: "luma", Params: protocol.SessionParams{Incognito: &yes}})
	iu, _ := h.exchange(family, incog.ID, "a secret", "uuid-i1")
	if fi := h.forkAt(family, incog.ID, iu.ID); !fi.Incognito || h.row(fi.ID).ProfileSnapshot != "" {
		t.Fatalf("fork of an incognito session = %+v; want incognito with no profile", fi)
	}
}

// When M is the first message the fork is a fresh session: no resume, and
// its first message has no parent.
func TestForkAtFirstMessageIsFresh(t *testing.T) {
	h := newHarness(t)
	root := h.newSession(family)
	u1, _ := h.exchange(family, root.ID, "one", "uuid-a1")
	h.ready(root.ID, "sdk-root")
	h.exchange(family, root.ID, "two", "uuid-a2")

	f := h.forkAt(family, root.ID, u1.ID)
	first := h.send(family, f.ID, "one, edited")
	if first.ParentMessageID != nil {
		t.Fatalf("first message parent = %v, want none", *first.ParentMessageID)
	}
	if got := h.openOf(f.ID).Resume; got != nil {
		t.Fatalf("resume = %+v, want null (a fresh session)", got)
	}
}

// The fork point's owner is the session that holds that assistant message,
// whichever session is forked.
func TestForkOfForkResolvesOwnerOfAssistantMessage(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	_, a1 := h.exchange(family, r.ID, "one", "r-1")
	h.ready(r.ID, "sdk-r")
	u2, _ := h.exchange(family, r.ID, "two", "r-2")

	f1 := h.forkAt(family, r.ID, u2.ID)
	u2e := h.send(family, f1.ID, "two, edited")
	h.ready(f1.ID, "sdk-f1")
	h.link(f1.ID, protocol.LinkMessageStarted{Model: haiku}, result("f1-2", "re: two, edited", 0.01))
	u3, _ := h.exchange(family, f1.ID, "three", "f1-3")
	rTotals, f1Totals := h.row(r.ID).LastTotals, h.row(f1.ID).LastTotals

	// At a message of f1 after f1's own turn: f1 owns the fork point.
	f2 := h.forkAt(family, f1.ID, u3.ID)
	f2first := h.send(family, f2.ID, "three, edited")
	if f2.RootID != r.ID || *f2.ParentID != f1.ID {
		t.Fatalf("fork of a fork = %+v; want root %s, parent %s", f2, r.ID, f1.ID)
	}
	want := &protocol.Resume{SDKSessionID: "sdk-f1", Fork: &protocol.ResumeFork{AtSDKUUID: "f1-2"}, BaselineTotals: f1Totals}
	if got := h.openOf(f2.ID).Resume; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork at f1's U3: resume = %+v, want %+v", got, want)
	}

	// At f1's first message: the last assistant message before it is the
	// root's, so the root is resumed, not f1.
	f3 := h.forkAt(family, f1.ID, u2e.ID)
	f3first := h.send(family, f3.ID, "two, edited again")
	want = &protocol.Resume{SDKSessionID: "sdk-r", Fork: &protocol.ResumeFork{AtSDKUUID: "r-1"}, BaselineTotals: rTotals}
	if got := h.openOf(f3.ID).Resume; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork at f1's first message: resume = %+v, want %+v", got, want)
	}
	if f3first.ParentMessageID == nil || *f3first.ParentMessageID != a1.ID {
		t.Fatalf("its first message parent = %v, want the root's A1", f3first.ParentMessageID)
	}

	// From f2, at f1's message that f2 inherited: still the root's A1.
	f4 := h.forkAt(family, f2.ID, u2e.ID)
	h.send(family, f4.ID, "two, a third way")
	if got := h.openOf(f4.ID).Resume; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork of f2 at an inherited message: resume = %+v, want %+v", got, want)
	}
	if got := h.chain(f2first.ID); len(got) != 5 || got[1] != a1.ID || got[2] != u2e.ID || got[4] != f2first.ID {
		t.Fatalf("f2 history = %v; want U1 A1 U2' A2' U3'", got)
	}
}

// A turn with two API messages and a tool is one assistant message whose
// sdk_uuid (the turn's last main-loop entry) is the fork point (P23, S8).
func TestForkAfterToolTurnUsesTurnSdkUUID(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	h.send(family, r.ID, "look it up")
	h.ready(r.ID, "sdk-r")
	h.link(r.ID,
		protocol.LinkMessageStarted{Model: haiku},
		protocol.MainRequestUsage{APIMessageID: "msg_api_1", Model: haiku, SLOExclusion: "first", Input: 10},
		protocol.LinkToolStarted{ToolUseID: "tu1", Name: "WebSearch", Summary: "kites"},
		protocol.LinkToolFinished{ToolUseID: "tu1", Summary: "3 results"},
		protocol.MainRequestUsage{APIMessageID: "msg_api_2", Model: haiku, SLOExclusion: "none", Input: 20},
		protocol.LinkTextDelta{Text: "Found it."},
		result("uuid-turn1-last", "Found it.", 0.02))
	if n := len(h.messages(r.ID, "assistant")); n != 1 {
		t.Fatalf("the tool turn stored %d assistant messages, want 1 (P23)", n)
	}
	u2, _ := h.exchange(family, r.ID, "and the second?", "uuid-turn2-last")

	f := h.forkAt(family, r.ID, u2.ID)
	h.send(family, f.ID, "and the other one?")
	got := h.openOf(f.ID).Resume
	if got == nil || got.SDKSessionID != "sdk-r" || got.Fork == nil || got.Fork.AtSDKUUID != "uuid-turn1-last" {
		t.Fatalf("resume = %+v; want sdk-r at the turn's sdk_uuid uuid-turn1-last", got)
	}
}

// The fork-point lookup skips assistant messages without an sdk_uuid (a turn
// that never reached a result); with nothing resumable before M the fork is
// fresh. The first message still follows the last message before M.
func TestForkSkipsAssistantWithoutSdkUUID(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	h.exchange(family, r.ID, "one", "uuid-a1")
	h.ready(r.ID, "sdk-r")
	h.send(family, r.ID, "two")
	h.link(r.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkTextDelta{Text: "partial"})
	h.hostState(protocol.HostState{SessionID: r.ID, State: "closed", Reason: "error"})
	failed := h.messages(r.ID, "assistant")[1]
	if failed.Status != "error" || failed.SDKUUID != nil {
		t.Fatalf("failed turn's message = %+v; want error with no sdk_uuid", failed)
	}
	u3, _ := h.exchange(family, r.ID, "three", "uuid-a3")

	f := h.forkAt(family, r.ID, u3.ID)
	first := h.send(family, f.ID, "three, edited")
	if got := h.openOf(f.ID).Resume; got == nil || got.Fork == nil || got.Fork.AtSDKUUID != "uuid-a1" || got.SDKSessionID != "sdk-r" {
		t.Fatalf("resume = %+v; want sdk-r at uuid-a1, skipping the failed turn", got)
	}
	if first.ParentMessageID == nil || *first.ParentMessageID != failed.ID {
		t.Fatalf("first message parent = %v, want the last message before M (%s)", first.ParentMessageID, failed.ID)
	}

	// Only a failed turn before M: nothing to resume.
	r2 := h.newSession(family)
	h.send(family, r2.ID, "one")
	h.link(r2.ID, protocol.LinkMessageStarted{Model: haiku})
	h.hostState(protocol.HostState{SessionID: r2.ID, State: "closed", Reason: "error"})
	u2, _ := h.exchange(family, r2.ID, "two", "uuid-b2")
	f2 := h.forkAt(family, r2.ID, u2.ID)
	h.send(family, f2.ID, "two, edited")
	if got := h.openOf(f2.ID).Resume; got != nil {
		t.Fatalf("resume = %+v, want null: no assistant message before M has an sdk_uuid", got)
	}
}

// P9: forks inherit taint, including a taint held in memory after a failed
// write (S46); an untainted session forks untainted.
func TestForkInheritsTaint(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	h.send(family, r.ID, "fetch it")
	h.link(r.ID, protocol.LinkMessageStarted{Model: haiku},
		protocol.LinkToolStarted{ToolUseID: "tu1", Name: "WebFetch", Summary: "example.test"},
		result("uuid-a1", "fetched", 0.01))
	u2, _ := h.exchange(family, r.ID, "two", "uuid-a2")
	taintedAt := h.row(r.ID).TaintedAt
	if taintedAt == nil {
		t.Fatal("WebFetch did not taint the source")
	}
	f := h.forkAt(family, r.ID, u2.ID)
	if row := h.row(f.ID); !f.Tainted || row.TaintedAt == nil || !row.TaintedAt.Equal(*taintedAt) {
		t.Fatalf("fork tainted = %v, tainted_at = %v; want the source's %v", f.Tainted, row.TaintedAt, *taintedAt)
	}
	h.send(family, f.ID, "two, edited")
	if !h.lastSend().Untrusted {
		t.Fatal("the fork's first turn was not untrusted")
	}

	clean := h.newSession(family)
	cu, _ := h.exchange(family, clean.ID, "one", "uuid-c1")
	if fc := h.forkAt(family, clean.ID, cu.ID); fc.Tainted || h.row(fc.ID).TaintedAt != nil {
		t.Fatalf("fork of an untainted session = %+v, want untainted", fc)
	}

	h.sql(`create function luma_fail() returns trigger language plpgsql as $$ begin raise exception 'injected failure'; end $$`)
	h.sql(`create trigger fail_taint before update on sessions for each row when (old.tainted_at is distinct from new.tainted_at) execute function luma_fail()`)
	held := h.newSession(family)
	hu := h.send(family, held.ID, "fetch again")
	h.link(held.ID, protocol.LinkMessageStarted{Model: haiku},
		protocol.LinkToolStarted{ToolUseID: "tu2", Name: "WebFetch", Summary: "example.test"},
		result("uuid-h1", "fetched", 0.01))
	if h.row(held.ID).TaintedAt != nil {
		t.Fatal("the injected failure did not stop the taint write")
	}
	fh := h.forkAt(family, held.ID, hu.ID)
	if !fh.Tainted || h.row(fh.ID).TaintedAt == nil {
		t.Fatalf("fork of a session whose taint is held in memory = %+v; want tainted", fh)
	}
}

// Fork refuses another person's session, and a message that is another
// person's; Tree refuses another person's tree.
func TestForkOtherPersons404(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	u1, _ := h.exchange(family, r.ID, "one", "uuid-a1")
	if _, err := h.svc.Fork(bg, other, r.ID, u1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's fork: err = %v, want ErrNotFound", err)
	}
	theirs := h.newSession(other)
	tu, _ := h.exchange(other, theirs.ID, "theirs", "uuid-t1")
	if _, err := h.svc.Fork(bg, family, r.ID, tu.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a fork at another person's message: err = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Fork(bg, family, theirs.ID, tu.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a fork of another person's session at its message: err = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Fork(bg, family, newID("ses_", testNow), u1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a fork of a missing session: err = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Tree(bg, family, theirs.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's tree: err = %v, want ErrNotFound", err)
	}
	for _, root := range []string{r.ID, theirs.ID} {
		if tree, err := h.st.TreeSessions(bg, root); err != nil || len(tree) != 1 {
			t.Fatalf("tree %s has %d sessions (%v), want only its root", root, len(tree), err)
		}
	}
}

// M must be a user message in the forked session's history: a message of a
// sibling branch or an unknown id is 404, an assistant message or none 400.
func TestForkRefusesMessagesOutsideHistory(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	_, a1 := h.exchange(family, r.ID, "one", "uuid-a1")
	u2, _ := h.exchange(family, r.ID, "two", "uuid-a2")
	f := h.forkAt(family, r.ID, u2.ID)
	u2e, _ := h.exchange(family, f.ID, "two, edited", "uuid-f2")

	for _, c := range []struct {
		name, from, at string
		want           error
	}{
		{"the source's M, from the fork (not in its history)", f.ID, u2.ID, ErrNotFound},
		{"the fork's message, from the root", r.ID, u2e.ID, ErrNotFound},
		{"an unknown message", r.ID, newID("msg_", testNow), ErrNotFound},
		{"an assistant message", r.ID, a1.ID, ErrInvalid},
		{"no message", r.ID, "", ErrInvalid},
	} {
		if _, err := h.svc.Fork(bg, family, c.from, c.at); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if tree, err := h.st.TreeSessions(bg, r.ID); err != nil || len(tree) != 2 {
		t.Fatalf("tree has %d sessions (%v), want the root and its one fork", len(tree), err)
	}
}

// Forking at the running turn's message is 409 busy; an earlier message of
// the same session may be forked while that turn runs.
func TestForkAtRunningTurnIsBusy(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	u1, _ := h.exchange(family, r.ID, "one", "uuid-a1")
	u2 := h.send(family, r.ID, "two")
	if _, err := h.svc.Fork(bg, family, r.ID, u2.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("a fork at the running turn's message: err = %v, want ErrBusy", err)
	}
	h.forkAt(family, r.ID, u1.ID)
	h.finish(r.ID)
	h.forkAt(family, r.ID, u2.ID)
}

// While the tree is being deleted, Fork answers 404 on any of its sessions
// (S39, Task 9b carry).
func TestForkWhileTreeDeletes404(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	u1, _ := h.exchange(family, r.ID, "one", "uuid-a1")
	f := h.forkAt(family, r.ID, u1.ID)
	fu, _ := h.exchange(family, f.ID, "one, edited", "uuid-f1")

	h.svc.deletions.mark(r.ID)
	for _, c := range []struct{ from, at string }{{r.ID, u1.ID}, {f.ID, fu.ID}} {
		if _, err := h.svc.Fork(bg, family, c.from, c.at); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Fork of %s while its tree is deleted: err = %v, want ErrNotFound", c.from, err)
		}
	}
	h.svc.deletions.unmark(r.ID)
	h.forkAt(family, r.ID, u1.ID)
}

// ---- Tree ----

// C1 /tree: every message of every session of the tree, each with its parent
// and session; head is the newest message of the most recently active
// session that has one; last_event_ids per session; the tree's pending
// writes only.
func TestTreeReturnsAllBranchesWithParents(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	u1, a1 := h.exchange(family, r.ID, "one", "uuid-a1")
	h.ready(r.ID, "sdk-r")
	u2, a2 := h.exchange(family, r.ID, "two", "uuid-a2")
	f := h.forkAt(family, r.ID, u2.ID)
	u2e, a2e := h.exchange(family, f.ID, "two, edited", "uuid-f2")

	elsewhere := h.newSession(family)
	for _, w := range []store.MemoryWrite{
		{ID: "mem_fork_pending", SessionID: str(f.ID), Status: "pending"},
		{ID: "mem_root_applied", SessionID: str(r.ID), Status: "applied"},
		{ID: "mem_elsewhere_pending", SessionID: str(elsewhere.ID), Status: "pending"},
	} {
		w.PersonID, w.TurnID, w.Op, w.Text = family.ID, "turn_synthetic", "remember", "synthetic"
		if _, err := h.st.InsertMemoryWrite(bg, w); err != nil {
			t.Fatal(err)
		}
	}

	tree := h.tree(family, r.ID)
	type node struct {
		id      string
		parent  *string
		session string
	}
	want := []node{
		{u1.ID, nil, r.ID}, {a1.ID, &u1.ID, r.ID}, {u2.ID, &a1.ID, r.ID}, {a2.ID, &u2.ID, r.ID},
		{u2e.ID, &a1.ID, f.ID}, {a2e.ID, &u2e.ID, f.ID},
	}
	got := make([]node, 0, len(tree.Messages))
	for _, m := range tree.Messages {
		got = append(got, node{m.Message.ID, m.ParentID, m.SessionID})
		if !reflect.DeepEqual(m.ParentID, m.Message.ParentMessageID) || m.SessionID != m.Message.SessionID {
			t.Errorf("node %s: parent_id %v / session %s disagree with its message", m.Message.ID, m.ParentID, m.SessionID)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree messages = %+v\nwant %+v", got, want)
	}
	if m := tree.Messages[1].Message; m.Status != "complete" || m.SDKUUID == nil || *m.SDKUUID != "uuid-a1" ||
		len(m.Content) != 1 || m.Content[0].Text != "re: one" {
		t.Fatalf("A1 in the tree = %+v", m)
	}
	if tree.HeadID == nil || *tree.HeadID != a2e.ID {
		t.Fatalf("head = %v, want the fork's newest message %s", tree.HeadID, a2e.ID)
	}
	if wantIDs := map[string]int64{r.ID: h.lastEventID(r.ID), f.ID: h.lastEventID(f.ID)}; !reflect.DeepEqual(tree.LastEventIDs, wantIDs) {
		t.Fatalf("last_event_ids = %v, want %v", tree.LastEventIDs, wantIDs)
	}
	if len(tree.PendingWrites) != 1 || tree.PendingWrites[0].ID != "mem_fork_pending" ||
		tree.PendingWrites[0].SessionID == nil || *tree.PendingWrites[0].SessionID != f.ID || tree.PendingWrites[0].Status != "pending" {
		t.Fatalf("pending_writes = %+v, want only the tree's pending write", tree.PendingWrites)
	}
	if fromFork := h.tree(family, f.ID); !reflect.DeepEqual(fromFork, tree) {
		t.Fatalf("the tree read through the fork differs:\n%+v\nwant %+v", fromFork, tree)
	}

	// A fork with no message yet does not move the head; a turn on the root does.
	empty := h.forkAt(family, r.ID, u2.ID)
	tree = h.tree(family, r.ID)
	if tree.HeadID == nil || *tree.HeadID != a2e.ID || tree.LastEventIDs[empty.ID] != 0 || len(tree.Messages) != 6 {
		t.Fatalf("after an empty fork: head %v, last_event_ids %v, %d messages", tree.HeadID, tree.LastEventIDs, len(tree.Messages))
	}
	_, a3 := h.exchange(family, r.ID, "three", "uuid-a3")
	if tree = h.tree(family, r.ID); tree.HeadID == nil || *tree.HeadID != a3.ID {
		t.Fatalf("after a turn on the root: head = %v, want %s", tree.HeadID, a3.ID)
	}

	// A conversation with no messages has no head.
	if tree := h.tree(family, elsewhere.ID); tree.HeadID != nil || len(tree.Messages) != 0 {
		t.Fatalf("empty tree = %+v", tree)
	}
}

// A running turn's assistant message is in the tree as the actor holds it,
// and last_event_ids is the event that state ends at: a stream opened since
// it replays exactly what the tree does not show. The tree is a copy.
func TestTreeOverlaysRunningMessage(t *testing.T) {
	h := newHarness(t)
	r := h.newSession(family)
	h.send(family, r.ID, "tell me")
	h.link(r.ID, protocol.LinkMessageStarted{Model: haiku}, protocol.LinkTextDelta{Text: "Hel"}, protocol.LinkTextDelta{Text: "lo"},
		protocol.LinkToolStarted{ToolUseID: "tu1", Name: "remember", Summary: "noted"})
	if stored := h.messages(r.ID, "assistant")[0]; len(stored.Content) != 0 {
		t.Fatalf("stored running message content = %+v; this test needs it to lag", stored.Content)
	}
	tree := h.tree(family, r.ID)
	if got := tree.LastEventIDs[r.ID]; got != h.lastEventID(r.ID) {
		t.Fatalf("last_event_ids = %d, want the newest event %d", got, h.lastEventID(r.ID))
	}
	if len(tree.Messages) != 2 {
		t.Fatalf("tree has %d messages, want 2", len(tree.Messages))
	}
	asst := tree.Messages[1].Message
	wantContent := []protocol.MessagePart{{Type: "text", Text: "Hello"}, {Type: "tool", ToolUseID: "tu1", Name: "remember", Summary: "noted"}}
	if asst.Status != "running" || !reflect.DeepEqual(asst.Content, wantContent) {
		t.Fatalf("running message in the tree = %+v, want status running and content %+v", asst, wantContent)
	}
	if tree.HeadID == nil || *tree.HeadID != asst.ID {
		t.Fatalf("head = %v, want the running message", tree.HeadID)
	}

	h.link(r.ID, protocol.LinkToolFinished{ToolUseID: "tu1", IsError: true, Summary: "failed"}, protocol.LinkTextDelta{Text: " there"})
	if !reflect.DeepEqual(asst.Content, wantContent) {
		t.Fatalf("the tree's message changed after it was returned: %+v", asst.Content)
	}
	h.link(r.ID, result("uuid-1", "Hello there", 0.01))
	if m := h.tree(family, r.ID).Messages[1].Message; m.Status != "complete" || len(m.Content) != 1 || m.Content[0].Text != "Hello there" {
		t.Fatalf("completed message in the tree = %+v", m)
	}
}

// ---- routes ----

// POST /fork returns the new Session and GET /tree the Tree, through Routes
// behind the BFF; their errors are ErrorInfo.
func TestForkAndTreeRoutes(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	c, oc := w.login(family), w.login(other)
	r := h.newSession(family)
	u1, a1 := h.exchange(family, r.ID, "one", "uuid-a1")
	u2, _ := h.exchange(family, r.ID, "two", "uuid-a2")

	var f protocol.Session
	w.json(http.MethodPost, "/v1/sessions/"+r.ID+"/fork", c, fmt.Sprintf(`{"at_message":%q}`, u2.ID), &f)
	if f.RootID != r.ID || f.ParentID == nil || *f.ParentID != r.ID || f.ForkAtMessageID == nil || *f.ForkAtMessageID != u2.ID || f.Title != "one" {
		t.Fatalf("POST /fork = %+v", f)
	}
	var sent protocol.SendResult
	w.json(http.MethodPost, "/v1/sessions/"+f.ID+"/events", c, userMessageJSON("two, edited"), &sent)
	if sent.Message == nil || sent.Message.ParentMessageID == nil || *sent.Message.ParentMessageID != a1.ID {
		t.Fatalf("the fork's first message = %+v, want parent %s", sent.Message, a1.ID)
	}

	st, body := w.do(http.MethodGet, "/v1/sessions/"+r.ID+"/tree", c, "")
	if st != http.StatusOK {
		t.Fatalf("GET /tree = %d %s", st, body)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 4 || string(keys["pending_writes"]) != "[]" || keys["messages"] == nil || keys["head_id"] == nil || keys["last_event_ids"] == nil {
		t.Fatalf("GET /tree body %s; want exactly messages, head_id, last_event_ids and pending_writes ([])", body)
	}
	var tree protocol.Tree
	if err := json.Unmarshal(body, &tree); err != nil {
		t.Fatal(err)
	}
	if len(tree.Messages) != 5 || tree.Messages[0].Message.ID != u1.ID || tree.HeadID == nil || *tree.HeadID != sent.Message.ID {
		t.Fatalf("GET /tree = %+v; want 5 messages, head the fork's message", tree)
	}
	if last := tree.Messages[4]; last.Message.ID != sent.Message.ID || last.SessionID != f.ID || last.ParentID == nil || *last.ParentID != a1.ID {
		t.Fatalf("the fork's node = %+v", last)
	}
	if len(tree.LastEventIDs) != 2 || tree.LastEventIDs[f.ID] != h.lastEventID(f.ID) {
		t.Fatalf("last_event_ids = %v", tree.LastEventIDs)
	}

	fork := func(sessionID, body string) (int, []byte) {
		return w.do(http.MethodPost, "/v1/sessions/"+sessionID+"/fork", c, body)
	}
	st, body = w.do(http.MethodPost, "/v1/sessions/"+r.ID+"/fork", oc, fmt.Sprintf(`{"at_message":%q}`, u1.ID))
	wantError(t, st, body, http.StatusNotFound, protocol.CodeNotFound)
	st, body = w.do(http.MethodGet, "/v1/sessions/"+r.ID+"/tree", oc, "")
	wantError(t, st, body, http.StatusNotFound, protocol.CodeNotFound)
	st, body = fork(f.ID, fmt.Sprintf(`{"at_message":%q}`, sent.Message.ID))
	wantError(t, st, body, http.StatusConflict, protocol.CodeBusy)
	st, body = fork(r.ID, fmt.Sprintf(`{"at_message":%q}`, a1.ID))
	wantError(t, st, body, http.StatusBadRequest, protocol.CodeInvalidRequest)
	st, body = fork(r.ID, `{}`)
	wantError(t, st, body, http.StatusBadRequest, protocol.CodeInvalidRequest)
	st, body = fork(r.ID, `{"at_message":`)
	wantError(t, st, body, http.StatusBadRequest, protocol.CodeInvalidRequest)
	if sessions, err := h.st.TreeSessions(bg, r.ID); err != nil || len(sessions) != 2 {
		t.Fatalf("tree has %d sessions (%v), want the root and the one fork", len(sessions), err)
	}
}
