package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
	"github.com/ChristopherJMiller/luma/core/internal/auth/oidctest"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

var viaMicrosoft = oidctest.Identity{Subject: "sub-b", Email: "b@example.net", EmailVerified: true, ConnectorID: "microsoft"}

func (h *harness) lastSeen(hash []byte) time.Time {
	h.t.Helper()
	var ts time.Time
	conn := storetest.ConnAs(h.t, h.st, storetest.Superuser)
	if err := conn.QueryRow(context.Background(), `select last_seen_at from web_sessions where id_hash = $1`, hash).Scan(&ts); err != nil {
		h.t.Fatal(err)
	}
	return ts
}

func TestIdleExpiry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	// Both log in at offset 0, where the DB's now() stamps last_seen_at.
	active := h.mustLogin(viaGoogle)
	idle := h.mustLogin(viaMicrosoft)

	h.clock.advance(13 * 24 * time.Hour)
	if who := h.get("/v1/whoami", active); who.Code != http.StatusOK {
		t.Fatalf("after 13 idle days: %d, want 200", who.Code)
	}

	// 14 days and a minute without a request (inside the 30-day cap) ends a
	// session; activity a day ago keeps the other.
	h.clock.advance(24*time.Hour + time.Minute)
	if who := h.get("/v1/whoami", idle); who.Code != http.StatusUnauthorized {
		t.Fatalf("after 14 idle days: %d, want 401", who.Code)
	}
	if _, ok := h.rows()[string(hashOf(t, idle.Value))]; ok {
		t.Fatal("the idle session's row survived")
	}
	h.expectEnded(hashOf(t, idle.Value))
	if who := h.get("/v1/whoami", active); who.Code != http.StatusOK {
		t.Fatalf("the active session, 1 day idle: %d, want 200", who.Code)
	}

	// Activity inside each window keeps a session alive past 14 days total.
	h.clock.advance(13 * 24 * time.Hour)
	if who := h.get("/v1/whoami", active); who.Code != http.StatusOK {
		t.Fatalf("28 days in, 13 idle: %d, want 200", who.Code)
	}
	h.expectNoEnded()
}

func TestAbsoluteExpiry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	sc := h.mustLogin(viaGoogle)
	quiet := h.mustLogin(viaMicrosoft)      // no request ever comes back for it
	for _, days := range []int{10, 10, 9} { // active all along, still capped at 30 days
		h.clock.advance(time.Duration(days) * 24 * time.Hour)
		if who := h.get("/v1/whoami", sc); who.Code != http.StatusOK {
			t.Fatalf("after %d more days: %d, want 200", days, who.Code)
		}
	}
	h.clock.advance(24*time.Hour + time.Minute)
	// The next request after 30 days ends the session (row and sockets).
	if who := h.get("/v1/whoami", sc); who.Code != http.StatusUnauthorized {
		t.Fatalf("30 days after login: %d, want 401", who.Code)
	}
	h.expectEnded(hashOf(t, sc.Value))
	if _, ok := h.rows()[string(hashOf(t, sc.Value))]; ok {
		t.Fatal("the expired row survived the request that found it")
	}

	// A session no request comes back for is ended by the sweep.
	n, err := h.bff.Sweep(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v, want 1", n, err)
	}
	if len(h.rows()) != 0 {
		t.Fatal("the expired row survived the sweep")
	}
	h.expectEnded(hashOf(t, quiet.Value))
}

// Every path that deletes a row fires OnSessionEnded, for an expired row
// too: logout, and a new login replacing it.
func TestExpiredSessionsEndedByLogoutAndLogin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	a := h.mustLogin(viaGoogle)
	b := h.mustLogin(viaMicrosoft)
	h.clock.advance(31 * 24 * time.Hour)

	if rec := h.logout(a); rec.Code != http.StatusNoContent {
		t.Fatalf("logout of an expired session = %d, want 204", rec.Code)
	}
	h.expectEnded(hashOf(t, a.Value))

	rec := h.login(viaGoogle, "", b)
	if rec.Code != http.StatusFound {
		t.Fatalf("login presenting an expired session = %d", rec.Code)
	}
	h.expectEnded(hashOf(t, b.Value))
	h.expectNoEnded()
	if rows := h.rows(); len(rows) != 1 {
		t.Fatalf("web_sessions = %+v, want only the new login's", rows)
	}
}

func TestLastSeenWrittenAtMostEvery5Minutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	sc := h.mustLogin(viaGoogle)
	hash := hashOf(t, sc.Value)
	created := h.lastSeen(hash)

	h.clock.advance(4 * time.Minute)
	h.get("/v1/whoami", sc)
	if got := h.lastSeen(hash); !got.Equal(created) {
		t.Fatalf("last_seen_at written after 4 minutes: %v → %v", created, got)
	}

	h.clock.advance(2 * time.Minute)
	at := h.clock.now()
	h.get("/v1/whoami", sc)
	got := h.lastSeen(hash)
	if d := got.Sub(at); d < -time.Second || d > time.Second {
		t.Fatalf("after 6 minutes last_seen_at = %v, want ≈ %v", got, at)
	}

	h.clock.advance(time.Minute)
	h.get("/v1/whoami", sc)
	if again := h.lastSeen(hash); !again.Equal(got) {
		t.Fatalf("last_seen_at written 1 minute after the last write: %v → %v", got, again)
	}
}

// logout POSTs with no body and so no Content-Type (S42).
func (h *harness) logout(cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, h.publicURL+"/auth/logout", nil)
	req.Header.Set("Origin", h.publicURL)
	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	return h.serve(req)
}

func TestLogoutDeletesRowAndFiresOnSessionEnded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	sc := h.mustLogin(viaGoogle)
	other := h.mustLogin(viaMicrosoft) // another device stays signed in

	rec := h.logout(sc)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", rec.Code)
	}
	// A __Host- deletion is only honoured with Secure and Path=/ and no
	// Domain; the rest match the cookie it replaces.
	wantCleared := map[string]string{"path": "/", "httponly": "", "secure": "", "samesite": "Lax", "max-age": "0"}
	if got := parseRaw(rawSetCookie(t, rec, "__Host-luma")); !equalMaps(got, wantCleared) {
		t.Errorf("logout's session cookie clear = %v, want %v", got, wantCleared)
	}
	rows := h.rows()
	if _, ok := rows[string(hashOf(t, sc.Value))]; ok || len(rows) != 1 {
		t.Fatalf("web_sessions after logout = %+v", rows)
	}
	h.expectEnded(hashOf(t, sc.Value))
	h.expectNoEnded()
	if who := h.get("/v1/whoami", sc); who.Code != http.StatusUnauthorized {
		t.Fatalf("whoami after logout = %d, want 401", who.Code)
	}
	if who := h.get("/v1/whoami", other); who.Code != http.StatusOK {
		t.Fatalf("the other device after logout = %d, want 200", who.Code)
	}

	// Logging out without a session is still 204, and ends nothing.
	if rec := h.logout(); rec.Code != http.StatusNoContent {
		t.Fatalf("logout without a cookie = %d, want 204", rec.Code)
	}
	h.expectNoEnded()
}

func TestRequirePerson401(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	for name, c := range map[string]*http.Cookie{
		"no cookie":         nil,
		"garbage":           {Name: "__Host-luma", Value: "not-a-session"},
		"unknown id":        {Name: "__Host-luma", Value: strings.Repeat("A", 43)},
		"dev name on https": {Name: "luma_dev", Value: h.mustLogin(viaGoogle).Value},
	} {
		var cookies []*http.Cookie
		if c != nil {
			cookies = append(cookies, c)
		}
		if who := h.get("/v1/whoami", cookies...); who.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, who.Code)
		}
	}
}

// RequirePerson refuses a request carrying a foreign Origin (a cross-origin
// fetch, or a cross-site WebSocket handshake) and a WebSocket handshake with
// no Origin, before it looks at the cookie.
func TestRequirePersonChecksOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	sc := h.mustLogin(viaGoogle)
	req := func(origin string, ws bool) int {
		r := httptest.NewRequest(http.MethodGet, publicHTTPS+"/v1/whoami", nil)
		r.AddCookie(&http.Cookie{Name: sc.Name, Value: sc.Value})
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if ws {
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "websocket")
		}
		return h.serve(r).Code
	}
	if got := req(foreign, false); got != http.StatusForbidden {
		t.Errorf("GET with a foreign Origin = %d, want 403", got)
	}
	if got := req(foreign, true); got != http.StatusForbidden {
		t.Errorf("WebSocket with a foreign Origin = %d, want 403", got)
	}
	if got := req("", true); got != http.StatusForbidden {
		t.Errorf("WebSocket without an Origin = %d, want 403", got)
	}
	if got := req(publicHTTPS, true); got != http.StatusOK {
		t.Errorf("WebSocket from the public origin = %d, want 200", got)
	}
	if got := req("", false); got != http.StatusOK {
		t.Errorf("same-origin GET (no Origin header) = %d, want 200", got)
	}
}

const registryWithoutP1 = `people:
  p2:
    display_name: Person Two
    tier: family
    emails:
      google: [c@example.com]
`

func TestRegistryReloadRemovesAccess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	sc := h.mustLogin(viaGoogle)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- auth.WatchRegistry(ctx, h.regPath, h.bff.SetRegistry) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("WatchRegistry: %v", err)
		}
	})

	writeFile(t, h.regPath, registryWithoutP1)
	h.expectEnded(hashOf(t, sc.Value))
	if who := h.get("/v1/whoami", sc); who.Code != http.StatusUnauthorized {
		t.Fatalf("whoami after p1 left the registry = %d, want 401", who.Code)
	}
	if len(h.rows()) != 0 {
		t.Fatal("the removed person's session row survived")
	}
}

// An address removed from a person ends the sessions that came through it,
// and a tier change applies to the next request.
func TestSetRegistryEndsSessionsOfRemovedAddresses(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	g := h.mustLogin(viaGoogle)
	m := h.mustLogin(viaMicrosoft)

	h.bff.SetRegistry(mustParseRegistry(t, `people:
  p1:
    display_name: Person One
    tier: family
    household: true
    emails:
      google: [A@Example.com]
`))
	h.expectEnded(hashOf(t, m.Value))
	h.expectNoEnded()
	if who := h.get("/v1/whoami", m); who.Code != http.StatusUnauthorized {
		t.Fatalf("the microsoft session = %d, want 401", who.Code)
	}
	if who := h.get("/v1/whoami", g); who.Code != http.StatusOK || who.Body.String() != "p1|Person One|family|true" {
		t.Fatalf("the google session = %d %q, want 200 with the new tier", who.Code, who.Body)
	}

	// Re-adding the address does not bring the ended session back.
	h.bff.SetRegistry(mustParseRegistry(t, registryYAML))
	if who := h.get("/v1/whoami", m); who.Code != http.StatusUnauthorized {
		t.Fatalf("the ended microsoft session after re-adding the address = %d, want 401", who.Code)
	}
}
