package sessions

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/auth/oidctest"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// ---- the web harness: the real BFF in front of the session routes ----

// devCookie is the session cookie's name for an http PublicURL (S17).
const devCookie = "luma_dev"

// webRegistry admits the harness's three people through the mock connector.
const webRegistry = `people:
  pers-fam:
    display_name: Synthetic Family
    tier: family
    emails:
      mock: [fam@example.test]
  pers-oth:
    display_name: Synthetic Other
    tier: family
    emails:
      mock: [oth@example.test]
  pers-adm:
    display_name: Synthetic Admin
    tier: admin
    emails:
      mock: [adm@example.test]
`

var webEmail = map[string]string{family.ID: "fam@example.test", other.ID: "oth@example.test", admin.ID: "adm@example.test"}

// web serves the public routes as Task 15 wires them: the BFF's routes and
// the session routes behind its RequirePerson, the whole mux inside
// UnsafeRequestGuard, and OnSessionEnded wired to WebSessionEnded.
type web struct {
	h       *harness
	bff     *auth.BFF
	srv     *httptest.Server
	origin  string
	regPath string
}

// newWeb serves h.svc; build the service (h.service) before calling it.
// The allowed origins are the server's own plus extraOrigins.
func newWeb(h *harness, extraOrigins ...string) *web {
	t := h.t
	t.Helper()
	w := &web{h: h, srv: httptest.NewUnstartedServer(nil)}
	w.origin = "http://" + w.srv.Listener.Addr().String()
	origins := append([]string{w.origin}, extraOrigins...)
	w.regPath = filepath.Join(t.TempDir(), "registry.yaml")
	writeFile(t, w.regPath, webRegistry)
	reg, err := auth.LoadRegistry(w.regPath)
	if err != nil {
		t.Fatal(err)
	}
	iss := oidctest.New(t, "luma", "synthetic-secret", w.origin+"/auth/callback")
	w.bff, err = auth.NewBFF(t.Context(), auth.BFFConfig{
		PublicURL: w.origin, Issuer: iss.URL, ClientID: "luma", ClientSecret: "synthetic-secret",
		SigningKey: bytes.Repeat([]byte{0x5a}, 32), AllowedOrigins: origins,
	}, h.st, reg)
	if err != nil {
		t.Fatal(err)
	}
	w.bff.OnSessionEnded(h.svc.WebSessionEnded)
	mux := http.NewServeMux()
	w.bff.Routes(mux)
	h.svc.Routes(mux, w.bff.RequirePerson)
	w.srv.Config.Handler = auth.UnsafeRequestGuard(origins)(mux)
	w.srv.Start()
	t.Cleanup(w.srv.Close)
	return w
}

// login stores a web session for p, as a sign-in through the mock connector
// would, and returns its cookie.
func (w *web) login(p auth.Person) *http.Cookie {
	w.h.t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		w.h.t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if err := w.h.st.CreateWebSession(bg, store.WebSession{
		IDHash: sum[:], PersonID: p.ID, ConnectorID: "mock", Email: webEmail[p.ID], ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		w.h.t.Fatal(err)
	}
	return &http.Cookie{Name: devCookie, Value: base64.RawURLEncoding.EncodeToString(raw)}
}

// do sends a same-origin request, with a JSON body when body is not empty.
func (w *web) do(method, path string, c *http.Cookie, body string) (int, []byte) {
	w.h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rd)
	if err != nil {
		w.h.t.Fatal(err)
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
		w.h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		w.h.t.Fatal(err)
	}
	return resp.StatusCode, b
}

// json sends a request that must answer 200 and decodes the body into out.
func (w *web) json(method, path string, c *http.Cookie, body string, out any) {
	w.h.t.Helper()
	got, b := w.do(method, path, c, body)
	if got != http.StatusOK {
		w.h.t.Fatalf("%s %s = %d %s, want 200", method, path, got, b)
	}
	if err := json.Unmarshal(b, out); err != nil {
		w.h.t.Fatalf("%s %s: decode %s: %v", method, path, b, err)
	}
}

// logout signs the cookie's web session out through the BFF.
func (w *web) logout(c *http.Cookie) {
	w.h.t.Helper()
	if st, b := w.do(http.MethodPost, "/auth/logout", c, ""); st != http.StatusNoContent {
		w.h.t.Fatalf("logout = %d %s", st, b)
	}
}

// wantErrorInfo checks a REST error: the status, and a body that is exactly
// ErrorInfo{code, message} (C1, C3).
func wantErrorInfo(t *testing.T, status int, body []byte, wantStatus int, code protocol.ErrorCode, message string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d (%s), want %d", status, body, wantStatus)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("error body %q is not a JSON object: %v", body, err)
	}
	var gotCode, gotMessage string
	if len(m) != 2 || json.Unmarshal(m["code"], &gotCode) != nil || json.Unmarshal(m["message"], &gotMessage) != nil {
		t.Fatalf("error body %s, want exactly {code, message}", body)
	}
	if gotCode != string(code) || gotMessage != message {
		t.Fatalf("error body = {%q, %q}, want {%q, %q}", gotCode, gotMessage, code, message)
	}
}

// wantError is wantErrorInfo with the code's own C3 copy as the message.
func wantError(t *testing.T, status int, body []byte, wantStatus int, code protocol.ErrorCode) {
	t.Helper()
	wantErrorInfo(t, status, body, wantStatus, code, protocol.ErrorCopy[code])
}

func userMessageJSON(text string) string {
	return fmt.Sprintf(`{"type":"user.message","client_id":%q,"content":[{"type":"text","text":%q}],"attachment_ids":[]}`, newClientID(), text)
}

// fork stores a fork of root in its tree, as Task 11 will create one.
func (h *harness) fork(root protocol.Session) protocol.Session {
	h.t.Helper()
	row := h.row(root.ID)
	row.ID, row.RootID, row.ParentID = newID("ses_", h.clk.Now()), root.ID, &root.ID
	created, err := h.st.CreateSession(bg, row)
	if err != nil {
		h.t.Fatal(err)
	}
	return view(created)
}

func (h *harness) gone(sessionID string) bool {
	h.t.Helper()
	_, err := h.st.Session(bg, sessionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.t.Fatal(err)
	}
	return errors.Is(err, store.ErrNotFound)
}

// purgeConductor records tree purges and turn sends in one ordered log. A
// purge can be held until release closes, and returns err.
type purgeConductor struct {
	*fakeConductor
	mu      sync.Mutex
	log     []string // "send:<session>" once a turn.send returned, "purge:<root>:<ref>" when a purge starts
	started chan struct{}
	once    sync.Once
	release chan struct{}
	err     error
	hang    bool // never ack: return only when the caller's context ends
}

func newPurgeConductor() *purgeConductor {
	return &purgeConductor{fakeConductor: &fakeConductor{}, started: make(chan struct{})}
}

func (c *purgeConductor) SendTurn(ctx context.Context, ts protocol.TurnSend) error {
	err := c.fakeConductor.SendTurn(ctx, ts)
	c.mu.Lock()
	c.log = append(c.log, "send:"+ts.SessionID)
	c.mu.Unlock()
	return err
}

func (c *purgeConductor) PurgeTree(ctx context.Context, rootID, personRef string) error {
	c.mu.Lock()
	c.log = append(c.log, "purge:"+rootID+":"+personRef)
	release, err, hang := c.release, c.err, c.hang
	c.mu.Unlock()
	c.once.Do(func() { close(c.started) })
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if release != nil {
		<-release
	}
	return err
}

func (c *purgeConductor) Log() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.log)
}

func (h *harness) ref(p auth.Person) string {
	h.t.Helper()
	ref, err := h.st.PersonRef(bg, p.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return ref
}

// ---- tests ----

// S1: Create maps the Gate's refusals to 503 maintenance and 402
// budget_exhausted, with the Gate's ErrorInfo as the body, and writes no
// session; a Send is refused the same way.
func TestCreateMapsGateRefusals(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	c := w.login(family)
	cases := []struct {
		code   protocol.ErrorCode
		status int
	}{{protocol.CodeMaintenance, http.StatusServiceUnavailable}, {protocol.CodeBudgetExhausted, http.StatusPaymentRequired}}
	for _, tc := range cases {
		h.gate.set(tc.code)
		st, body := w.do(http.MethodPost, "/v1/sessions", c, `{"agent":"luma"}`)
		wantErrorInfo(t, st, body, tc.status, tc.code, protocol.ErrorCopy[tc.code])
	}
	if roots, err := h.st.ListRoots(bg, family.ID, true, nil, 10); err != nil || len(roots) != 0 {
		t.Fatalf("refused creates stored %d sessions (%v)", len(roots), err)
	}

	h.gate.set("")
	ses := h.newSession(family)
	for _, tc := range cases {
		h.gate.set(tc.code)
		st, body := w.do(http.MethodPost, "/v1/sessions/"+ses.ID+"/events", c, userMessageJSON("hi"))
		wantErrorInfo(t, st, body, tc.status, tc.code, protocol.ErrorCopy[tc.code])
	}
	if n := len(h.cond.Sends()); n != 0 {
		t.Fatalf("refused sends reached the conductor %d times", n)
	}
}

// The routes C1 lists for one session, end to end through the BFF; every
// REST error body a handler writes is ErrorInfo.
func TestSessionRoutes(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	c, oc := w.login(family), w.login(other)

	var ses protocol.Session
	w.json(http.MethodPost, "/v1/sessions", c, `{"agent":"luma","params":{"tier":"smart"},"source":"ci"}`, &ses)
	if ses.Tier != "smart" || ses.Source != "web" || ses.AgentID != "luma" || ses.RootID != ses.ID || ses.Status != "idle" {
		t.Fatalf("created %+v", ses)
	}
	var got protocol.Session
	w.json(http.MethodGet, "/v1/sessions/"+ses.ID, c, "", &got)
	if got.ID != ses.ID || got.Title != "" {
		t.Fatalf("GET = %+v", got)
	}

	for _, req := range [][2]string{
		{http.MethodGet, "/v1/sessions/" + ses.ID},
		{http.MethodPatch, "/v1/sessions/" + ses.ID},
		{http.MethodPost, "/v1/sessions/" + ses.ID + "/archive"},
		{http.MethodPost, "/v1/sessions/" + ses.ID + "/events"},
		{http.MethodDelete, "/v1/sessions/" + ses.ID},
		{http.MethodGet, "/v1/sessions/ses_00000000000000000000000000"},
	} {
		body := ""
		switch {
		case req[0] == http.MethodPatch:
			body = `{"title":"mine now"}`
		case strings.HasSuffix(req[1], "/events"):
			body = userMessageJSON("hello")
		}
		st, b := w.do(req[0], req[1], oc, body)
		wantError(t, st, b, http.StatusNotFound, protocol.CodeNotFound)
	}
	if h.gone(ses.ID) || h.row(ses.ID).Title != "" {
		t.Fatal("another person's requests changed the session")
	}

	w.json(http.MethodPatch, "/v1/sessions/"+ses.ID, c, `{"title":"  Trip \n plans  "}`, &got)
	if got.Title != "Trip plans" || h.row(ses.ID).Title != "Trip plans" {
		t.Fatalf("renamed title = %q, stored %q", got.Title, h.row(ses.ID).Title)
	}
	st, b := w.do(http.MethodPatch, "/v1/sessions/"+ses.ID, c, `{"title":"   "}`)
	wantError(t, st, b, http.StatusBadRequest, protocol.CodeInvalidRequest)

	w.json(http.MethodPost, "/v1/sessions/"+ses.ID+"/archive", c, "", &got)
	if got.ArchivedAt == nil {
		t.Fatal("archive left archived_at null")
	}
	var list protocol.SessionList
	w.json(http.MethodGet, "/v1/sessions?roots=true&archived=false", c, "", &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("an archived root is listed: %+v", list.Sessions)
	}
	w.json(http.MethodGet, "/v1/sessions?roots=true&archived=true", c, "", &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != ses.ID {
		t.Fatalf("archived=true lists %+v", list.Sessions)
	}
	w.json(http.MethodPost, "/v1/sessions/"+ses.ID+"/unarchive", c, "", &got)
	if got.ArchivedAt != nil {
		t.Fatal("unarchive kept archived_at")
	}

	var sent protocol.SendResult
	w.json(http.MethodPost, "/v1/sessions/"+ses.ID+"/events", c, userMessageJSON("plan a trip"), &sent)
	if sent.Message == nil || sent.Message.Role != "user" || len(sent.Message.Content) != 1 || sent.Message.Content[0].Text != "plan a trip" {
		t.Fatalf("send result = %+v", sent)
	}
	st, b = w.do(http.MethodPost, "/v1/sessions/"+ses.ID+"/events", c, userMessageJSON("and another"))
	wantErrorInfo(t, st, b, http.StatusConflict, protocol.CodeBusy, protocol.ErrorCopy[protocol.CodeBusy])
	st, b = w.do(http.MethodPost, "/v1/sessions/"+ses.ID+"/events", c, `{"type":"user.interrupt"}`)
	if st != http.StatusOK || string(b) != "{}" {
		t.Fatalf("interrupt = %d %s, want 200 {}", st, b)
	}
	if got := h.cond.Interrupts(); len(got) != 1 {
		t.Fatalf("interrupts = %v", got)
	}

	for _, bad := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/sessions", `{"params":{}}`},
		{http.MethodPost, "/v1/sessions", `{"agent":`},
		{http.MethodPost, "/v1/sessions/" + ses.ID + "/events", `{"type":"user.shout"}`},
		{http.MethodPost, "/v1/sessions/" + ses.ID + "/events", `{"type":"user.message","client_id":"not-a-uuid","content":[{"type":"text","text":"x"}],"attachment_ids":[]}`},
		{http.MethodGet, "/v1/sessions?roots=false", ""},
		{http.MethodGet, "/v1/sessions?archived=maybe", ""},
		{http.MethodGet, "/v1/sessions?after=bm90LWEtY3Vyc29y", ""},
	} {
		st, b := w.do(bad.method, bad.path, c, bad.body)
		wantError(t, st, b, http.StatusBadRequest, protocol.CodeInvalidRequest)
	}
	st, b = w.do(http.MethodPost, "/v1/sessions", c, `{"agent":"no-such-agent"}`)
	wantError(t, st, b, http.StatusNotFound, protocol.CodeNotFound)
	huge := fmt.Sprintf(`{"agent":"luma","pad":%q}`, strings.Repeat("x", 1<<20))
	st, b = w.do(http.MethodPost, "/v1/sessions", c, huge)
	wantError(t, st, b, http.StatusRequestEntityTooLarge, protocol.CodeRequestTooLarge)

	// Without a session the auth layer answers, status-only (S41).
	if st, b := w.do(http.MethodGet, "/v1/sessions/"+ses.ID, nil, ""); st != http.StatusUnauthorized || bytes.HasPrefix(b, []byte("{")) {
		t.Fatalf("no cookie = %d %s, want a plain 401", st, b)
	}
}

// GET /v1/sessions lists roots newest first in pages, with next as the
// cursor of the following page (C1); forks are never listed.
func TestListSessionsPages(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	c := w.login(family)
	var created []string
	for range 52 {
		created = append(created, h.newSession(family).ID)
	}
	h.fork(protocol.Session{ID: created[0], RootID: created[0]})
	h.newSession(other)
	slices.Reverse(created) // newest first

	var page1, page2 protocol.SessionList
	w.json(http.MethodGet, "/v1/sessions?roots=true&archived=false", c, "", &page1)
	if len(page1.Sessions) != 50 || page1.Next == "" {
		t.Fatalf("page 1 has %d sessions, next %q; want 50 and a cursor", len(page1.Sessions), page1.Next)
	}
	w.json(http.MethodGet, "/v1/sessions?roots=true&archived=false&after="+page1.Next, c, "", &page2)
	if len(page2.Sessions) != 2 || page2.Next != "" {
		t.Fatalf("page 2 has %d sessions, next %q; want 2 and none", len(page2.Sessions), page2.Next)
	}
	var listed []string
	for _, s := range append(page1.Sessions, page2.Sessions...) {
		listed = append(listed, s.ID)
	}
	if !slices.Equal(listed, created) {
		t.Fatalf("listed\n%v\nwant (newest first)\n%v", listed, created)
	}
	st, b := w.do(http.MethodGet, "/v1/sessions?after="+page1.Next, c, "")
	if st != http.StatusOK || !strings.Contains(string(b), `"sessions":[`) || strings.Contains(string(b), `"next"`) {
		t.Fatalf("defaults page 2 = %d %s", st, b)
	}
	w.json(http.MethodGet, "/v1/sessions", w.login(admin), "", &page1)
	if b, _ := json.Marshal(page1); len(page1.Sessions) != 0 || !strings.Contains(string(b), `"sessions":[]`) {
		t.Fatalf("an empty list = %s, want sessions: []", b)
	}
}

// DELETE on a fork deletes its whole tree (P3) and purges it with the
// owner's person ref; another person gets 404 and nothing is purged.
func TestDeleteDeletesTree(t *testing.T) {
	h := newHarness(t)
	pc := newPurgeConductor()
	h.svc = h.service(pc)
	w := newWeb(h)
	c := w.login(family)
	root := h.newSession(family)
	fork1, fork2 := h.fork(root), h.fork(root)
	h.send(family, fork1.ID, "in a fork")
	h.finish(fork1.ID)
	keep := h.newSession(family)

	st, b := w.do(http.MethodDelete, "/v1/sessions/"+keep.ID, w.login(other), "")
	wantError(t, st, b, http.StatusNotFound, protocol.CodeNotFound)

	if st, b := w.do(http.MethodDelete, "/v1/sessions/"+fork1.ID, c, ""); st != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("DELETE = %d %q, want 204 and no body", st, b)
	}
	for _, id := range []string{root.ID, fork1.ID, fork2.ID} {
		if !h.gone(id) {
			t.Fatalf("session %s survived its tree's delete", id)
		}
	}
	if h.gone(keep.ID) {
		t.Fatal("another tree was deleted")
	}
	var purges []string
	for _, e := range pc.Log() {
		if strings.HasPrefix(e, "purge:") {
			purges = append(purges, e)
		}
	}
	if want := []string{"purge:" + root.ID + ":" + h.ref(family)}; !slices.Equal(purges, want) {
		t.Fatalf("purges = %v, want %v", purges, want)
	}
	st, b = w.do(http.MethodDelete, "/v1/sessions/"+root.ID, c, "")
	wantError(t, st, b, http.StatusNotFound, protocol.CodeNotFound)
}

// S39: the tree's rows go only after its purge returns, and from the moment
// the delete starts a Send on any session of the tree is 404 and opens
// nothing. A purge that times out still deletes the rows, and a send already
// in flight finishes before the purge goes out.
func TestDeletePurgesBeforeRows(t *testing.T) {
	t.Run("rows go after the purge", func(t *testing.T) {
		h := newHarness(t)
		pc := newPurgeConductor()
		pc.release = make(chan struct{})
		release := sync.OnceFunc(func() { close(pc.release) })
		h.svc = h.service(pc)
		w := newWeb(h)
		t.Cleanup(release) // a failed test must not leave the DELETE, and so the server, waiting
		c := w.login(family)
		root := h.newSession(family)
		sib := h.fork(root)

		done := make(chan int, 1)
		go func() {
			st, _ := w.do(http.MethodDelete, "/v1/sessions/"+root.ID, c, "")
			done <- st
		}()
		select {
		case <-pc.started:
		case <-time.After(5 * time.Second):
			t.Fatal("the delete never purged the tree")
		}
		if h.gone(root.ID) || h.gone(sib.ID) {
			t.Fatal("rows were deleted while the purge was still waiting for its ack")
		}
		st, b := w.do(http.MethodPost, "/v1/sessions/"+sib.ID+"/events", c, userMessageJSON("are you there"))
		wantError(t, st, b, http.StatusNotFound, protocol.CodeNotFound)
		if _, err := h.svc.Send(bg, family, root.ID, userMsg("and here", newClientID())); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Send on the root mid-delete = %v, want ErrNotFound", err)
		}
		if n, m := len(pc.Opens()), len(pc.Sends()); n != 0 || m != 0 {
			t.Fatalf("a send mid-delete reached the conductor: %d opens, %d sends", n, m)
		}
		release()
		if st := <-done; st != http.StatusNoContent {
			t.Fatalf("DELETE = %d, want 204", st)
		}
		if !h.gone(root.ID) || !h.gone(sib.ID) {
			t.Fatal("rows survived the delete")
		}
	})

	t.Run("a purge timeout still deletes", func(t *testing.T) {
		h := newHarness(t)
		pc := newPurgeConductor()
		pc.err = errors.New("tree.purge: no tree.purged within 30s")
		h.svc = h.service(pc)
		root := h.newSession(family)
		sib := h.fork(root)
		if err := h.svc.DeleteTree(bg, family.ID, root.ID); err != nil {
			t.Fatalf("DeleteTree after a purge timeout = %v, want nil", err)
		}
		if !h.gone(root.ID) || !h.gone(sib.ID) {
			t.Fatal("a purge timeout left the rows")
		}
	})

	t.Run("a send in flight goes out before the purge", func(t *testing.T) {
		h := newHarness(t)
		pc := newPurgeConductor()
		h.svc = h.service(pc)
		root := h.newSession(family)
		sib := h.fork(root)
		inSend, letSend := make(chan struct{}), make(chan struct{})
		allow := sync.OnceFunc(func() { close(letSend) })
		t.Cleanup(allow)
		pc.onSend = func(protocol.TurnSend) {
			close(inSend)
			<-letSend
		}
		sent := make(chan error, 1)
		go func() {
			_, err := h.svc.Send(bg, family, sib.ID, userMsg("racing the delete", newClientID()))
			sent <- err
		}()
		<-inSend
		deleted := make(chan error, 1)
		go func() { deleted <- h.svc.DeleteTree(bg, family.ID, root.ID) }()
		time.Sleep(200 * time.Millisecond)
		if log := pc.Log(); len(log) != 0 {
			t.Fatalf("the purge went out while a send of the tree was in flight: %v", log)
		}
		allow()
		if err := <-sent; err != nil {
			t.Fatalf("the in-flight send = %v", err)
		}
		if err := <-deleted; err != nil {
			t.Fatalf("DeleteTree = %v", err)
		}
		if want := []string{"send:" + sib.ID, "purge:" + root.ID + ":" + h.ref(family)}; !slices.Equal(pc.Log(), want) {
			t.Fatalf("conductor saw %v, want %v", pc.Log(), want)
		}
	})
}

// deleteWithin runs DeleteTree for family's root, failing the test if it has
// not returned within d.
func deleteWithin(t *testing.T, h *harness, rootID string, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.svc.DeleteTree(bg, family.ID, rootID) }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("DeleteTree has not returned after %v", d)
		return nil
	}
}

// Defence in depth around S39: the barrier and the purge are each bounded
// (Task 10's PurgeTree has its own 30 s), and running out of either goes on
// with the delete as a lost link would.
func TestDeleteTreeBounded(t *testing.T) {
	t.Run("a purge that never acks", func(t *testing.T) {
		h := newHarness(t)
		pc := newPurgeConductor()
		pc.hang = true
		h.svc = h.service(pc)
		h.svc.deletions.purgeWait = 100 * time.Millisecond
		root := h.newSession(family)
		sib := h.fork(root)
		start := time.Now()
		if err := deleteWithin(t, h, root.ID, 5*time.Second); err != nil {
			t.Fatalf("DeleteTree = %v, want nil after the purge's bound", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("DeleteTree took %v with a 100 ms purge bound", d)
		}
		if !h.gone(root.ID) || !h.gone(sib.ID) {
			t.Fatal("rows survived a purge that timed out")
		}
	})

	t.Run("a barrier that never clears", func(t *testing.T) {
		h := newHarness(t)
		pc := newPurgeConductor()
		h.svc = h.service(pc)
		h.svc.deletions.barrierWait = 100 * time.Millisecond
		root := h.newSession(family)
		sib := h.fork(root)
		inSend, letSend := make(chan struct{}), make(chan struct{})
		sent := make(chan error, 1)
		t.Cleanup(func() { close(letSend); <-sent }) // the stuck send ends before the store closes
		pc.onSend = func(protocol.TurnSend) {
			close(inSend)
			<-letSend
		}
		go func() {
			_, err := h.svc.Send(bg, family, sib.ID, userMsg("stuck", newClientID()))
			sent <- err
		}()
		<-inSend
		if err := deleteWithin(t, h, root.ID, 5*time.Second); err != nil {
			t.Fatalf("DeleteTree = %v, want nil after the barrier's bound", err)
		}
		if want := []string{"purge:" + root.ID + ":" + h.ref(family)}; !slices.Equal(pc.Log(), want) {
			t.Fatalf("conductor saw %v, want %v", pc.Log(), want)
		}
		if !h.gone(root.ID) || !h.gone(sib.ID) {
			t.Fatal("rows survived a barrier that timed out")
		}
	})

	if deleteBarrierWait != 30*time.Second || deletePurgeWait != 35*time.Second {
		t.Fatalf("bounds = %v, %v; want 30s, 35s", deleteBarrierWait, deletePurgeWait)
	}
}

// A resend after a flaky POST (Review Focus 3): the same client_id answers
// 200 with the message stored the first time and starts no second turn, also
// once the Gate has closed (the id is reserved before it).
func TestRepeatedClientIDOverHTTP(t *testing.T) {
	h := newHarness(t)
	w := newWeb(h)
	c := w.login(family)
	ses := h.newSession(family)
	body := userMessageJSON("did this go through")
	var first, again, gated protocol.SendResult
	w.json(http.MethodPost, "/v1/sessions/"+ses.ID+"/events", c, body, &first)
	w.json(http.MethodPost, "/v1/sessions/"+ses.ID+"/events", c, body, &again)
	h.gate.set(protocol.CodeMaintenance)
	w.json(http.MethodPost, "/v1/sessions/"+ses.ID+"/events", c, body, &gated)
	if first.Message == nil || again.Message == nil || gated.Message == nil {
		t.Fatalf("results %+v %+v %+v, want a message each", first, again, gated)
	}
	if again.Message.ID != first.Message.ID || gated.Message.ID != first.Message.ID || again.Message.TurnID != first.Message.TurnID {
		t.Fatalf("repeats answered %s and %s, want the first message %s", again.Message.ID, gated.Message.ID, first.Message.ID)
	}
	if n := len(h.cond.Sends()); n != 1 {
		t.Fatalf("turn.send %d times, want 1", n)
	}
	if n := len(h.messages(ses.ID, "user")); n != 1 {
		t.Fatalf("%d user messages stored, want 1", n)
	}
}
