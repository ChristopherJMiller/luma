package auth_test

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginCookieIsEncrypted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	h.iss.SetIdentity(viaGoogle)
	lc, authURL, _ := h.startLogin("/profile")
	rec := h.callback(h.authorize(authURL), lc)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}

	// The secrets the cookie carries, as the issuer saw them.
	verifier := h.iss.TokenRequests()[0].Form.Get("code_verifier")
	authReq := h.iss.AuthRequests()[0]
	secrets := map[string]string{"verifier": verifier, "state": authReq.Get("state"), "nonce": authReq.Get("nonce"), "return_to": "/profile"}
	raw, err := base64.RawURLEncoding.DecodeString(lc.Value)
	if err != nil {
		t.Fatalf("login cookie is not base64url: %v", err)
	}
	for name, s := range secrets {
		if s == "" {
			t.Fatalf("no %s recorded", name)
		}
		if strings.Contains(lc.Value, s) || bytes.Contains(raw, []byte(s)) {
			t.Errorf("the login cookie carries the %s in the clear", name)
		}
	}

	// One flipped byte anywhere (AEAD nonce, ciphertext, tag) → 400, and no
	// session.
	for _, where := range []string{"first", "middle", "last"} {
		h.iss.SetIdentity(viaGoogle)
		lc, authURL, _ := h.startLogin("")
		b, _ := base64.RawURLEncoding.DecodeString(lc.Value)
		at := map[string]int{"first": 0, "middle": len(b) / 2, "last": len(b) - 1}[where]
		b[at] ^= 0x01
		tampered := &http.Cookie{Name: lc.Name, Value: base64.RawURLEncoding.EncodeToString(b)}
		rec := h.callback(h.authorize(authURL), tampered)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("tampered %s byte: callback = %d, want 400", where, rec.Code)
		}
		if cookieNamed(t, rec, "__Host-luma") != nil {
			t.Errorf("tampered %s byte: a session cookie was set", where)
		}
	}
	if n := len(h.rows()); n != 1 {
		t.Fatalf("%d sessions, want only the untampered login's", n)
	}

	// A cookie sealed under another signing key does not open.
	other := h.newBFF(bytes.Repeat([]byte{0x24}, 32), mustParseRegistry(t, registryYAML))
	mux := http.NewServeMux()
	other.Routes(mux)
	h.iss.SetIdentity(viaGoogle)
	lc, authURL, _ = h.startLogin("") // sealed by h.bff
	cb := h.authorize(authURL)
	req, _ := http.NewRequest(http.MethodGet, publicHTTPS+cb.Path+"?"+cb.RawQuery, nil)
	req.AddCookie(&http.Cookie{Name: lc.Name, Value: lc.Value})
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("a cookie from another key: callback = %d, want 400", rw.Code)
	}
}

func TestLoginCookieExpiresAfter10Minutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	h.iss.SetIdentity(viaGoogle)
	lc, authURL, _ := h.startLogin("")
	cb := h.authorize(authURL)
	h.clock.advance(10*time.Minute + time.Second)
	if rec := h.callback(cb, lc); rec.Code != http.StatusBadRequest {
		t.Fatalf("an 11-minute-old login cookie: callback = %d, want 400", rec.Code)
	}
	if len(h.rows()) != 0 {
		t.Fatal("a session was created from an expired login cookie")
	}
}
