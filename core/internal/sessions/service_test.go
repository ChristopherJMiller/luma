package sessions

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

var bg = context.Background()

var (
	family = auth.Person{ID: "pers-fam", DisplayName: "Synthetic Family", Tier: "family"}
	admin  = auth.Person{ID: "pers-adm", DisplayName: "Synthetic Admin", Tier: "admin"}
	other  = auth.Person{ID: "pers-oth", DisplayName: "Synthetic Other", Tier: "family"}
)

// testNow is 19:30 PDT on Sunday 2026-10-18, already the 19th in UTC.
var testNow = time.Date(2026, 10, 19, 2, 30, 0, 0, time.UTC)

// The shipped agents and models.yaml, three levels up from this package.
const shippedDir = "../../../agents"

const adminOnlyAgent = `---
name: household-admin
description: A synthetic agent only admins may use.
kind: agent
tiers: [fast]
audience: {tier: admin}
memory: none
---
You are a synthetic test agent.
`

// catalogDir writes the shipped luma and deep-think agents, models.yaml with
// the given helper model, and an admin-only agent into a fresh directory.
func catalogDir(t *testing.T, helper string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range []string{"luma/AGENT.md", "deep-think/AGENT.md"} {
		b, err := os.ReadFile(filepath.Join(shippedDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, rel), string(b))
	}
	writeFile(t, filepath.Join(dir, "household-admin", "AGENT.md"), adminOnlyAgent)
	writeModels(t, dir, helper)
	return dir
}

func writeModels(t *testing.T, dir, helper string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(shippedDir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	models := strings.Replace(string(b), "helper_model: claude-haiku-5-5", "helper_model: "+helper, 1)
	if !strings.Contains(models, "helper_model: "+helper) {
		t.Fatal("models.yaml has no helper_model line to replace")
	}
	writeFile(t, filepath.Join(dir, "models.yaml"), models)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- fakes ----

type fakeConductor struct {
	mu         sync.Mutex
	opens      []protocol.HostSpec
	sends      []protocol.TurnSend
	interrupts []string
	openErr    error
	onSend     func(protocol.TurnSend)
}

func (c *fakeConductor) Open(_ context.Context, spec protocol.HostSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openErr != nil {
		return c.openErr
	}
	c.opens = append(c.opens, spec)
	return nil
}

func (c *fakeConductor) SendTurn(_ context.Context, ts protocol.TurnSend) error {
	c.mu.Lock()
	hook := c.onSend
	c.mu.Unlock()
	if hook != nil {
		hook(ts)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, ts)
	return nil
}

func (c *fakeConductor) Interrupt(_ context.Context, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.interrupts = append(c.interrupts, sessionID)
	return nil
}

func (c *fakeConductor) Close(context.Context, string, string) error     { return nil }
func (c *fakeConductor) PurgeTree(context.Context, string, string) error { return nil }
func (c *fakeConductor) Connected() bool                                 { return true }

func (c *fakeConductor) Opens() []protocol.HostSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.HostSpec(nil), c.opens...)
}

func (c *fakeConductor) Sends() []protocol.TurnSend {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.TurnSend(nil), c.sends...)
}

func (c *fakeConductor) Interrupts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.interrupts...)
}

type fakeGate struct {
	mu      sync.Mutex
	refusal *protocol.ErrorInfo
}

func (g *fakeGate) CheckNewTurn(context.Context) *protocol.ErrorInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refusal
}

func (g *fakeGate) set(code protocol.ErrorCode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if code == "" {
		g.refusal = nil
		return
	}
	g.refusal = &protocol.ErrorInfo{Code: code, Message: protocol.ErrorCopy[code]}
}

type fakeProfile struct {
	mu    sync.Mutex
	text  string
	calls int
}

func (p *fakeProfile) Snapshot(context.Context, string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.text, nil
}

func (p *fakeProfile) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type bindCall struct {
	person, root, tier string
	ids                []string
}

type fakeResolver struct {
	mu     sync.Mutex
	blocks map[string]protocol.ContentBlock
	calls  []bindCall
}

func (r *fakeResolver) Bind(_ context.Context, p auth.Person, rootID string, ids []string, tier string) ([]protocol.ContentBlock, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, bindCall{p.ID, rootID, tier, append([]string(nil), ids...)})
	out := make([]protocol.ContentBlock, 0, len(ids))
	for _, id := range ids {
		b, ok := r.blocks[id]
		if !ok {
			return nil, fmt.Errorf("fake resolver: no block for %s", id)
		}
		out = append(out, b)
	}
	return out, nil
}

type ledgerTurn struct {
	sessionID, turnID string
	usage             protocol.TurnUsage
	costBasis         *string
}

type ledgerMain struct {
	sessionID string
	usage     protocol.MainRequestUsage
}

type fakeLedger struct {
	mu    sync.Mutex
	turns []ledgerTurn
	mains []ledgerMain
}

func (l *fakeLedger) RecordTurn(_ context.Context, sess store.Session, turnID string, u protocol.TurnUsage, costBasis *string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turns = append(l.turns, ledgerTurn{sess.ID, turnID, u, costBasis})
	return nil
}

func (l *fakeLedger) RecordMainRequest(sess store.Session, u protocol.MainRequestUsage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mains = append(l.mains, ledgerMain{sess.ID, u})
}

// ---- harness ----

type harness struct {
	t    *testing.T
	st   *store.Store
	dir  string
	live *agents.Live
	cond *fakeConductor
	gate *fakeGate
	prof *fakeProfile
	att  *fakeResolver
	led  *fakeLedger
	reg  *prometheus.Registry
	m    *metrics.Core
	svc  *Service
}

// sql runs a statement as the database superuser: tests inject store
// failures with triggers.
func (h *harness) sql(q string) {
	h.t.Helper()
	conn := storetest.ConnAs(h.t, h.st, storetest.Superuser)
	if _, err := conn.Exec(bg, q); err != nil {
		h.t.Fatalf("%s: %v", q, err)
	}
}

// counter reads an unlabelled counter from the harness's registry.
func (h *harness) counter(name string) float64 {
	h.t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	h.t.Fatalf("no counter %s", name)
	return 0
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Parallel()
	st := storetest.New(t)
	dir := catalogDir(t, "claude-haiku-5-5")
	cat, err := agents.LoadCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	h := &harness{
		t: t, st: st, dir: dir, live: agents.NewLive(cat),
		cond: &fakeConductor{}, gate: &fakeGate{},
		prof: &fakeProfile{text: "<luma-context>\nsynthetic profile\n</luma-context>"},
		att:  &fakeResolver{blocks: map[string]protocol.ContentBlock{}},
		led:  &fakeLedger{}, reg: reg, m: metrics.NewCore(reg),
	}
	h.svc = h.service(h.cond)
	return h
}

// service builds another Service over the harness's store, catalog and fakes:
// a second one stands for a restarted Core.
func (h *harness) service(c Conductor) *Service {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		h.t.Fatal(err)
	}
	return NewService(h.st, h.live, c, h.prof, h.att, h.gate, h.led, h.m,
		func() time.Time { return testNow },
		Config{Location: la, WebFetchDenyDomains: []string{"deny.example"}})
}

func (h *harness) create(p auth.Person, req protocol.CreateSession) protocol.Session {
	h.t.Helper()
	ses, err := h.svc.Create(bg, p, req)
	if err != nil {
		h.t.Fatalf("Create(%+v): %v", req, err)
	}
	return ses
}

func (h *harness) newSession(p auth.Person) protocol.Session {
	h.t.Helper()
	return h.create(p, protocol.CreateSession{Agent: "luma"})
}

func newClientID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func userMsg(text, clientID string, attachmentIDs ...string) protocol.UserMessage {
	return protocol.UserMessage{
		ClientID:      clientID,
		Content:       []protocol.TextBlock{{Type: "text", Text: text}},
		AttachmentIDs: attachmentIDs,
	}
}

// send posts a user message with a fresh client id and fails the test on error.
func (h *harness) send(p auth.Person, sessionID, text string, attachmentIDs ...string) protocol.Message {
	h.t.Helper()
	m, err := h.svc.Send(bg, p, sessionID, userMsg(text, newClientID(), attachmentIDs...))
	if err != nil {
		h.t.Fatalf("Send: %v", err)
	}
	if m == nil {
		h.t.Fatal("Send returned no message")
	}
	return *m
}

// flush waits until the session's actor has handled everything queued so far.
func (h *harness) flush(sessionID string) {
	h.t.Helper()
	if err := h.svc.call(bg, sessionID, func(context.Context, *actor) error { return nil }); err != nil {
		h.t.Fatal(err)
	}
}

// link delivers link events for the session's running turn, then flushes.
func (h *harness) link(sessionID string, evs ...protocol.LinkEvent) {
	h.t.Helper()
	turnID, ok := h.svc.RunningTurn(sessionID)
	if !ok {
		h.t.Fatalf("session %s has no running turn", sessionID)
	}
	for _, ev := range evs {
		h.svc.OnLinkEvent(sessionID, turnID, ev)
	}
	h.flush(sessionID)
}

func (h *harness) hostState(hs protocol.HostState) {
	h.t.Helper()
	h.svc.OnHostState(hs)
	h.flush(hs.SessionID)
}

func (h *harness) lastSend() protocol.TurnSend {
	h.t.Helper()
	sends := h.cond.Sends()
	if len(sends) == 0 {
		h.t.Fatal("no turn.send")
	}
	return sends[len(sends)-1]
}

func (h *harness) row(sessionID string) store.Session {
	h.t.Helper()
	s, err := h.st.Session(bg, sessionID)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) messages(sessionID, role string) []protocol.Message {
	h.t.Helper()
	all, err := h.st.Messages(bg, sessionID)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []protocol.Message
	for _, m := range all {
		if m.Role == role {
			out = append(out, m)
		}
	}
	return out
}

func (h *harness) events(sessionID, typ string) []protocol.Event {
	h.t.Helper()
	all, err := h.st.EventsSince(bg, sessionID, 0, 100000)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []protocol.Event
	for _, e := range all {
		if typ == "" || e.Body.EventType() == typ {
			out = append(out, e)
		}
	}
	return out
}

var haiku = "claude-haiku-5-5"

func result(sdkUUID, text string, cost float64) protocol.TurnResult {
	basis := "list"
	return protocol.TurnResult{
		SDKUUID:   sdkUUID,
		Content:   []protocol.MessagePart{{Type: "text", Text: text}},
		Usage:     protocol.TurnUsage{Models: map[string]protocol.ModelUsage{haiku: {Input: 10, Output: 5, CostUSD: cost}}, CostUSD: cost},
		Totals:    protocol.Totals{Models: map[string]protocol.ModelUsage{haiku: {Input: 30, Output: 15, CostUSD: 3 * cost}}, CostUSD: 3 * cost},
		CostBasis: &basis,
	}
}

// finish completes the running turn of a session.
func (h *harness) finish(sessionID string) {
	h.t.Helper()
	h.link(sessionID, protocol.LinkMessageStarted{Model: haiku}, result("sdk-"+sessionID, "ok", 0.01))
}

// ---- Create ----

func TestCreateRefusedByGate(t *testing.T) {
	h := newHarness(t)
	for _, code := range []protocol.ErrorCode{protocol.CodeMaintenance, protocol.CodeBudgetExhausted} {
		h.gate.set(code)
		_, err := h.svc.Create(bg, family, protocol.CreateSession{Agent: "luma"})
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Info.Code != code {
			t.Fatalf("Create with a %s gate: err = %v, want a RefusedError with that code", code, err)
		}
	}
	if roots, err := h.st.ListRoots(bg, family.ID, true, 100); err != nil || len(roots) != 0 {
		t.Fatalf("session rows after refused creates = %d (%v), want 0", len(roots), err)
	}
	if n := h.prof.Calls(); n != 0 {
		t.Errorf("the profile was read %d times before the gate refused; the gate runs first", n)
	}
}

func TestSendRefusedByGate(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.gate.set(protocol.CodeBudgetExhausted)
	_, err := h.svc.Send(bg, family, ses.ID, userMsg("hi", newClientID()))
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Info.Code != protocol.CodeBudgetExhausted {
		t.Fatalf("Send with a budget gate: err = %v", err)
	}
	if n := len(h.messages(ses.ID, "user")); n != 0 || len(h.cond.Sends()) != 0 {
		t.Fatalf("refused send left %d user messages and %d turn.sends", n, len(h.cond.Sends()))
	}
	if _, running := h.svc.RunningTurn(ses.ID); running {
		t.Fatal("a refused send started a turn")
	}
	h.gate.set("")
	h.send(family, ses.ID, "hi")
}

func TestSourceDerivedNotRead(t *testing.T) {
	h := newHarness(t)
	var req protocol.CreateSession
	if err := json.Unmarshal([]byte(`{"agent":"luma","params":{},"source":"ci"}`), &req); err != nil {
		t.Fatal(err)
	}
	ses := h.create(family, req)
	row := h.row(ses.ID)
	if ses.Source != "web" || row.Source != "web" || row.APIWorkspace != "interactive" || row.Workspace != "none" {
		t.Fatalf("source = %q / %q, api_workspace = %q, workspace = %q; want web, web, interactive, none",
			ses.Source, row.Source, row.APIWorkspace, row.Workspace)
	}
}

func TestCreateRefusesSubagentAndAudience(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		p     auth.Person
		agent string
	}{{family, "deep-think"}, {admin, "deep-think"}, {family, "household-admin"}, {family, "no-such-agent"}} {
		if _, err := h.svc.Create(bg, c.p, protocol.CreateSession{Agent: c.agent}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Create(%s) as %s: err = %v, want ErrNotFound", c.agent, c.p.Tier, err)
		}
	}
	for _, p := range []auth.Person{family, admin} {
		if roots, err := h.st.ListRoots(bg, p.ID, true, 100); err != nil || len(roots) != 0 {
			t.Fatalf("%s has %d session rows after refusals (%v)", p.ID, len(roots), err)
		}
	}
	ses := h.create(admin, protocol.CreateSession{Agent: "household-admin"})
	if ses.AgentID != "household-admin" || ses.Tier != "fast" {
		t.Fatalf("admin session = %+v", ses)
	}
}

func TestDeepTierAdminOnly(t *testing.T) {
	h := newHarness(t)
	deep := protocol.CreateSession{Agent: "luma", Params: protocol.SessionParams{Tier: "deep"}}
	if _, err := h.svc.Create(bg, family, deep); !errors.Is(err, ErrInvalid) {
		t.Fatalf("family deep: err = %v, want ErrInvalid", err)
	}
	if roots, _ := h.st.ListRoots(bg, family.ID, true, 100); len(roots) != 0 {
		t.Fatal("a refused deep create wrote a row")
	}
	ses := h.create(admin, deep)
	if ses.Tier != "deep" || ses.Model != "claude-opus-5-5" {
		t.Fatalf("admin deep session: tier %q model %q", ses.Tier, ses.Model)
	}
}

func TestWorkspaceOtherThanNoneRejected(t *testing.T) {
	h := newHarness(t)
	bad := protocol.CreateSession{Agent: "luma", Params: protocol.SessionParams{Workspace: "sandbox"}}
	if _, err := h.svc.Create(bg, family, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("workspace sandbox: err = %v, want ErrInvalid", err)
	}
	ses := h.create(family, protocol.CreateSession{Agent: "luma", Params: protocol.SessionParams{Workspace: "none"}})
	if ses.Workspace != "none" {
		t.Fatalf("workspace = %q", ses.Workspace)
	}
}

func TestCreateTierAndEffortDefaults(t *testing.T) {
	h := newHarness(t)
	def := h.newSession(family)
	if def.Tier != "fast" || def.Model != "claude-haiku-5-5" || def.Effort != "low" {
		t.Fatalf("default session: tier %q model %q effort %q; want fast, claude-haiku-5-5, low", def.Tier, def.Model, def.Effort)
	}
	smart := h.create(family, protocol.CreateSession{Agent: "luma", Params: protocol.SessionParams{Tier: "smart"}})
	if smart.Model != "claude-sonnet-5-5" || smart.Effort != "medium" {
		t.Fatalf("smart session: model %q effort %q", smart.Model, smart.Effort)
	}
	high := h.create(family, protocol.CreateSession{Agent: "luma", Params: protocol.SessionParams{Effort: "high"}})
	if high.Effort != "high" {
		t.Fatalf("effort = %q, want high", high.Effort)
	}
	for _, params := range []protocol.SessionParams{{Effort: "max"}, {Tier: "turbo"}} {
		if _, err := h.svc.Create(bg, family, protocol.CreateSession{Agent: "luma", Params: params}); !errors.Is(err, ErrInvalid) {
			t.Errorf("params %+v: err = %v, want ErrInvalid", params, err)
		}
	}
	if _, err := h.svc.Create(bg, admin, protocol.CreateSession{Agent: "household-admin", Params: protocol.SessionParams{Tier: "smart"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a tier the agent does not offer: err = %v, want ErrInvalid", err)
	}
	row := h.row(def.ID)
	var spec protocol.AgentSpec
	if err := json.Unmarshal(row.AgentSnapshot, &spec); err != nil || spec.ID != "luma" || spec.TierBinding.Model != "claude-haiku-5-5" {
		t.Fatalf("agent_snapshot = %s (%v)", row.AgentSnapshot, err)
	}
	if !reflect.DeepEqual(row.BoundModels, []string{"claude-haiku-5-5", "claude-sonnet-5-5"}) {
		t.Fatalf("bound_models = %v, want the model, the helper and deep-think's model, once each", row.BoundModels)
	}
}

func TestHelperModelSnapshottedAtCreate(t *testing.T) {
	h := newHarness(t)
	reloads := make(chan error, 16)
	h.live.OnReload(func(err error) { reloads <- err })
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- h.live.Watch(ctx, h.dir) }()
	t.Cleanup(func() { cancel(); <-done })
	select { // the watcher reloads once after registering
	case <-reloads:
	case <-time.After(10 * time.Second):
		t.Fatal("no first reload")
	}

	old := h.newSession(family)
	writeModels(t, h.dir, "claude-haiku-9-9")
	deadline := time.Now().Add(15 * time.Second)
	for h.live.Models().HelperModel != "claude-haiku-9-9" {
		if time.Now().After(deadline) {
			t.Fatal("the catalog did not reload the new helper model")
		}
		time.Sleep(50 * time.Millisecond)
	}
	fresh := h.newSession(family)

	h.send(family, old.ID, "one")
	h.send(family, fresh.ID, "two")
	opens := h.cond.Opens()
	if len(opens) != 2 || opens[0].SessionID != old.ID || opens[1].SessionID != fresh.ID {
		t.Fatalf("opens = %+v", opens)
	}
	if opens[0].HelperModel != "claude-haiku-5-5" || opens[1].HelperModel != "claude-haiku-9-9" {
		t.Fatalf("helper models = %q, %q; want the old one for the old session and the new one for the new", opens[0].HelperModel, opens[1].HelperModel)
	}
	if got := h.row(fresh.ID).BoundModels; !reflect.DeepEqual(got, []string{"claude-haiku-5-5", "claude-haiku-9-9", "claude-sonnet-5-5"}) {
		t.Fatalf("new session bound_models = %v", got)
	}
}

func TestProfileSnapshotStoredVerbatim(t *testing.T) {
	h := newHarness(t)
	h.prof.text = "<luma-context>\n  Likes kites\t& <b>tea</b> é\r\n</luma-context>\n"
	ses := h.newSession(family)
	if got := h.row(ses.ID).ProfileSnapshot; got != h.prof.text {
		t.Fatalf("profile_snapshot = %q, want %q", got, h.prof.text)
	}
	h.send(family, ses.ID, "hi")
	if got := h.cond.Opens()[0].ProfileContext; got != h.prof.text {
		t.Fatalf("profile_context = %q, want %q", got, h.prof.text)
	}
	calls := h.prof.Calls()
	yes := true
	incog := h.create(family, protocol.CreateSession{Agent: "luma", Params: protocol.SessionParams{Incognito: &yes}})
	none := h.create(admin, protocol.CreateSession{Agent: "household-admin"}) // memory: none
	for _, id := range []string{incog.ID, none.ID} {
		if got := h.row(id).ProfileSnapshot; got != "" {
			t.Errorf("session %s profile_snapshot = %q, want empty", id, got)
		}
	}
	if h.prof.Calls() != calls {
		t.Errorf("the profile was read for an incognito or memory-none session")
	}
	if !incog.Incognito {
		t.Error("incognito not stored")
	}
}

// ---- HostSpec and resume ----

func TestHostSpecCarriesRefNotName(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	spec := h.cond.Opens()[0]
	ref, err := h.st.PersonRef(bg, family.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^u_[0-9a-f]{10}$`).MatchString(spec.Person.Ref) || spec.Person.Ref != ref {
		t.Fatalf("person.ref = %q, want the store's ref %q", spec.Person.Ref, ref)
	}
	b, err := json.Marshal(protocol.HostOpen{SessionID: ses.ID, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{family.ID, family.DisplayName, "display_name"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("host.open carries %q", leak)
		}
	}
	if spec.SessionID != ses.ID || spec.RootID != ses.ID || spec.Agent.ID != "luma" || spec.Model != "claude-haiku-5-5" ||
		spec.HelperModel != "claude-haiku-5-5" || spec.Effort != "low" || spec.CacheTTL != "1h" ||
		spec.Workspace != "interactive" || spec.Source != "web" || spec.Timezone != "America/Los_Angeles" ||
		!reflect.DeepEqual(spec.WebfetchDenyDomains, []string{"deny.example"}) {
		t.Fatalf("host spec = %+v", spec)
	}
}

func TestHostStatePersistsSDKSessionID(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.hostState(protocol.HostState{SessionID: ses.ID, State: "ready", SDKSessionID: "sdk-s1"})
	if got := h.row(ses.ID).SDKSessionID; got == nil || *got != "sdk-s1" {
		t.Fatalf("sdk_session_id = %v, want sdk-s1", got)
	}
}

func TestTurnResultPersistsLastTotals(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "hi")
	r := result("sdk-u1", "hello", 0.02)
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, r)
	got := h.row(ses.ID).LastTotals
	if got == nil || !reflect.DeepEqual(*got, r.Totals) {
		t.Fatalf("last_totals = %+v, want %+v", got, r.Totals)
	}
}

func TestReopenBuildsResume(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "one")
	h.hostState(protocol.HostState{SessionID: ses.ID, State: "ready", SDKSessionID: "s1"})
	r := result("sdk-u1", "hello", 0.02)
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku}, r)
	h.hostState(protocol.HostState{SessionID: ses.ID, State: "closed", Reason: "idle"})
	h.send(family, ses.ID, "two")
	opens := h.cond.Opens()
	if len(opens) != 2 {
		t.Fatalf("host.open sent %d times, want 2 (lazy reopen after closed)", len(opens))
	}
	want := &protocol.Resume{SDKSessionID: "s1", BaselineTotals: &r.Totals}
	if !reflect.DeepEqual(opens[1].Resume, want) {
		t.Fatalf("resume = %+v, want %+v", opens[1].Resume, want)
	}
}

func TestFreshSessionHasNoResume(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "one")
	h.finish(ses.ID)
	h.send(family, ses.ID, "two")
	opens := h.cond.Opens()
	if len(opens) != 1 || opens[0].Resume != nil {
		t.Fatalf("opens = %d, first resume = %+v; want one open with a null resume", len(opens), opens[0].Resume)
	}
	if len(h.cond.Sends()) != 2 {
		t.Fatalf("turn.sends = %d, want 2", len(h.cond.Sends()))
	}
}

// ---- Send ----

func TestSendPersistsBeforeConductor(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	live, cancel := h.svc.hub.subscribe(ses.ID, 100)
	defer cancel()
	var stored, persisted, fannedOut bool
	calls := 0
	h.cond.onSend = func(ts protocol.TurnSend) {
		if calls++; calls > 1 {
			return // only the first turn.send is observed
		}
		for _, m := range h.messages(ses.ID, "user") {
			stored = stored || m.TurnID == ts.TurnID
		}
		persisted = len(h.events(ses.ID, "message.user")) == 1
	drain:
		for {
			select {
			case ev, ok := <-live:
				if !ok {
					break drain
				}
				if mu, isUser := ev.Body.(protocol.MessageUser); isUser && mu.Message.TurnID == ts.TurnID {
					fannedOut = true
				}
			default:
				break drain
			}
		}
	}
	h.send(family, ses.ID, "hello")
	if calls != 1 {
		t.Fatalf("turn.send called %d times, want 1", calls)
	}
	if !stored || !persisted || !fannedOut {
		t.Fatalf("at turn.send: message stored %v, event persisted %v, fanned out %v; want all true", stored, persisted, fannedOut)
	}
}

func TestSendIdempotentClientID(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	cid := newClientID()
	first, err := h.svc.Send(bg, family, ses.ID, userMsg("hello", cid))
	if err != nil {
		t.Fatal(err)
	}
	// A resend while the turn runs (a flaky POST) returns the same message.
	again, err := h.svc.Send(bg, family, ses.ID, userMsg("hello", cid))
	if err != nil || again == nil || again.ID != first.ID {
		t.Fatalf("resend while running = %+v, %v; want message %s", again, err, first.ID)
	}
	// Another id while it runs is busy...
	cid2 := newClientID()
	if _, err := h.svc.Send(bg, family, ses.ID, userMsg("second", cid2)); !errors.Is(err, ErrBusy) {
		t.Fatalf("new id while running: err = %v, want ErrBusy", err)
	}
	h.finish(ses.ID)
	// ...and after the turn the first id still returns its message and starts nothing.
	later, err := h.svc.Send(bg, family, ses.ID, userMsg("hello", cid))
	if err != nil || later.ID != first.ID {
		t.Fatalf("resend after the turn = %+v, %v", later, err)
	}
	if n := len(h.cond.Sends()); n != 1 {
		t.Fatalf("turn.sends = %d, want 1", n)
	}
	// The id refused as busy is not burnt: its retry starts a turn, once.
	second, err := h.svc.Send(bg, family, ses.ID, userMsg("second", cid2))
	if err != nil || second.ID == first.ID {
		t.Fatalf("retry of the busy id = %+v, %v", second, err)
	}
	if again, err := h.svc.Send(bg, family, ses.ID, userMsg("second", cid2)); err != nil || again.ID != second.ID {
		t.Fatalf("resend of the second id = %+v, %v", again, err)
	}
	if n, users := len(h.cond.Sends()), len(h.messages(ses.ID, "user")); n != 2 || users != 2 {
		t.Fatalf("turn.sends = %d, user messages = %d; want 2 and 2", n, users)
	}
	if _, err := h.svc.Send(bg, family, ses.ID, userMsg("x", "not-a-uuid")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed client_id: err = %v, want ErrInvalid", err)
	}
}

func TestSecondTurnWhileRunning409(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "one")
	if _, err := h.svc.Send(bg, family, ses.ID, userMsg("two", newClientID())); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if n := len(h.cond.Sends()); n != 1 {
		t.Fatalf("turn.sends = %d, want 1", n)
	}
}

func TestOtherPersonsSession404(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	if _, err := h.svc.Send(bg, other, ses.ID, userMsg("hi", newClientID())); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other person's send: err = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Send(bg, other, ses.ID, protocol.UserInterrupt{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other person's interrupt: err = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Send(bg, family, "ses_missing", userMsg("hi", newClientID())); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: err = %v, want ErrNotFound", err)
	}
	if n := len(h.messages(ses.ID, "user")); n != 0 {
		t.Fatalf("user messages = %d, want 0", n)
	}
}

func TestInterruptForwarded(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	if m, err := h.svc.Send(bg, family, ses.ID, protocol.UserInterrupt{}); err != nil || m != nil || len(h.cond.Interrupts()) != 0 {
		t.Fatalf("idle interrupt = %v, %v, %d calls; want nothing", m, err, len(h.cond.Interrupts()))
	}
	h.send(family, ses.ID, "one")
	if m, err := h.svc.Send(bg, family, ses.ID, protocol.UserInterrupt{}); err != nil || m != nil {
		t.Fatalf("interrupt = %v, %v", m, err)
	}
	if got := h.cond.Interrupts(); !reflect.DeepEqual(got, []string{ses.ID}) {
		t.Fatalf("interrupts = %v", got)
	}
}

func TestUserMessageCarriesAttachmentParts(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	for _, a := range []store.Attachment{
		{ID: "att_1", PersonID: family.ID, MediaType: "image/png", Filename: "kite.png", SizeBytes: 3, SHA256: "s1", Data: []byte{1, 2, 3}},
		{ID: "att_2", PersonID: family.ID, MediaType: "text/plain", Filename: "notes.txt", SizeBytes: 2, SHA256: "s2", Data: []byte("hi")},
	} {
		if err := h.st.InsertAttachment(bg, a); err != nil {
			t.Fatal(err)
		}
	}
	img := protocol.ContentBlock{Type: "image", Source: &protocol.Base64Source{Type: "base64", MediaType: "image/png", Data: "AQID"}}
	txt := protocol.ContentBlock{Type: "text", Text: `<luma-attachment name="notes.txt">hi</luma-attachment>`}
	h.att.blocks["att_1"], h.att.blocks["att_2"] = img, txt

	m := h.send(family, ses.ID, "look at these", "att_1", "att_2")
	want := []protocol.MessagePart{
		{Type: "text", Text: "look at these"},
		{Type: "attachment", ID: "att_1", MediaType: "image/png", Filename: "kite.png"},
		{Type: "attachment", ID: "att_2", MediaType: "text/plain", Filename: "notes.txt"},
	}
	if !reflect.DeepEqual(m.Content, want) {
		t.Fatalf("user message content = %+v, want %+v", m.Content, want)
	}
	ts := h.lastSend()
	if m.TurnID == "" || m.TurnID != ts.TurnID {
		t.Fatalf("message turn_id %q, turn.send turn_id %q", m.TurnID, ts.TurnID)
	}
	if len(ts.Content) != 4 || ts.Content[0].Text != "look at these" || !reflect.DeepEqual(ts.Content[1], img) ||
		!reflect.DeepEqual(ts.Content[2], txt) || !strings.HasPrefix(ts.Content[3].Text, "<luma-now>") {
		t.Fatalf("turn.send content = %+v", ts.Content)
	}
	if got := h.att.calls; len(got) != 1 || !reflect.DeepEqual(got[0], bindCall{family.ID, ses.ID, "fast", []string{"att_1", "att_2"}}) {
		t.Fatalf("Bind calls = %+v", got)
	}
	stored := h.messages(ses.ID, "user")
	if len(stored) != 1 || !reflect.DeepEqual(stored[0].Content, want) {
		t.Fatalf("stored user message = %+v", stored)
	}

	// Core checks the owner itself, whatever the resolver said.
	h.finish(ses.ID)
	if err := h.st.InsertAttachment(bg, store.Attachment{ID: "att_3", PersonID: other.ID, MediaType: "text/plain",
		Filename: "theirs.txt", SizeBytes: 1, SHA256: "s3", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	h.att.blocks["att_3"] = txt
	if _, err := h.svc.Send(bg, family, ses.ID, userMsg("and this", newClientID(), "att_3")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person's attachment: err = %v, want ErrNotFound", err)
	}
	if n := len(h.messages(ses.ID, "user")); n != 1 {
		t.Fatalf("user messages = %d, want still 1", n)
	}
}

func TestTaintFromAttachment(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	if err := h.st.InsertAttachment(bg, store.Attachment{ID: "att_1", PersonID: family.ID, MediaType: "text/plain",
		Filename: "a.txt", SizeBytes: 1, SHA256: "s", Data: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	h.att.blocks["att_1"] = protocol.ContentBlock{Type: "text", Text: "fenced"}
	if tainted, err := h.svc.Tainted(bg, ses.ID); err != nil || tainted {
		t.Fatalf("fresh session tainted = %v, %v", tainted, err)
	}
	h.send(family, ses.ID, "read this", "att_1")
	if tainted, err := h.svc.Tainted(bg, ses.ID); err != nil || !tainted {
		t.Fatalf("after an attachment tainted = %v, %v; want true", tainted, err)
	}
	if h.row(ses.ID).TaintedAt == nil || !h.lastSend().Untrusted {
		t.Fatal("tainted_at unset, or the attachment's own turn.send was not untrusted")
	}
}

func TestTurnSendCarriesUntrusted(t *testing.T) {
	h := newHarness(t)
	ses := h.newSession(family)
	h.send(family, ses.ID, "one")
	if h.lastSend().Untrusted {
		t.Fatal("an untainted session sent untrusted")
	}
	h.link(ses.ID, protocol.LinkMessageStarted{Model: haiku},
		protocol.LinkToolStarted{ToolUseID: "tu1", Name: "WebFetch", Summary: "example.test"},
		result("sdk-1", "done", 0.01))
	h.send(family, ses.ID, "two")
	if !h.lastSend().Untrusted {
		t.Fatal("the turn after a WebFetch was not untrusted")
	}
}

func TestTitleFromFirstMessage(t *testing.T) {
	if got := titleFrom("  Plan   the\n\tkite   festival  "); got != "Plan the kite festival" {
		t.Errorf("titleFrom = %q", got)
	}
	long := strings.Repeat("é", 70)
	if got := titleFrom(long); got != strings.Repeat("é", 60) {
		t.Errorf("titleFrom(70 runes) = %q (%d runes), want 60 runes", got, len([]rune(got)))
	}
	h := newHarness(t)
	ses := h.newSession(family)
	if ses.Title != "" {
		t.Fatalf("a new session's title = %q", ses.Title)
	}
	h.send(family, ses.ID, "What\n\nshould   I pack for a kite festival on a windy weekend by the sea?")
	want := "What should I pack for a kite festival on a windy weekend by" // 60 runes
	if got := h.row(ses.ID).Title; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	h.finish(ses.ID)
	h.send(family, ses.ID, "Another question entirely")
	if got := h.row(ses.ID).Title; got != want {
		t.Fatalf("title after a second message = %q, want it unchanged", got)
	}
}

// Ids are prefixed ULIDs, the shape C1's return_to pattern accepts
// (ses_[0-9A-HJKMNP-TV-Z]{26}); the first ten characters are the time.
func TestNewIDIsPrefixedULID(t *testing.T) {
	re := regexp.MustCompile(`^ses_[0-9A-HJKMNP-TV-Z]{26}$`)
	a, b := newID("ses_", time.UnixMilli(1)), newID("ses_", time.UnixMilli(1))
	if !re.MatchString(a) || !strings.HasPrefix(a, "ses_0000000001") || a == b {
		t.Fatalf("newID = %q, %q", a, b)
	}
	// 2026-10-19T02:30:00Z is 1792377000000 ms: base 32 digits 0 1 20 5 8 31 29 4 2 0.
	if got := newID("msg_", testNow)[:14]; got != "msg_01M58ZX420" {
		t.Fatalf("time part = %q, want msg_01M58ZX420 (2026-10-19T02:30:00Z)", got)
	}
	if early, late := newID("x_", testNow), newID("x_", testNow.Add(time.Millisecond)); early[:12] >= late[:12] {
		t.Fatalf("ids do not sort by time: %q >= %q", early, late)
	}
}
