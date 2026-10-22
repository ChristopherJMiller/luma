package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/auth/oidctest"
	"github.com/ChristopherJMiller/luma/core/internal/link"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

// The interfaces Memory implements and consumes, wired as Task 15 will.
var (
	_ sessions.ProfileSource = (*Memory)(nil)
	_ link.Memory            = (*Memory)(nil)
	_ TaintSource            = (*sessions.Service)(nil)
)

var bg = context.Background()

var (
	chris = auth.Person{ID: "pers-chris", DisplayName: "Synthetic Chris", Tier: "admin"}
	erin  = auth.Person{ID: "pers-erin", DisplayName: "Synthetic Erin", Tier: "family"}
)

// ---- the real session service in front of Memory ----

// lateService is the session service as Memory sees it: built after Memory
// (the Service takes Memory as its ProfileSource), as serve must, and
// recording what Memory asks of it.
type lateService struct {
	*sessions.Service
	mu         sync.Mutex
	emits      []string // "<session>:<type>"
	taints     int      // Taint calls
	queries    int      // Tainted calls
	taintErr   error    // when set, Taint fails with it
	taintedErr error    // when set, Tainted fails with it
}

func (l *lateService) Emit(ctx context.Context, sessionID, typ string, payload any) (int64, error) {
	l.mu.Lock()
	l.emits = append(l.emits, sessionID+":"+typ)
	l.mu.Unlock()
	return l.Sink().Emit(ctx, sessionID, typ, payload)
}

func (l *lateService) Tainted(ctx context.Context, sessionID string) (bool, error) {
	l.mu.Lock()
	l.queries++
	err := l.taintedErr
	l.mu.Unlock()
	if err != nil {
		return false, err
	}
	return l.Service.Tainted(ctx, sessionID)
}

func (l *lateService) Taint(ctx context.Context, sessionID string) error {
	l.mu.Lock()
	l.taints++
	err := l.taintErr
	l.mu.Unlock()
	if err != nil {
		return err
	}
	return l.Service.Taint(ctx, sessionID)
}

// fail makes Taint and Tainted fail with the given errors (nil: work).
func (l *lateService) fail(taintErr, taintedErr error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.taintErr, l.taintedErr = taintErr, taintedErr
}

// calls returns the recorded emits and the Taint and Tainted call counts.
func (l *lateService) calls() (emits []string, taints, queries int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.emits...), l.taints, l.queries
}

type noAttachments struct{}

func (noAttachments) Bind(context.Context, auth.Person, string, []string, string) ([]protocol.ContentBlock, error) {
	return nil, nil
}

type openGate struct{}

func (openGate) CheckNewTurn(context.Context) *protocol.ErrorInfo { return nil }

// fakeConductor accepts every host and turn.
type fakeConductor struct{}

func (fakeConductor) Open(context.Context, protocol.HostSpec) error     { return nil }
func (fakeConductor) SendTurn(context.Context, protocol.TurnSend) error { return nil }
func (fakeConductor) Interrupt(context.Context, string) error           { return nil }
func (fakeConductor) Close(context.Context, string, string) error       { return nil }
func (fakeConductor) PurgeTree(context.Context, string, string) error   { return nil }
func (fakeConductor) Connected() bool                                   { return true }

type harness struct {
	t    *testing.T
	st   *store.Store
	svc  *sessions.Service
	late *lateService
	mem  *Memory
	reg  *prometheus.Registry
}

// newHarness runs Memory over a real store and session service (the real
// agent catalog, a conductor that accepts everything), counting into one
// metrics registry.
func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Parallel()
	st := storetest.New(t)
	cat, err := agents.LoadCatalog("../../../agents")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	m := metrics.NewCore(reg)
	late := &lateService{}
	mem := New(st, late, late, WithMetrics(m))
	late.Service = sessions.NewService(st, agents.NewLive(cat), fakeConductor{}, mem, noAttachments{}, openGate{},
		sessions.NopLedger{}, m, time.Now, sessions.Config{})
	return &harness{t: t, st: st, svc: late.Service, late: late, mem: mem, reg: reg}
}

// counted reads luma_memory_writes_total{op,status} (0 if never counted).
func (h *harness) counted(op, status string) float64 {
	h.t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "luma_memory_writes_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["op"] == op && labels["status"] == status {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// session creates a conversation with the luma agent and returns its row.
func (h *harness) session(p auth.Person, incognito bool) store.Session {
	h.t.Helper()
	req := protocol.CreateSession{Agent: "luma"}
	if incognito {
		req.Params.Incognito = &incognito
	}
	ses, err := h.svc.Create(bg, p, req)
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	return h.row(ses.ID)
}

func (h *harness) row(sessionID string) store.Session {
	h.t.Helper()
	s, err := h.st.Session(bg, sessionID)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func newClientID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// turn sends a user message and returns it with the turn it started.
func (h *harness) turn(p auth.Person, sessionID, text string) (msg protocol.Message, turnID string) {
	h.t.Helper()
	m, err := h.svc.Send(bg, p, sessionID, protocol.UserMessage{ClientID: newClientID(),
		Content: []protocol.TextBlock{{Type: "text", Text: text}}})
	if err != nil || m == nil {
		h.t.Fatalf("Send: %v", err)
	}
	turnID, ok := h.svc.RunningTurn(sessionID)
	if !ok {
		h.t.Fatal("Send started no turn")
	}
	return *m, turnID
}

// link delivers the conductor's events for a turn and waits until the
// session's actor has applied them.
func (h *harness) link(sessionID, turnID string, evs ...protocol.LinkEvent) {
	h.t.Helper()
	for _, ev := range evs {
		h.svc.OnLinkEvent(sessionID, turnID, ev)
	}
	if err := h.svc.Barrier(bg); err != nil {
		h.t.Fatal(err)
	}
}

// fetch is the WebFetch tool.started that taints a session (P9).
func fetch() protocol.LinkEvent {
	return protocol.LinkToolStarted{ToolUseID: "toolu_fetch", Name: "WebFetch", Summary: "https://example.test/page"}
}

// finish ends a turn with its result.
func (h *harness) finish(sessionID, turnID string) {
	h.t.Helper()
	h.link(sessionID, turnID, protocol.LinkMessageStarted{Model: "claude-haiku-5-5"}, protocol.TurnResult{
		SDKUUID: "sdk-" + turnID, Content: []protocol.MessagePart{{Type: "text", Text: "ok"}}})
	if _, running := h.svc.RunningTurn(sessionID); running {
		h.t.Fatal("the turn is still running after its result")
	}
}

// taintedTurn starts a turn whose WebFetch tainted the session.
func (h *harness) taintedTurn(p auth.Person, sessionID string) string {
	h.t.Helper()
	_, turnID := h.turn(p, sessionID, "read this page")
	h.link(sessionID, turnID, fetch())
	return turnID
}

// setProfile replaces the person's profile, as the editor would.
func (h *harness) setProfile(p auth.Person, content string) {
	h.t.Helper()
	if err := h.st.InMemoryTx(bg, p.ID, func(m *store.MemoryTx) error {
		cur, err := m.Profile(bg)
		if err != nil {
			return err
		}
		_, err = m.SetProfile(bg, content, cur.Version)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) profile(p auth.Person) store.Profile {
	h.t.Helper()
	prof, err := h.st.Profile(bg, p.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return prof
}

// writes lists the person's memory_writes rows, oldest first.
func (h *harness) writes(p auth.Person) []store.MemoryWrite {
	h.t.Helper()
	ws, err := h.st.MemoryWrites(bg, p.ID, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	for i, j := 0, len(ws)-1; i < j; i, j = i+1, j-1 {
		ws[i], ws[j] = ws[j], ws[i]
	}
	return ws
}

func (h *harness) write(id string) store.MemoryWrite {
	h.t.Helper()
	w, err := h.st.MemoryWrite(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *harness) remember(sess store.Session, turnID, text string, flagged bool) protocol.MemoryResult {
	h.t.Helper()
	r, err := h.mem.Remember(bg, sess, turnID, text, flagged)
	if err != nil {
		h.t.Fatalf("Remember(%q): %v", text, err)
	}
	return r
}

// forget is an unflagged Forget (a flagged one is called directly).
func (h *harness) forget(sess store.Session, turnID, query string) protocol.MemoryResult {
	h.t.Helper()
	r, err := h.mem.Forget(bg, sess, turnID, query, false)
	if err != nil {
		h.t.Fatalf("Forget(%q): %v", query, err)
	}
	return r
}

// memoryEvents returns the memory.write events of a session's log.
func (h *harness) memoryEvents(sessionID string) []protocol.MemoryWrite {
	h.t.Helper()
	evs, err := h.st.EventsSince(bg, sessionID, 0, 100000)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []protocol.MemoryWrite
	for _, e := range evs {
		if b, ok := e.Body.(protocol.MemoryWriteEvent); ok {
			out = append(out, b.Write)
		}
	}
	return out
}

// ---- the web harness: the real BFF in front of the routes, as Task 15 wires them ----

const devCookie = "luma_dev" // the session cookie's name for an http public URL (S17)

const webRegistry = `people:
  pers-chris:
    display_name: Synthetic Chris
    tier: admin
    emails:
      mock: [chris@example.test]
  pers-erin:
    display_name: Synthetic Erin
    tier: family
    emails:
      mock: [erin@example.test]
`

type web struct {
	*harness
	srv    *httptest.Server
	origin string
}

// newWeb serves Memory's routes behind the BFF's RequirePerson, the whole
// mux inside UnsafeRequestGuard.
func newWeb(t *testing.T) *web {
	t.Helper()
	w := &web{harness: newHarness(t), srv: httptest.NewUnstartedServer(nil)}
	w.origin = "http://" + w.srv.Listener.Addr().String()
	regPath := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(regPath, []byte(webRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := auth.LoadRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	iss := oidctest.New(t, "luma", "synthetic-secret", w.origin+"/auth/callback")
	bff, err := auth.NewBFF(t.Context(), auth.BFFConfig{
		PublicURL: w.origin, Issuer: iss.URL, ClientID: "luma", ClientSecret: "synthetic-secret",
		SigningKey: bytes.Repeat([]byte{0x5a}, 32), AllowedOrigins: []string{w.origin},
	}, w.st, reg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	bff.Routes(mux)
	w.mem.Routes(mux, bff.RequirePerson)
	w.srv.Config.Handler = auth.UnsafeRequestGuard([]string{w.origin})(mux)
	w.srv.Start()
	t.Cleanup(w.srv.Close)
	return w
}

// login stores a web session for p, as a sign-in would, and returns its cookie.
func (w *web) login(p auth.Person) *http.Cookie {
	w.t.Helper()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	sum := sha256.Sum256(raw)
	email := map[string]string{chris.ID: "chris@example.test", erin.ID: "erin@example.test"}[p.ID]
	if err := w.st.CreateWebSession(bg, store.WebSession{
		IDHash: sum[:], PersonID: p.ID, ConnectorID: "mock", Email: email, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		w.t.Fatal(err)
	}
	return &http.Cookie{Name: devCookie, Value: base64.RawURLEncoding.EncodeToString(raw)}
}

type response struct {
	status int
	body   []byte
}

// do sends a same-origin request; a non-empty body goes as JSON.
func (w *web) do(method, path string, c *http.Cookie, body string) response {
	w.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rd)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Origin", w.origin)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := w.srv.Client().Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	return response{resp.StatusCode, b}
}

// profileOf decodes a Profile response.
func (w *web) profileOf(r response) protocol.Profile {
	w.t.Helper()
	if r.status != http.StatusOK {
		w.t.Fatalf("status %d (%s), want 200", r.status, r.body)
	}
	var p protocol.Profile
	if err := json.Unmarshal(r.body, &p); err != nil {
		w.t.Fatalf("decoding %s: %v", r.body, err)
	}
	return p
}

// errorOf checks a REST error's status and C3 body.
func (w *web) errorOf(r response, status int, code protocol.ErrorCode) {
	w.t.Helper()
	var info protocol.ErrorInfo
	if err := json.Unmarshal(r.body, &info); err != nil || r.status != status || info.Code != code ||
		info.Message != protocol.ErrorCopy[code] {
		w.t.Fatalf("response = %d %s, want %d %s with its copy", r.status, r.body, status, code)
	}
}

func putBody(content string, version int) string {
	b, _ := json.Marshal(protocol.ProfileUpdate{Content: content, Version: version})
	return string(b)
}
