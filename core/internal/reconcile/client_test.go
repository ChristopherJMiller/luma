package reconcile

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

const fakeKey = "sk-ant-admin01-FAKE-TEST-KEY-not-real"

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keyFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeAdmin serves the hand-written fixtures and records the requests.
type fakeAdmin struct {
	t    *testing.T
	mu   sync.Mutex
	reqs []*http.Request
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Clone(bg))
	f.mu.Unlock()
	if r.Header.Get("x-api-key") != fakeKey || r.Header.Get("anthropic-version") != "2023-06-01" {
		http.Error(w, "bad auth; your key was "+r.Header.Get("x-api-key"), http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/organizations/workspaces":
		_, _ = w.Write(fixture(f.t, "workspaces.json"))
	case "/v1/organizations/cost_report":
		if r.URL.Query().Get("page") == "page_abc123" {
			_, _ = w.Write(fixture(f.t, "cost_report_page2.json"))
			return
		}
		_, _ = w.Write(fixture(f.t, "cost_report_page1.json"))
	default:
		http.NotFound(w, r)
	}
}

func newAdmin(t *testing.T) (*AdminClient, *fakeAdmin) {
	t.Helper()
	f := &fakeAdmin{t: t}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &AdminClient{BaseURL: srv.URL, KeyFile: keyFile(t, fakeKey+"\n"), HTTP: srv.Client()}, f
}

func TestWorkspaceNamesResolved(t *testing.T) {
	c, _ := newAdmin(t)
	ids, err := c.WorkspaceIDs(bg, []string{"luma-interactive", "luma-ci"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids["luma-interactive"] != "wrkspc_01LumaInteractive" || ids["luma-ci"] != "wrkspc_01LumaCi" {
		t.Errorf("ids = %v", ids)
	}
}

func TestMissingWorkspaceIsError(t *testing.T) {
	c, _ := newAdmin(t)
	_, err := c.WorkspaceIDs(bg, []string{"luma-interactive", "luma-ghost"})
	if err == nil || !strings.Contains(err.Error(), "luma-ghost") {
		t.Fatalf("err = %v, want one naming luma-ghost", err)
	}
}

func TestWorkspacesPaged(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Query().Get("after_id"))
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"wrkspc_1","name":"luma-ci"}],"has_more":true,"last_id":"wrkspc_1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"wrkspc_2","name":"luma-interactive"}],"has_more":false,"last_id":"wrkspc_2"}`))
	}))
	defer srv.Close()
	c := &AdminClient{BaseURL: srv.URL, KeyFile: keyFile(t, fakeKey), HTTP: srv.Client()}
	ids, err := c.WorkspaceIDs(bg, []string{"luma-interactive", "luma-ci"})
	if err != nil || ids["luma-ci"] != "wrkspc_1" || ids["luma-interactive"] != "wrkspc_2" {
		t.Fatalf("ids = %v, err = %v", ids, err)
	}
	if len(calls) != 2 || calls[1] != "wrkspc_1" {
		t.Errorf("after_id sequence = %q", calls)
	}
}

func TestCostReportPagedAndSummed(t *testing.T) {
	c, f := newAdmin(t)
	ids := map[string]string{"luma-interactive": "wrkspc_01LumaInteractive", "luma-ci": "wrkspc_01LumaCi"}
	got, err := c.DailyCost(bg, time.Date(2026, 10, 14, 17, 30, 0, 0, time.UTC), ids)
	if err != nil {
		t.Fatal(err)
	}
	// 1234.5 + 100 + 50.25 cents (interactive, across both pages); 250 cents (ci).
	if !near(got["luma-interactive"], 13.8475) || !near(got["luma-ci"], 2.5) || len(got) != 2 {
		t.Errorf("costs = %v, want interactive 13.8475, ci 2.5", got)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("%d requests, want 2 pages", len(f.reqs))
	}
	q := f.reqs[0].URL.Query()
	if q.Get("starting_at") != "2026-10-14T00:00:00Z" || q.Get("ending_at") != "2026-10-15T00:00:00Z" ||
		q.Get("bucket_width") != "1d" || q.Get("group_by[]") != "workspace_id" || q.Get("page") != "" {
		t.Errorf("page 1 query = %v", q)
	}
	if f.reqs[1].URL.Query().Get("page") != "page_abc123" {
		t.Errorf("page 2 query = %v", f.reqs[1].URL.Query())
	}
}

// The fixture states amounts in cents as decimal strings (the docs: "lowest
// units (cents)"); a ci-only report of "250" is $2.50, not $250.
func TestCostUnitsConvertedToUSD(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"results":[{"currency":"USD","amount":"250","workspace_id":"wrkspc_c"},
			{"currency":"USD","amount":"0.5","workspace_id":"wrkspc_c"}]}],"has_more":false}`))
	}))
	defer srv.Close()
	c := &AdminClient{BaseURL: srv.URL, KeyFile: keyFile(t, fakeKey), HTTP: srv.Client()}
	got, err := c.DailyCost(bg, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), map[string]string{"luma-ci": "wrkspc_c", "luma-interactive": "wrkspc_i"})
	if err != nil {
		t.Fatal(err)
	}
	if !near(got["luma-ci"], 2.505) || got["luma-interactive"] != 0 {
		t.Errorf("costs = %v, want ci 2.505 and interactive 0", got)
	}
}

type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func TestAdminKeyReadFromFileAndNeverLogged(t *testing.T) {
	sink := &logSink{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	// Right key from the file (trailing newline trimmed): works.
	c, _ := newAdmin(t)
	if _, err := c.WorkspaceIDs(bg, []string{"luma-ci"}); err != nil {
		t.Fatal(err)
	}
	// The key is read per run: rewriting the file takes effect.
	if err := os.WriteFile(c.KeyFile, []byte("sk-ant-admin01-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.WorkspaceIDs(bg, []string{"luma-ci"})
	if err == nil {
		t.Fatal("rotated key was not re-read")
	}
	// The fake echoes the offending key in its 401 body; no error carries it.
	if strings.Contains(err.Error(), "rotated") || strings.Contains(err.Error(), "sk-ant") {
		t.Errorf("error leaks the key: %v", err)
	}
	// A bad key file (path in error is fine, content is not).
	bad := &AdminClient{BaseURL: c.BaseURL, KeyFile: keyFile(t, "   "), HTTP: c.HTTP}
	if _, err := bad.WorkspaceIDs(bg, []string{"luma-ci"}); err == nil {
		t.Error("an empty key file must fail")
	}
	if s := sink.buf.String(); strings.Contains(s, "sk-ant") || strings.Contains(s, "rotated") {
		t.Errorf("logs leak the key: %s", s)
	}
}
