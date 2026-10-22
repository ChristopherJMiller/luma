package reconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// These tests replace the package-level sleep, so none is parallel.

type fakeCore struct {
	mu       sync.Mutex
	statuses []int // one per request, the last repeats
	retry    string
	tokens   []string
	bodies   []protocol.UsageReconciliation
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.tokens)
	f.tokens = append(f.tokens, r.Header.Get("Authorization"))
	var b protocol.UsageReconciliation
	_ = json.NewDecoder(r.Body).Decode(&b)
	f.bodies = append(f.bodies, b)
	st := f.statuses[min(n, len(f.statuses)-1)]
	if st == 503 && f.retry != "" {
		w.Header().Set("Retry-After", f.retry)
	}
	if st != 200 {
		http.Error(w, http.StatusText(st), st)
		return
	}
	_, _ = w.Write([]byte(`{"workspaces":[{"name":"luma-ci","api_usd":2.5,"ledger_usd":2.5,"drift_ratio":0}]}`))
}

func runWith(t *testing.T, f *fakeCore) (error, []time.Duration, *os.File) {
	t.Helper()
	var waits []time.Duration
	old := sleep
	sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	t.Cleanup(func() { sleep = old })
	core := httptest.NewServer(f)
	t.Cleanup(core.Close)
	c, _ := newAdmin(t)
	tokFile := keyFile(t, "tok-1")
	err := Run(bg, c, core.Client(), core.URL, tokFile, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), []string{"luma-interactive", "luma-ci"})
	return err, waits, nil
}

func TestRunPostsCostsToCore(t *testing.T) {
	f := &fakeCore{statuses: []int{200}}
	err, waits, _ := runWith(t, f)
	if err != nil || len(waits) != 0 || len(f.bodies) != 1 {
		t.Fatalf("err=%v waits=%v posts=%d", err, waits, len(f.bodies))
	}
	b := f.bodies[0]
	if b.Day != "2026-10-14" || len(b.Workspaces) != 2 ||
		b.Workspaces[0].Name != "luma-interactive" || !near(b.Workspaces[0].APIUSD, 13.8475) ||
		b.Workspaces[1].Name != "luma-ci" || !near(b.Workspaces[1].APIUSD, 2.5) {
		t.Errorf("body = %+v", b)
	}
	if f.tokens[0] != "Bearer tok-1" {
		t.Errorf("authorization = %q", f.tokens[0])
	}
}

func TestRunRetriesCore503(t *testing.T) {
	t.Run("two 503s with Retry-After then success", func(t *testing.T) {
		f := &fakeCore{statuses: []int{503, 503, 200}, retry: "1"}
		err, waits, _ := runWith(t, f)
		if err != nil || len(f.bodies) != 3 {
			t.Fatalf("err=%v posts=%d", err, len(f.bodies))
		}
		// Backoff 1 s then 2 s; Retry-After: 1 never shortens it.
		if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
			t.Errorf("waits = %v, want [1s 2s]", waits)
		}
	})
	t.Run("Retry-After longer than the backoff wins", func(t *testing.T) {
		f := &fakeCore{statuses: []int{503, 200}, retry: "30"}
		err, waits, _ := runWith(t, f)
		if err != nil || len(waits) != 1 || waits[0] != 30*time.Second {
			t.Fatalf("err=%v waits=%v", err, waits)
		}
	})
	t.Run("six 503s fail after five retries", func(t *testing.T) {
		f := &fakeCore{statuses: []int{503}}
		err, waits, _ := runWith(t, f)
		if err == nil || len(f.bodies) != 6 {
			t.Fatalf("err=%v posts=%d, want an error after 6", err, len(f.bodies))
		}
		want := []time.Duration{1, 2, 4, 8, 16}
		for i, w := range want {
			if i >= len(waits) || waits[i] != w*time.Second {
				t.Fatalf("waits = %v, want 1s 2s 4s 8s 16s", waits)
			}
		}
	})
	t.Run("a 403 fails at once", func(t *testing.T) {
		f := &fakeCore{statuses: []int{403}}
		err, waits, _ := runWith(t, f)
		if err == nil || len(f.bodies) != 1 || len(waits) != 0 {
			t.Fatalf("err=%v posts=%d waits=%v", err, len(f.bodies), waits)
		}
	})
}

func TestRunRereadsTokenEachAttempt(t *testing.T) {
	f := &fakeCore{statuses: []int{503, 200}}
	old := sleep
	t.Cleanup(func() { sleep = old })
	core := httptest.NewServer(f)
	defer core.Close()
	c, _ := newAdmin(t)
	tokFile := keyFile(t, "tok-1")
	sleep = func(context.Context, time.Duration) error { return os.WriteFile(tokFile, []byte("tok-2\n"), 0o600) }
	if err := Run(bg, c, core.Client(), core.URL, tokFile, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), []string{"luma-ci"}); err != nil {
		t.Fatal(err)
	}
	if f.tokens[0] != "Bearer tok-1" || f.tokens[1] != "Bearer tok-2" {
		t.Errorf("tokens = %q", f.tokens)
	}
}
