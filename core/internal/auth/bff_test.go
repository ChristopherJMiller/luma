package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/auth/oidctest"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

const (
	clientID     = "luma-core"
	clientSecret = "test-client-secret-not-a-real-one"
	publicHTTPS  = "https://luma.example.test"
	publicHTTP   = "http://luma.localtest:8080"
	foreign      = "https://evil.example"
)

// registryYAML is the brief's registry: one person p1 with one address under
// each of two connectors.
const registryYAML = `people:
  p1:
    display_name: Person One
    tier: admin
    household: true
    emails:
      google: [a@example.com]
      microsoft: [b@example.net]
`

var (
	signingKey = bytes.Repeat([]byte{0x42}, 32)
	viaGoogle  = oidctest.Identity{Subject: "sub-a", Email: "a@example.com", EmailVerified: true, Name: "A", ConnectorID: "google"}
)

// fakeClock is the real clock plus an offset the test advances, so rows the
// database stamps with now() agree with the BFF's view of time.
type fakeClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off += d
}

type ended struct {
	hash     []byte
	personID string
}

type harness struct {
	t         *testing.T
	publicURL string
	st        *store.Store
	iss       *oidctest.Issuer
	bff       *auth.BFF
	handler   http.Handler
	regPath   string
	clock     *fakeClock
	ended     chan ended
}

// newHarness builds a BFF against a throwaway Postgres and the fake issuer.
// The handler is wired as Task 15 wires the public port: the whole mux inside
// UnsafeRequestGuard, and GET /v1/whoami behind RequirePerson.
func newHarness(t *testing.T, publicURL string) *harness {
	t.Helper()
	h := &harness{t: t, publicURL: publicURL, st: storetest.New(t), clock: &fakeClock{}, ended: make(chan ended, 16)}
	h.iss = oidctest.New(t, clientID, clientSecret, publicURL+"/auth/callback")
	h.regPath = filepath.Join(t.TempDir(), "registry.yaml")
	writeFile(t, h.regPath, registryYAML)
	reg, err := auth.LoadRegistry(h.regPath)
	if err != nil {
		t.Fatal(err)
	}
	h.bff = h.newBFF(signingKey, reg)
	h.bff.OnSessionEnded(func(hash []byte, personID string) { h.ended <- ended{hash, personID} })
	mux := http.NewServeMux()
	h.bff.Routes(mux)
	mux.Handle("/v1/whoami", h.bff.RequirePerson(http.HandlerFunc(whoami)))
	h.handler = auth.UnsafeRequestGuard([]string{publicURL})(mux)
	return h
}

func (h *harness) newBFF(key []byte, reg *auth.Registry) *auth.BFF {
	h.t.Helper()
	b, err := auth.NewBFF(h.t.Context(), auth.BFFConfig{
		PublicURL: h.publicURL, Issuer: h.iss.URL, ClientID: clientID, ClientSecret: clientSecret,
		SigningKey: key, AbsoluteTTL: 30 * 24 * time.Hour, IdleTTL: 14 * 24 * time.Hour,
		AllowedOrigins: []string{h.publicURL},
	}, h.st, reg)
	if err != nil {
		h.t.Fatalf("NewBFF: %v", err)
	}
	auth.SetClock(b, h.clock.now)
	return b
}

func whoami(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PersonFrom(r.Context())
	if !ok {
		http.Error(w, "no person in context", http.StatusInternalServerError)
		return
	}
	hash, ok := auth.WebSessionIDHashFrom(r.Context())
	if !ok || len(hash) != sha256.Size {
		http.Error(w, "no web session in context", http.StatusInternalServerError)
		return
	}
	_, _ = fmt.Fprintf(w, "%s|%s|%s|%t", p.ID, p.DisplayName, p.Tier, p.Household)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, h.publicURL+path, nil)
	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	return h.serve(req)
}

// cookieNamed returns the Set-Cookie of a response with that name.
func cookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// rawSetCookie returns the raw Set-Cookie line for a cookie name.
func rawSetCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) string {
	t.Helper()
	for _, line := range rec.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(line, name+"=") {
			return line
		}
	}
	return ""
}

func (h *harness) sessionName() string {
	if strings.HasPrefix(h.publicURL, "https://") {
		return "__Host-luma"
	}
	return "luma_dev"
}

func (h *harness) loginName() string {
	if strings.HasPrefix(h.publicURL, "https://") {
		return "__Host-luma-login"
	}
	return "luma_login_dev"
}

// startLogin calls /auth/login and returns its login cookie and the
// authorization URL it redirects to.
func (h *harness) startLogin(returnTo string) (*http.Cookie, *url.URL, *httptest.ResponseRecorder) {
	h.t.Helper()
	path := "/auth/login"
	if returnTo != "" {
		path += "?" + url.Values{"return_to": {returnTo}}.Encode()
	}
	rec := h.get(path)
	if rec.Code != http.StatusFound {
		h.t.Fatalf("/auth/login = %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	lc := cookieNamed(h.t, rec, h.loginName())
	if lc == nil {
		h.t.Fatalf("/auth/login set no %s cookie", h.loginName())
	}
	return lc, loc, rec
}

// authorize follows the authorization URL at the fake issuer (which signs in
// its identity at once) and returns the callback URL it redirects to.
func (h *harness) authorize(authURL *url.URL) *url.URL {
	h.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL.String())
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("issuer /auth = %d %s", resp.StatusCode, body)
	}
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	if got := cb.Scheme + "://" + cb.Host + cb.Path; got != h.publicURL+"/auth/callback" {
		h.t.Fatalf("issuer redirected to %s", got)
	}
	return cb
}

func (h *harness) callback(cb *url.URL, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return h.get(cb.Path+"?"+cb.RawQuery, cookies...)
}

// login runs the whole flow as id and returns the callback response.
func (h *harness) login(id oidctest.Identity, returnTo string, extra ...*http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	h.iss.SetIdentity(id)
	lc, authURL, _ := h.startLogin(returnTo)
	return h.callback(h.authorize(authURL), append([]*http.Cookie{lc}, extra...)...)
}

// mustLogin logs in and returns the session cookie.
func (h *harness) mustLogin(id oidctest.Identity) *http.Cookie {
	h.t.Helper()
	rec := h.login(id, "")
	if rec.Code != http.StatusFound {
		h.t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
	c := cookieNamed(h.t, rec, h.sessionName())
	if c == nil || c.Value == "" {
		h.t.Fatalf("callback set no %s cookie", h.sessionName())
	}
	return c
}

type row struct{ personID, connectorID, email string }

// rows reads web_sessions directly, as the superuser.
func (h *harness) rows() map[string]row {
	h.t.Helper()
	conn := storetest.ConnAs(h.t, h.st, storetest.Superuser)
	rs, err := conn.Query(context.Background(), `select id_hash, person_id, connector_id, email from web_sessions`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]row{}
	for rs.Next() {
		var hash []byte
		var r row
		if err := rs.Scan(&hash, &r.personID, &r.connectorID, &r.email); err != nil {
			h.t.Fatal(err)
		}
		out[string(hash)] = r
	}
	if err := rs.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// hashOf is the DB key of a session cookie value: sha256 of its 32 bytes.
func hashOf(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 32 {
		t.Fatalf("session cookie %q is not 32 bytes of base64url: %v", value, err)
	}
	sum := sha256.Sum256(raw)
	return sum[:]
}

// expectEnded waits for OnSessionEnded for one session. Every harness
// session belongs to p1, the registry's one person.
func (h *harness) expectEnded(hash []byte) {
	h.t.Helper()
	select {
	case e := <-h.ended:
		if !bytes.Equal(e.hash, hash) || e.personID != "p1" {
			h.t.Fatalf("OnSessionEnded(%x, %q), want (%x, \"p1\")", e.hash, e.personID, hash)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("OnSessionEnded did not fire")
	}
}

func (h *harness) expectNoEnded() {
	h.t.Helper()
	select {
	case e := <-h.ended:
		h.t.Fatalf("unexpected OnSessionEnded(%x, %q)", e.hash, e.personID)
	default:
	}
}

func TestLoginRoundTrip(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	h.iss.SetIdentity(viaGoogle)

	lc, authURL, _ := h.startLogin("/profile")
	if got := authURL.Scheme + "://" + authURL.Host + authURL.Path; got != h.iss.URL+"/auth" {
		t.Fatalf("login redirected to %s, want the issuer's /auth", got)
	}
	q := authURL.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             clientID,
		"redirect_uri":          publicHTTPS + "/auth/callback",
		"scope":                 "openid email profile federated:id",
		"code_challenge_method": "S256",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("auth request %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q.Get("code_challenge")) != 43 || len(q.Get("state")) < 32 || len(q.Get("nonce")) < 32 {
		t.Errorf("auth request challenge/state/nonce = %q / %q / %q", q.Get("code_challenge"), q.Get("state"), q.Get("nonce"))
	}

	rec := h.callback(h.authorize(authURL), lc)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/profile" {
		t.Fatalf("callback = %d Location %q, want 302 /profile (%s)", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	sc := cookieNamed(t, rec, "__Host-luma")
	if sc == nil {
		t.Fatal("no session cookie")
	}

	toks := h.iss.TokenRequests()
	if len(toks) != 1 {
		t.Fatalf("%d token requests, want 1", len(toks))
	}
	tr := toks[0]
	if !tr.HasBasic || tr.BasicUser != clientID || tr.BasicPass != clientSecret {
		t.Errorf("token request did not use client_secret_basic: %+v", tr)
	}
	if tr.Form.Has("client_secret") {
		t.Error("client_secret sent in the token request body")
	}
	if tr.Form.Get("grant_type") != "authorization_code" || len(tr.Form.Get("code_verifier")) != 43 {
		t.Errorf("token form = %v", tr.Form)
	}

	rows := h.rows()
	if r, ok := rows[string(hashOf(t, sc.Value))]; len(rows) != 1 || !ok ||
		r != (row{personID: "p1", connectorID: "google", email: "a@example.com"}) {
		t.Fatalf("web_sessions = %+v", rows)
	}

	who := h.get("/v1/whoami", sc)
	if who.Code != http.StatusOK || who.Body.String() != "p1|Person One|admin|true" {
		t.Fatalf("whoami = %d %q", who.Code, who.Body)
	}
}

func TestCallbackStateMismatch400(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	h.iss.SetIdentity(viaGoogle)

	lc, authURL, _ := h.startLogin("")
	cb := h.authorize(authURL)
	q := cb.Query()
	q.Set("state", "forged-state")
	cb.RawQuery = q.Encode()
	rec := h.callback(cb, lc)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state = %d, want 400", rec.Code)
	}
	if cookieNamed(t, rec, "__Host-luma") != nil || len(h.rows()) != 0 {
		t.Fatal("a session was created for a forged state")
	}
	if n := len(h.iss.TokenRequests()); n != 0 {
		t.Fatalf("the code was exchanged %d times despite the state mismatch", n)
	}

	// No login cookie at all is the same refusal.
	_, authURL, _ = h.startLogin("")
	if rec := h.callback(h.authorize(authURL)); rec.Code != http.StatusBadRequest {
		t.Fatalf("callback without a login cookie = %d, want 400", rec.Code)
	}
}

func TestNonceMismatchRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	h.iss.OverrideNonce("a-nonce-this-login-never-sent")
	rec := h.login(viaGoogle, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("nonce mismatch = %d, want 400", rec.Code)
	}
	if cookieNamed(t, rec, "__Host-luma") != nil || len(h.rows()) != 0 {
		t.Fatal("a session was created despite the nonce mismatch")
	}
}

func TestCallbackRotatesSessionID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)

	// A live session presented at a new login is replaced and its row deleted.
	first := h.mustLogin(viaGoogle)
	rec := h.login(viaGoogle, "", first)
	second := cookieNamed(t, rec, "__Host-luma")
	if rec.Code != http.StatusFound || second == nil {
		t.Fatalf("second login = %d", rec.Code)
	}
	if second.Value == first.Value {
		t.Fatal("the callback reused the presented session id")
	}
	rows := h.rows()
	if _, ok := rows[string(hashOf(t, first.Value))]; ok {
		t.Fatal("the presented session's row survived the login")
	}
	if _, ok := rows[string(hashOf(t, second.Value))]; !ok || len(rows) != 1 {
		t.Fatalf("web_sessions = %+v, want only the new session", rows)
	}
	h.expectEnded(hashOf(t, first.Value))

	// A planted value (session fixation) is never the one issued.
	planted := make([]byte, 32)
	_, _ = rand.Read(planted)
	plantedValue := base64.RawURLEncoding.EncodeToString(planted)
	rec = h.login(viaGoogle, "", &http.Cookie{Name: "__Host-luma", Value: plantedValue})
	third := cookieNamed(t, rec, "__Host-luma")
	if third == nil || third.Value == plantedValue {
		t.Fatalf("the callback issued the planted session id (%v)", third)
	}
	if _, ok := h.rows()[string(hashOf(t, plantedValue))]; ok {
		t.Fatal("a row exists for the planted id")
	}
}

func TestNotInvited403(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	rec := h.login(oidctest.Identity{Subject: "s", Email: "stranger@example.org", EmailVerified: true, ConnectorID: "google"}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("uninvited login = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "This account isn't invited to Luma.") {
		t.Fatalf("403 body = %q, want the C3 not_invited copy", rec.Body)
	}
	if cookieNamed(t, rec, "__Host-luma") != nil || len(h.rows()) != 0 {
		t.Fatal("a session was created for an uninvited account")
	}
}

func TestEmailUnverifiedRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	id := viaGoogle
	id.EmailVerified = false
	rec := h.login(id, "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "This account isn't invited to Luma.") {
		t.Fatalf("unverified email = %d %q, want 403 not_invited", rec.Code, rec.Body)
	}
	if len(h.rows()) != 0 {
		t.Fatal("a session was created for an unverified email")
	}
}

func TestLoginViaEachRegisteredConnector(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	g := h.mustLogin(oidctest.Identity{Subject: "g", Email: "a@example.com", EmailVerified: true, ConnectorID: "google"})
	m := h.mustLogin(oidctest.Identity{Subject: "m", Email: "b@example.net", EmailVerified: true, ConnectorID: "microsoft"})

	rows := h.rows()
	if len(rows) != 2 {
		t.Fatalf("web_sessions = %+v, want 2 rows", rows)
	}
	if r := rows[string(hashOf(t, g.Value))]; r != (row{"p1", "google", "a@example.com"}) {
		t.Errorf("google session row = %+v", r)
	}
	if r := rows[string(hashOf(t, m.Value))]; r != (row{"p1", "microsoft", "b@example.net"}) {
		t.Errorf("microsoft session row = %+v", r)
	}
	for _, c := range []*http.Cookie{g, m} {
		if who := h.get("/v1/whoami", c); who.Body.String() != "p1|Person One|admin|true" {
			t.Errorf("whoami = %d %q", who.Code, who.Body)
		}
	}
}

func TestConnectorPinned(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	cases := map[string]oidctest.Identity{
		"google address via microsoft":    {Subject: "1", Email: "a@example.com", EmailVerified: true, ConnectorID: "microsoft"},
		"microsoft address via google":    {Subject: "2", Email: "b@example.net", EmailVerified: true, ConnectorID: "google"},
		"listed address via github":       {Subject: "3", Email: "a@example.com", EmailVerified: true, ConnectorID: "github"},
		"listed address, no federated id": {Subject: "4", Email: "a@example.com", EmailVerified: true},
	}
	for name, id := range cases {
		rec := h.login(id, "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "This account isn't invited to Luma.") {
			t.Errorf("%s: %d %q, want 403 not_invited", name, rec.Code, rec.Body)
		}
		if cookieNamed(t, rec, "__Host-luma") != nil {
			t.Errorf("%s: a session cookie was set", name)
		}
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("web_sessions = %+v, want none", rows)
	}
}

// parseRaw parses one raw Set-Cookie line and returns its attributes by
// lower-cased name (flags map to "").
func parseRaw(line string) map[string]string {
	attrs := map[string]string{}
	for i, part := range strings.Split(line, ";") {
		part = strings.TrimSpace(part)
		if i == 0 {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		attrs[strings.ToLower(k)] = v
	}
	return attrs
}

func TestCookieAttributes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	h.iss.SetIdentity(viaGoogle)
	lc, authURL, loginRec := h.startLogin("")

	wantLogin := map[string]string{"path": "/", "httponly": "", "secure": "", "samesite": "Lax", "max-age": "600"}
	if got := parseRaw(rawSetCookie(t, loginRec, "__Host-luma-login")); !equalMaps(got, wantLogin) {
		t.Errorf("login cookie attributes = %v, want %v", got, wantLogin)
	}

	rec := h.callback(h.authorize(authURL), lc)
	wantSession := map[string]string{"path": "/", "httponly": "", "secure": "", "samesite": "Lax", "max-age": "2592000"}
	if got := parseRaw(rawSetCookie(t, rec, "__Host-luma")); !equalMaps(got, wantSession) {
		t.Errorf("session cookie attributes = %v, want %v", got, wantSession)
	}
	// The single-use login cookie is cleared with the attributes a __Host-
	// cookie needs for the browser to accept the deletion.
	wantCleared := map[string]string{"path": "/", "httponly": "", "secure": "", "samesite": "Lax", "max-age": "0"}
	if got := parseRaw(rawSetCookie(t, rec, "__Host-luma-login")); !equalMaps(got, wantCleared) {
		t.Errorf("cleared login cookie attributes = %v, want %v", got, wantCleared)
	}
	sc := cookieNamed(t, rec, "__Host-luma")
	if len(sc.Value) != 43 {
		t.Errorf("session cookie value %q is not 43 base64url characters", sc.Value)
	}
	if _, ok := h.rows()[string(hashOf(t, sc.Value))]; !ok {
		t.Error("the DB does not key the session by sha256 of the cookie's 32 bytes")
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestHTTPPublicURLUsesDevCookieNames(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTP)
	h.iss.SetIdentity(viaGoogle)
	lc, authURL, loginRec := h.startLogin("")
	if lc.Name != "luma_login_dev" {
		t.Fatalf("login cookie %q, want luma_login_dev", lc.Name)
	}
	if _, secure := parseRaw(rawSetCookie(t, loginRec, "luma_login_dev"))["secure"]; secure {
		t.Error("luma_login_dev is Secure over http")
	}
	rec := h.callback(h.authorize(authURL), lc)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
	if _, secure := parseRaw(rawSetCookie(t, rec, "luma_dev"))["secure"]; secure {
		t.Error("luma_dev is Secure over http")
	}
	for _, line := range append(loginRec.Result().Header.Values("Set-Cookie"), rec.Result().Header.Values("Set-Cookie")...) {
		if strings.HasPrefix(line, "__Host-") {
			t.Errorf("a __Host- cookie was set over http: %s", line)
		}
	}
	sc := cookieNamed(t, rec, "luma_dev")
	if who := h.get("/v1/whoami", sc); who.Code != http.StatusOK {
		t.Fatalf("whoami with luma_dev = %d", who.Code)
	}
}

func TestReturnToAllowlist(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	cases := map[string]string{
		"//evil":                            "/",
		"https://evil":                      "/",
		`/\evil.com`:                        "/",
		"/%5Cevil.com":                      "/",
		"/%2F%2Fevil":                       "/",
		"/c/../x":                           "/",
		"/c/ses_bad":                        "/",
		"/profile\n":                        "/",
		"/profile/":                         "/",
		"":                                  "/",
		"/c/ses_01JABCDEFGHJKMNPQRSTVWXYZ0": "/c/ses_01JABCDEFGHJKMNPQRSTVWXYZ0",
		"/profile":                          "/profile",
		"/":                                 "/",
	}
	for in, want := range cases {
		rec := h.login(viaGoogle, in)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
			t.Errorf("return_to %q → %d %q, want %q", in, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}

func TestNewBFFValidatesConfig(t *testing.T) {
	t.Parallel()
	good := auth.BFFConfig{
		PublicURL: publicHTTPS, Issuer: "http://127.0.0.1:1", ClientID: clientID, ClientSecret: clientSecret,
		SigningKey: signingKey, AbsoluteTTL: time.Hour, IdleTTL: time.Hour,
	}
	reg := mustParseRegistry(t, registryYAML)
	bad := map[string]func(c *auth.BFFConfig){
		"31-byte signing key":  func(c *auth.BFFConfig) { c.SigningKey = signingKey[:31] },
		"empty client secret":  func(c *auth.BFFConfig) { c.ClientSecret = "" },
		"empty client id":      func(c *auth.BFFConfig) { c.ClientID = "" },
		"empty issuer":         func(c *auth.BFFConfig) { c.Issuer = "" },
		"public URL with path": func(c *auth.BFFConfig) { c.PublicURL = publicHTTPS + "/luma" },
		"public URL ftp":       func(c *auth.BFFConfig) { c.PublicURL = "ftp://luma.example.test" },
		"origin with a path":   func(c *auth.BFFConfig) { c.AllowedOrigins = []string{publicHTTPS + "/x"} },
		"negative idle TTL":    func(c *auth.BFFConfig) { c.IdleTTL = -time.Second },
	}
	for name, mutate := range bad {
		c := good
		mutate(&c)
		if _, err := auth.NewBFF(t.Context(), c, &store.Store{}, reg); err == nil {
			t.Errorf("%s: NewBFF accepted it", name)
		}
	}
	if _, err := auth.NewBFF(t.Context(), good, nil, reg); err == nil {
		t.Error("NewBFF accepted a nil store")
	}
	if _, err := auth.NewBFF(t.Context(), good, &store.Store{}, nil); err == nil {
		t.Error("NewBFF accepted a nil registry")
	}
}

// An unreachable issuer at start is not fatal: sessions need only the DB,
// and sign-in retries discovery.
func TestNewBFFToleratesIssuerDown(t *testing.T) {
	t.Parallel()
	reg := mustParseRegistry(t, registryYAML)
	b, err := auth.NewBFF(t.Context(), auth.BFFConfig{
		PublicURL: publicHTTPS, Issuer: "http://127.0.0.1:1", ClientID: clientID, ClientSecret: clientSecret,
		SigningKey: signingKey,
	}, &store.Store{}, reg)
	if err != nil {
		t.Fatalf("NewBFF with the issuer down: %v", err)
	}
	mux := http.NewServeMux()
	b.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, publicHTTPS+"/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/auth/login with the issuer down = %d, want 503", rec.Code)
	}
}

func mustParseRegistry(t *testing.T, content string) *auth.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	writeFile(t, path, content)
	reg, err := auth.LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}
