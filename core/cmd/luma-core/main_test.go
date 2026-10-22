package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/luma/core/internal/maintenance"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

var bg = context.Background()

type memSettings struct {
	mu   sync.Mutex
	vals map[string]json.RawMessage
}

func (s *memSettings) Setting(_ context.Context, key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}

func (s *memSettings) SetSetting(_ context.Context, key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.vals[key] = b
	return nil
}

// fleet has one running turn that ends when interrupted, and two open hosts
// until shutdown.
type fleet struct {
	mu      sync.Mutex
	running []string
	hosts   int
	calls   []string
}

func (f *fleet) RunningTurns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.running)
}

func (f *fleet) OpenHosts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts
}

func (f *fleet) Barrier(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "barrier")
	return nil
}

func (f *fleet) Interrupt(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "interrupt:"+id)
	f.running = slices.DeleteFunc(f.running, func(s string) bool { return s == id })
	return nil
}

func (f *fleet) Shutdown(_ context.Context, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "shutdown:"+reason)
	f.hosts = 0
	return nil
}

// admin serves the real admin handler on a loopback port and returns its env.
func admin(t *testing.T, f *fleet) func(string) string {
	t.Helper()
	mt, err := maintenance.New(bg, &memSettings{vals: map[string]json.RawMessage{}}, f, metrics.NewCore(prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	l, err := maintenance.ListenAdmin("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mt.AdminHandler()}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := l.Addr().String()
	return func(k string) string {
		if k == "LUMA_ADMIN_LISTEN" {
			return addr
		}
		return ""
	}
}

func runCmd(env func(string) string, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(bg, args, env, &out, &errOut)
	return code, out.String(), errOut.String()
}

func noEnv(string) string { return "" }

func TestUnknownSubcommandPrintsUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"frobnicate"}, {"maintenance"}, {"maintenance", "sideways"}} {
		code, out, errOut := runCmd(noEnv, args...)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, "usage: luma-core ") {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want 2 and usage on stderr", args, code, out, errOut)
		}
	}
}

func TestServeIsAStub(t *testing.T) {
	code, out, errOut := runCmd(noEnv, "serve")
	if code != 1 || out != "" || errOut != "serve: not wired yet (Task 15)\n" {
		t.Fatalf("serve: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestMaintenanceCommands(t *testing.T) {
	f := &fleet{running: []string{"ses_A"}, hosts: 2}
	env := admin(t, f)

	if code, out, errOut := runCmd(env, "maintenance", "status"); code != 0 || out != "enabled=false running_turns=1 open_hosts=2\n" {
		t.Fatalf("status: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut := runCmd(env, "maintenance", "on", "--reason", "rotate tokens", "--drain-timeout=20ms")
	if code != 0 || out != "enabled=true running_turns=0 open_hosts=0\n" {
		t.Fatalf("on: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	f.mu.Lock()
	calls := slices.Clone(f.calls)
	f.mu.Unlock()
	if !slices.Equal(calls, []string{"barrier", "interrupt:ses_A", "shutdown:maintenance"}) {
		t.Fatalf("calls = %v", calls)
	}
	if code, out, _ := runCmd(env, "maintenance", "status"); code != 0 || out != "enabled=true running_turns=0 open_hosts=0\n" {
		t.Fatalf("status after on: exit %d, stdout %q", code, out)
	}
	if code, out, errOut := runCmd(env, "maintenance", "off"); code != 0 || out != "enabled=false running_turns=0 open_hosts=0\n" {
		t.Fatalf("off: exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	// Bad flags are usage errors.
	for _, args := range [][]string{
		{"maintenance", "on", "--drain-timeout=soon"},
		{"maintenance", "on", "--drain-timeout=-1s"},
		{"maintenance", "on", "extra"},
		{"maintenance", "off", "--reason", "x"},
		{"maintenance", "status", "now"},
	} {
		if code, _, _ := runCmd(env, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestMaintenanceAdminAddress(t *testing.T) {
	// A non-loopback admin address is refused before any request.
	env := func(k string) string {
		if k == "LUMA_ADMIN_LISTEN" {
			return "0.0.0.0:8082"
		}
		return ""
	}
	if code, _, errOut := runCmd(env, "maintenance", "status"); code != 2 || !strings.Contains(errOut, "loopback") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	// Nothing listening: a plain failure, not a usage error.
	gone := func(k string) string {
		if k == "LUMA_ADMIN_LISTEN" {
			return "127.0.0.1:1"
		}
		return ""
	}
	if code, out, errOut := runCmd(gone, "maintenance", "status"); code != 1 || out != "" || errOut == "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := adminURL(adminAddr(noEnv)); got != "http://127.0.0.1:8082/maintenance" {
		t.Fatalf("default admin URL = %q", got)
	}
}
