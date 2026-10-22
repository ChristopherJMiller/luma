package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ChristopherJMiller/luma/core/internal/auth"
)

var ok200 = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

func guarded(method, path, origin, contentType string) int {
	req := httptest.NewRequest(method, publicHTTPS+path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	auth.UnsafeRequestGuard([]string{publicHTTPS})(ok200).ServeHTTP(rec, req)
	return rec.Code
}

func TestUnsafeMethodNeedsAllowedOrigin(t *testing.T) {
	t.Parallel()
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, origin := range []string{"", foreign, "null", "https://other.example.test", "http://luma.example.test", publicHTTPS + ":8443", publicHTTPS + "/"} {
			if got := guarded(m, "/v1/sessions", origin, "application/json"); got != http.StatusForbidden {
				t.Errorf("%s with Origin %q = %d, want 403", m, origin, got)
			}
		}
		if got := guarded(m, "/v1/sessions", publicHTTPS, "application/json"); got != http.StatusOK {
			t.Errorf("%s from the public origin = %d, want 200", m, got)
		}
	}
	// Two Origin headers are not one allowed origin.
	req := httptest.NewRequest(http.MethodPost, publicHTTPS+"/v1/sessions", nil)
	req.Header.Add("Origin", publicHTTPS)
	req.Header.Add("Origin", foreign)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	auth.UnsafeRequestGuard([]string{publicHTTPS})(ok200).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("two Origin headers = %d, want 403", rec.Code)
	}
	// Safe methods pass without an Origin (same-origin GETs send none).
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if got := guarded(m, "/v1/sessions", "", ""); got != http.StatusOK {
			t.Errorf("%s without an Origin = %d, want 200", m, got)
		}
	}
	// An empty allowlist refuses every unsafe request.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, publicHTTPS+"/v1/sessions", nil)
	req.Header.Set("Origin", publicHTTPS)
	req.Header.Set("Content-Type", "application/json")
	auth.UnsafeRequestGuard(nil)(ok200).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("empty allowlist = %d, want 403", rec.Code)
	}
}

func TestJSONContentTypeRequired(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method, path, contentType string
		want                      int
	}{
		{http.MethodPost, "/v1/sessions", "text/plain", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/v1/sessions", "", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/v1/sessions", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/v1/sessions", "multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/v1/sessions", "application/jsonx", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/v1/sessions", "application/json", http.StatusOK},
		{http.MethodPost, "/v1/sessions", "Application/JSON; charset=utf-8", http.StatusOK},
		{http.MethodDelete, "/v1/sessions/ses_x", "", http.StatusUnsupportedMediaType},
		{http.MethodDelete, "/v1/sessions/ses_x", "application/json", http.StatusOK},
		{http.MethodPost, "/v1/attachments", "multipart/form-data; boundary=x", http.StatusOK},
		{http.MethodPost, "/v1/attachments", "application/json", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/v1/attachments", "text/plain", http.StatusUnsupportedMediaType},
		{http.MethodDelete, "/v1/attachments/att_x", "multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/v1/attachments/", "multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
	}
	for _, c := range cases {
		if got := guarded(c.method, c.path, publicHTTPS, c.contentType); got != c.want {
			t.Errorf("%s %s Content-Type %q = %d, want %d", c.method, c.path, c.contentType, got, c.want)
		}
	}
}

// S17: the guard wraps RequirePerson, so a cross-origin POST is refused
// before authentication is even looked at.
func TestGuardBeforeAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, publicHTTPS)
	handler := auth.UnsafeRequestGuard([]string{publicHTTPS})(h.bff.RequirePerson(ok200))
	post := func(origin string) int {
		req := httptest.NewRequest(http.MethodPost, publicHTTPS+"/v1/sessions", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := post(foreign); got != http.StatusForbidden {
		t.Errorf("foreign-Origin POST without a cookie = %d, want 403", got)
	}
	if got := post(publicHTTPS); got != http.StatusUnauthorized {
		t.Errorf("same-origin POST without a cookie = %d, want 401", got)
	}
}
