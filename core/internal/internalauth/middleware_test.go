package internalauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	saConductor = "system:serviceaccount:luma:luma-conductor"
	saEvals     = "system:serviceaccount:luma:luma-evals"
)

func principals(t *testing.T) map[string]Principal {
	t.Helper()
	m, err := ParsePrincipals("conductor=" + saConductor + ",evals=" + saEvals +
		",reconcile=system:serviceaccount:luma:luma-usage-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func serve(t *testing.T, r Reviewer, allow ...Principal) (http.Handler, *int) {
	t.Helper()
	hits := 0
	h := Middleware(r, testAudience, principals(t), allow...)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits++
		p, id, ok := PrincipalFrom(req.Context())
		if !ok {
			t.Error("PrincipalFrom missing in handler")
		}
		fmt.Fprintf(w, "%s|%s|%s", p, id.Username, id.PodName)
	}))
	return h, &hits
}

func do(h http.Handler, authz string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/x", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func kubeRev(t *testing.T) (*fakeKube, Reviewer) {
	f := newFakeKube(t)
	r, _ := f.reviewer(t)
	return f, r
}

func TestValidConductorToken(t *testing.T) {
	f, r := kubeRev(t)
	f.allow("good", saConductor, testAudience)
	h, hits := serve(t, r, PrincipalConductor)
	rec := do(h, "Bearer good")
	if rec.Code != 200 || rec.Body.String() != "conductor|"+saConductor+"|pod-luma-conductor" || *hits != 1 {
		t.Fatalf("%d %q hits=%d", rec.Code, rec.Body.String(), *hits)
	}
}

func TestMissingOrMalformedBearer401(t *testing.T) {
	f, r := kubeRev(t)
	h, hits := serve(t, r, PrincipalConductor)
	for _, a := range []string{"", "Bearer", "Bearer ", "Basic abc", "good"} {
		if rec := do(h, a); rec.Code != 401 {
			t.Errorf("%q -> %d", a, rec.Code)
		}
	}
	if *hits != 0 || f.callCount() != 0 {
		t.Fatalf("handler hits %d, reviews %d", *hits, f.callCount())
	}
}

func TestUnauthenticatedToken401(t *testing.T) {
	_, r := kubeRev(t)
	h, hits := serve(t, r, PrincipalConductor)
	if rec := do(h, "Bearer unknown"); rec.Code != 401 || *hits != 0 {
		t.Fatalf("%d hits=%d", rec.Code, *hits)
	}
}

func TestWrongAudience401(t *testing.T) {
	f, r := kubeRev(t)
	// An API-audience token (e.g. Core's own) must not open the internal port.
	f.allow("apitok", saConductor, "https://kubernetes.default.svc")
	f.allow("noaud", saConductor)
	h, hits := serve(t, r, PrincipalConductor)
	for _, tok := range []string{"apitok", "noaud"} {
		if rec := do(h, "Bearer "+tok); rec.Code != 401 {
			t.Errorf("%s -> %d", tok, rec.Code)
		}
	}
	if *hits != 0 {
		t.Fatal("handler ran")
	}
}

func TestUnknownServiceAccount403(t *testing.T) {
	f, r := kubeRev(t)
	f.allow("other", "system:serviceaccount:luma:luma-gateway", testAudience)
	f.allow("otherns", "system:serviceaccount:evil:luma-conductor", testAudience)
	f.allow("user", "alice", testAudience)
	h, hits := serve(t, r, PrincipalConductor)
	for _, tok := range []string{"other", "otherns", "user"} {
		if rec := do(h, "Bearer "+tok); rec.Code != 403 {
			t.Errorf("%s -> %d", tok, rec.Code)
		}
	}
	if *hits != 0 {
		t.Fatal("handler ran")
	}
}

func TestPrincipalNotAllowedForRoute403(t *testing.T) {
	f, r := kubeRev(t)
	f.allow("cond", saConductor, testAudience)
	f.allow("ev", saEvals, testAudience)
	h, hits := serve(t, r, PrincipalEvals)
	if rec := do(h, "Bearer cond"); rec.Code != 403 {
		t.Fatalf("conductor on evals route -> %d", rec.Code)
	}
	if rec := do(h, "Bearer ev"); rec.Code != 200 {
		t.Fatalf("evals on evals route -> %d", rec.Code)
	}
	// A cached conductor must still be refused on the evals route.
	if rec := do(h, "Bearer cond"); rec.Code != 403 || *hits != 1 {
		t.Fatalf("cached conductor -> %d hits=%d", rec.Code, *hits)
	}
}

func TestMultiplePrincipalsAllowed(t *testing.T) {
	f, r := kubeRev(t)
	f.allow("cond", saConductor, testAudience)
	h, _ := serve(t, r, PrincipalEvals, PrincipalConductor)
	if rec := do(h, "Bearer cond"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestAPIServerDown503FailsClosed(t *testing.T) {
	f, r := kubeRev(t)
	f.allow("good", saConductor, testAudience)
	h, hits := serve(t, r, PrincipalConductor)
	f.srv.Close()
	if rec := do(h, "Bearer good"); rec.Code != 503 || *hits != 0 {
		t.Fatalf("%d hits=%d", rec.Code, *hits)
	}
}

func TestPositiveCacheHitsApiserverOnce(t *testing.T) {
	f, r := kubeRev(t)
	f.allow("good", saConductor, testAudience)
	h, _ := serve(t, r, PrincipalConductor)
	for i := 0; i < 5; i++ {
		if rec := do(h, "Bearer good"); rec.Code != 200 {
			t.Fatal(rec.Code)
		}
	}
	if f.callCount() != 1 {
		t.Fatalf("reviews = %d, want 1", f.callCount())
	}
}

func TestNegativeResultsNotCached(t *testing.T) {
	f, r := kubeRev(t)
	h, _ := serve(t, r, PrincipalConductor)
	if rec := do(h, "Bearer late"); rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	f.allow("late", saConductor, testAudience)
	if rec := do(h, "Bearer late"); rec.Code != 200 {
		t.Fatalf("after becoming valid -> %d", rec.Code)
	}
}

// jwtWithExp makes a token whose unverified payload carries exp.
func jwtWithExp(exp time.Time) string {
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return "e30." + p + ".c2ln"
}

func TestCacheBoundedBy60sAndTokenExpiry(t *testing.T) {
	base := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clock := base
	now := func() time.Time { return clock }

	f, r := kubeRev(t)
	long := jwtWithExp(base.Add(10 * time.Minute))
	short := jwtWithExp(base.Add(20 * time.Second))
	f.allow(long, saConductor, testAudience)
	f.allow(short, saConductor, testAudience)
	h := middleware(r, testAudience, principals(t), []Principal{PrincipalConductor}, now)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	// Long-lived token: cached up to 60s, not beyond.
	do(h, "Bearer "+long)
	clock = base.Add(59 * time.Second)
	do(h, "Bearer "+long)
	if f.callCount() != 1 {
		t.Fatalf("within 60s: %d reviews", f.callCount())
	}
	clock = base.Add(61 * time.Second)
	do(h, "Bearer "+long)
	if f.callCount() != 2 {
		t.Fatalf("after 60s: %d reviews", f.callCount())
	}

	// Short-lived token: cache ends at its own expiry (20s), well before 60s.
	clock = base
	do(h, "Bearer "+short)
	n := f.callCount()
	clock = base.Add(19 * time.Second)
	do(h, "Bearer "+short)
	if f.callCount() != n {
		t.Fatal("re-reviewed before token expiry")
	}
	clock = base.Add(21 * time.Second)
	do(h, "Bearer "+short)
	if f.callCount() != n+1 {
		t.Fatalf("not re-reviewed after token expiry: %d vs %d", f.callCount(), n)
	}

	// Already-expired token is never cached.
	dead := jwtWithExp(base.Add(-time.Minute))
	f.allow(dead, saConductor, testAudience)
	clock = base
	n = f.callCount()
	do(h, "Bearer "+dead)
	do(h, "Bearer "+dead)
	if f.callCount() != n+2 {
		t.Fatalf("expired token cached: %d", f.callCount()-n)
	}
}

func TestTokenNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	f, r := kubeRev(t)
	f.allow("TOKEN-OK-1234", saConductor, testAudience)
	f.allow("TOKEN-WRONGAUD-1234", saConductor, "other")
	f.allow("TOKEN-OTHERSA-1234", "system:serviceaccount:x:y", testAudience)
	h, _ := serve(t, r, PrincipalConductor)
	for _, tok := range []string{"TOKEN-OK-1234", "TOKEN-WRONGAUD-1234", "TOKEN-OTHERSA-1234", "TOKEN-BAD-1234"} {
		do(h, "Bearer "+tok)
	}
	f.srv.Close()
	do(h, "Bearer TOKEN-DOWN-1234")

	out := buf.String()
	if !strings.Contains(out, "internal auth denied") {
		t.Fatalf("expected denial logs, got %q", out)
	}
	if strings.Contains(out, "TOKEN-") {
		t.Fatalf("token leaked into logs:\n%s", out)
	}
}

func TestPrincipalFromAbsent(t *testing.T) {
	if _, _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("unexpected principal")
	}
}
