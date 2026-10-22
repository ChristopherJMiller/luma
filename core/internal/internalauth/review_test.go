package internalauth

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testAudience = "luma-core-internal"

// fakeKube is a fake apiserver TokenReview endpoint. It answers by the token
// in the request body.
type fakeKube struct {
	srv *httptest.Server

	mu       sync.Mutex
	reviews  map[string]map[string]any // token -> status object
	calls    int
	ownSeen  []string
	lastReq  map[string]any
	failWith int
}

func newFakeKube(t *testing.T) *fakeKube {
	t.Helper()
	f := &fakeKube{reviews: map[string]map[string]any{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			http.NotFound(w, r)
			return
		}
		f.calls++
		f.ownSeen = append(f.ownSeen, r.Header.Get("Authorization"))
		if f.failWith != 0 {
			http.Error(w, "boom", f.failWith)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastReq = req
		tok, _ := req["spec"].(map[string]any)["token"].(string)
		status, ok := f.reviews[tok]
		if !ok {
			status = map[string]any{"authenticated": false, "error": "invalid"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview", "status": status,
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKube) allow(tok, user string, auds ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews[tok] = map[string]any{
		"authenticated": true,
		"user": map[string]any{"username": user, "extra": map[string]any{
			"authentication.kubernetes.io/pod-name": []string{"pod-" + user[strings.LastIndex(user, ":")+1:]},
		}},
		"audiences": auds,
	}
}

func (f *fakeKube) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// reviewer wires a KubeReviewer at the fake, with a CA file holding its cert
// and an own-token file the test can rotate.
func (f *fakeKube) reviewer(t *testing.T) (*KubeReviewer, string) {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "token")
	if err := os.WriteFile(own, []byte("core-own-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewKubeReviewer(f.srv.URL, ca, own, testAudience), own
}

func TestReviewSendsAudienceAndOwnToken(t *testing.T) {
	f := newFakeKube(t)
	f.allow("tok", "system:serviceaccount:luma:luma-conductor", testAudience)
	r, _ := f.reviewer(t)
	id, err := r.Review(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if id.Username != "system:serviceaccount:luma:luma-conductor" || id.PodName != "pod-luma-conductor" {
		t.Fatalf("identity %+v", id)
	}
	spec := f.lastReq["spec"].(map[string]any)
	auds := spec["audiences"].([]any)
	if len(auds) != 1 || auds[0] != testAudience {
		t.Fatalf("spec.audiences = %v", auds)
	}
	if f.lastReq["kind"] != "TokenReview" || f.lastReq["apiVersion"] != "authentication.k8s.io/v1" {
		t.Fatalf("request %v", f.lastReq)
	}
	if f.ownSeen[0] != "Bearer core-own-1" {
		t.Fatalf("own auth header %q", f.ownSeen[0])
	}
}

func TestReviewerReReadsOwnToken(t *testing.T) {
	f := newFakeKube(t)
	f.allow("tok", "system:serviceaccount:luma:luma-conductor", testAudience)
	r, own := f.reviewer(t)
	if _, err := r.Review(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("core-own-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Review(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	if got := f.ownSeen; len(got) != 2 || got[0] != "Bearer core-own-1" || got[1] != "Bearer core-own-2" {
		t.Fatalf("own tokens seen: %v", got)
	}
}

func TestReviewUnauthenticated(t *testing.T) {
	f := newFakeKube(t)
	r, _ := f.reviewer(t)
	if _, err := r.Review(context.Background(), "nope"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}

func TestReviewFailsClosed(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		f := newFakeKube(t)
		r, _ := f.reviewer(t)
		f.srv.Close()
		_, err := r.Review(context.Background(), "tok")
		if !errors.Is(err, ErrReviewUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("apiserver 500", func(t *testing.T) {
		f := newFakeKube(t)
		f.failWith = 500
		r, _ := f.reviewer(t)
		if _, err := r.Review(context.Background(), "tok"); !errors.Is(err, ErrReviewUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("own token missing", func(t *testing.T) {
		f := newFakeKube(t)
		r, own := f.reviewer(t)
		_ = os.Remove(own)
		if _, err := r.Review(context.Background(), "tok"); !errors.Is(err, ErrReviewUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("untrusted apiserver cert", func(t *testing.T) {
		f := newFakeKube(t)
		f.allow("tok", "system:serviceaccount:luma:luma-conductor", testAudience)
		dir := t.TempDir()
		emptyCA := filepath.Join(dir, "ca.crt")
		_ = os.WriteFile(emptyCA, nil, 0o600)
		own := filepath.Join(dir, "token")
		_ = os.WriteFile(own, []byte("x"), 0o600)
		r := NewKubeReviewer(f.srv.URL, emptyCA, own, testAudience)
		if _, err := r.Review(context.Background(), "tok"); !errors.Is(err, ErrReviewUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestReviewErrorsNeverContainToken(t *testing.T) {
	f := newFakeKube(t)
	f.failWith = 500
	r, _ := f.reviewer(t)
	const secret = "SECRET-BEARER-VALUE"
	_, err := r.Review(context.Background(), secret)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIServerFromEnv(t *testing.T) {
	got, err := APIServerFromEnv("10.96.0.1", "443")
	if err != nil || got != "https://10.96.0.1:443" {
		t.Fatalf("%q %v", got, err)
	}
	got, _ = APIServerFromEnv("fd00::1", "443")
	if got != "https://[fd00::1]:443" {
		t.Fatalf("%q", got)
	}
	if _, err := APIServerFromEnv("", "443"); err == nil {
		t.Fatal("want error for empty host")
	}
}

func TestParsePrincipals(t *testing.T) {
	m, err := ParsePrincipals("conductor=system:serviceaccount:luma:luma-conductor, evals=system:serviceaccount:luma:luma-evals,reconcile=system:serviceaccount:luma:luma-usage-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 || m["system:serviceaccount:luma:luma-evals"] != PrincipalEvals ||
		m["system:serviceaccount:luma:luma-usage-reconcile"] != PrincipalReconcile ||
		m["system:serviceaccount:luma:luma-conductor"] != PrincipalConductor {
		t.Fatalf("%v", m)
	}
	for _, bad := range []string{
		"", "conductor", "conductor=", "root=system:serviceaccount:a:b",
		"conductor=admin", "conductor=system:serviceaccount:a",
		"conductor=system:serviceaccount:a:b,evals=system:serviceaccount:a:b",
	} {
		if _, err := ParsePrincipals(bad); err == nil {
			t.Errorf("ParsePrincipals(%q) = nil error", bad)
		}
	}
}
