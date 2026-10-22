package internalauth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestOversizedAndMalformedTokens401WithoutReview(t *testing.T) {
	f, r := kubeRev(t)
	h, hits := serve(t, r, PrincipalConductor)
	for name, tok := range map[string]string{
		"oversized":   strings.Repeat("a", 9<<10) + ".b.c",
		"two parts":   "aaa.bbb",
		"four parts":  "a.b.c.d",
		"empty part":  "a..c",
		"bad chars":   "a b.c.d",
		"not base64u": "a+/.b.c",
		"opaque":      "ghp_abcdef",
	} {
		rec := do(h, "Bearer "+tok)
		if rec.Code != 401 {
			t.Errorf("%s -> %d", name, rec.Code)
		}
	}
	if f.callCount() != 0 || *hits != 0 {
		t.Fatalf("reviews=%d hits=%d", f.callCount(), *hits)
	}
}

type blockingReviewer struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingReviewer) Review(ctx context.Context, _ string) (Identity, error) {
	b.started <- struct{}{}
	<-b.release
	return Identity{}, ErrUnauthenticated
}

func TestSaturatedReviewSemaphore503(t *testing.T) {
	b := &blockingReviewer{started: make(chan struct{}, 64), release: make(chan struct{})}
	h, _ := serve(t, b, PrincipalConductor)
	var wg sync.WaitGroup
	for i := range maxConcurrentReviews {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(h, "Bearer tok"+string(rune('a'+i))+".b.c")
		}()
	}
	for range maxConcurrentReviews {
		<-b.started
	}
	rec := do(h, "Bearer other.b.c")
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("code=%d headers=%v", rec.Code, rec.Header())
	}
	close(b.release)
	wg.Wait()
	// Slots are released: a later request reaches the reviewer again.
	go func() { <-b.started }()
	if rec := do(h, "Bearer later.b.c"); rec.Code != 401 {
		t.Fatalf("after release -> %d", rec.Code)
	}
}

type panickingReviewer struct{}

func (panickingReviewer) Review(context.Context, string) (Identity, error) {
	panic("reviewer bug")
}

// A panicking Reviewer (net/http recovers it per request) must give its
// semaphore slot back. With more panics than slots, a leak would make a later
// request a 503 before it reaches the reviewer, instead of panicking again.
func TestPanickingReviewerReleasesItsSlot(t *testing.T) {
	h, _ := serve(t, panickingReviewer{}, PrincipalConductor)
	for i := range maxConcurrentReviews + 2 {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("request %d did not reach the reviewer (slot leaked)", i)
				}
			}()
			do(h, "Bearer p"+string(rune('a'+i))+".b.c")
		}()
	}
}

func TestFailClosedLogsReasonNotToken(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	f, r := kubeRev(t)
	f.failWith = 403
	h, _ := serve(t, r, PrincipalConductor)
	rec := do(h, "Bearer TOKEN-SECRET-9.a.b")
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("code=%d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "apiserver status 403") {
		t.Fatalf("reason missing from log: %s", out)
	}
	if strings.Contains(out, "TOKEN-SECRET") {
		t.Fatalf("token leaked: %s", out)
	}
}

func TestBodiesAreStatusTextOnly(t *testing.T) {
	_, r := kubeRev(t)
	h, _ := serve(t, r, PrincipalConductor)
	rec := do(h, "")
	if rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != "Unauthorized" || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("%d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestLowercaseBearerAccepted(t *testing.T) {
	f, r := kubeRev(t)
	f.allow("good.a.b", saConductor, testAudience)
	h, _ := serve(t, r, PrincipalConductor)
	if rec := do(h, "bearer good.a.b"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestMultipleAuthorizationHeaders401(t *testing.T) {
	f, r := kubeRev(t)
	f.allow("good.a.b", saConductor, testAudience)
	h, _ := serve(t, r, PrincipalConductor)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Add("Authorization", "Bearer good.a.b")
	req.Header.Add("Authorization", "Bearer good.a.b")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 || f.callCount() != 0 {
		t.Fatalf("%d reviews=%d", rec.Code, f.callCount())
	}
}

func TestWhitespaceOnlyOwnToken503(t *testing.T) {
	f := newFakeKube(t)
	r, own := f.reviewer(t)
	if err := os.WriteFile(own, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, _ := serve(t, r, PrincipalConductor)
	if rec := do(h, "Bearer good.a.b"); rec.Code != 503 || f.callCount() != 0 {
		t.Fatalf("%d reviews=%d", rec.Code, f.callCount())
	}
}

func TestReviewerConfigErrors(t *testing.T) {
	f := newFakeKube(t)
	good, own := f.reviewer(t)
	if good.Err() != nil {
		t.Fatalf("good reviewer Err = %v", good.Err())
	}
	dir := t.TempDir()
	junk := dir + "/junk.crt"
	_ = os.WriteFile(junk, []byte("not a pem"), 0o600)

	cases := map[string]*KubeReviewer{
		"missing CA": NewKubeReviewer(f.srv.URL, dir+"/nope", own, testAudience),
		"junk CA":    NewKubeReviewer(f.srv.URL, junk, own, testAudience),
		"http url":   NewKubeReviewer(strings.Replace(f.srv.URL, "https://", "http://", 1), junk, own, testAudience),
	}
	for name, r := range cases {
		if r.Err() == nil {
			t.Errorf("%s: Err() = nil", name)
			continue
		}
		_, err := r.Review(context.Background(), "x.y.z")
		if err == nil || !strings.Contains(err.Error(), r.Err().Error()) {
			t.Errorf("%s: Review err = %v, want reason %q", name, err, r.Err())
		}
	}
	if f.callCount() != 0 {
		t.Fatal("misconfigured reviewer called the apiserver")
	}
}

func TestReviewDoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer target.Close()
	f := newFakeKube(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	r, _ := f.reviewer(t)
	if _, err := r.Review(context.Background(), "x.y.z"); err == nil {
		t.Fatal("expected failure on 307")
	}
	if leaked {
		t.Fatal("redirect was followed")
	}
}

func TestParsePrincipalsEmptySegments(t *testing.T) {
	for _, bad := range []string{
		"conductor=system:serviceaccount::sa",
		"conductor=system:serviceaccount:ns:",
		"conductor=system:user:ns:sa",
	} {
		if _, err := ParsePrincipals(bad); err == nil {
			t.Errorf("ParsePrincipals(%q) accepted", bad)
		}
	}
}
