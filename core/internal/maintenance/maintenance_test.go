package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/luma/core/internal/link"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

// The contracts this package meets and consumes.
var (
	_ sessions.Gate = (*Maintenance)(nil)
	_ Fleet         = (*link.Server)(nil)
	_ Settings      = (*store.Store)(nil)
)

var bg = context.Background()

// ---- fakes ----

type fakeSettings struct {
	mu   sync.Mutex
	vals map[string]json.RawMessage
	err  error
}

func (s *fakeSettings) Setting(_ context.Context, key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}

func (s *fakeSettings) SetSetting(_ context.Context, key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if s.vals == nil {
		s.vals = map[string]json.RawMessage{}
	}
	s.vals[key] = b
	return nil
}

// fakeFleet is the conductor: running turns end when the test says so, or on
// interrupt if endOnInterrupt.
type fakeFleet struct {
	mu             sync.Mutex
	start          time.Time
	running        map[string]bool
	hosts          int
	endOnInterrupt bool
	calls          []string        // "interrupt:<id>", "shutdown:<reason>"
	at             []time.Duration // when, since start
}

func newFleet(hosts int, running ...string) *fakeFleet {
	f := &fakeFleet{start: time.Now(), running: map[string]bool{}, hosts: hosts}
	for _, id := range running {
		f.running[id] = true
	}
	return f
}

func (f *fakeFleet) RunningTurns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.running {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (f *fakeFleet) OpenHosts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts
}

func (f *fakeFleet) Interrupt(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "interrupt:"+id)
	f.at = append(f.at, time.Since(f.start))
	if f.endOnInterrupt {
		delete(f.running, id)
	}
	return nil
}

func (f *fakeFleet) Shutdown(_ context.Context, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "shutdown:"+reason)
	f.at = append(f.at, time.Since(f.start))
	f.hosts = 0
	return nil
}

func (f *fakeFleet) end(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, id)
}

func (f *fakeFleet) Calls() ([]string, []time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), slices.Clone(f.at)
}

func newTest(t *testing.T, st Settings, f Fleet) (*Maintenance, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := New(bg, st, f, metrics.NewCore(reg))
	if err != nil {
		t.Fatal(err)
	}
	m.poll = 5 * time.Millisecond
	m.grace = 150 * time.Millisecond
	return m, reg
}

func enabledGauge(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "luma_maintenance_enabled" {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("no luma_maintenance_enabled metric")
	return 0
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

var maintenanceInfo = protocol.ErrorInfo{Code: protocol.CodeMaintenance, Message: "Luma is down for maintenance. Try again later."}

// ---- tests ----

func TestMaintenanceRefusesCreate(t *testing.T) {
	m, reg := newTest(t, &fakeSettings{}, newFleet(0))
	if info := m.CheckNewTurn(bg); info != nil {
		t.Fatalf("CheckNewTurn with maintenance off = %#v", info)
	}
	if m.Enabled() || enabledGauge(t, reg) != 0 {
		t.Fatal("enabled before On")
	}
	if _, err := m.On(bg, "upgrade", 0); err != nil {
		t.Fatal(err)
	}
	info := m.CheckNewTurn(bg)
	if info == nil || *info != maintenanceInfo {
		t.Fatalf("CheckNewTurn with maintenance on = %#v, want %#v", info, maintenanceInfo)
	}
	if !m.Enabled() || enabledGauge(t, reg) != 1 {
		t.Fatal("Enabled()/gauge not set by On")
	}
	if _, err := m.Off(bg); err != nil {
		t.Fatal(err)
	}
	if info := m.CheckNewTurn(bg); info != nil {
		t.Fatalf("CheckNewTurn after Off = %#v", info)
	}
	if m.Enabled() || enabledGauge(t, reg) != 0 {
		t.Fatal("Enabled()/gauge still set after Off")
	}
}

func TestMaintenanceDrainsThenShutsDown(t *testing.T) {
	if DefaultDrainTimeout != 120*time.Second || interruptGrace != 10*time.Second {
		t.Fatalf("drain %v, grace %v; want 120s and 10s", DefaultDrainTimeout, interruptGrace)
	}

	t.Run("waits for the drain timeout, interrupts what still runs, then shuts down", func(t *testing.T) {
		f := newFleet(3, "ses_A", "ses_B")
		m, _ := newTest(t, &fakeSettings{}, f)
		go func() {
			time.Sleep(50 * time.Millisecond)
			f.end("ses_A") // finishes inside the drain
		}()
		done := make(chan Status, 1)
		go func() {
			s, err := m.On(bg, "token rotation", 300*time.Millisecond)
			if err != nil {
				t.Error(err)
			}
			done <- s
		}()
		// New work is refused from the start of the drain.
		eventually(t, "the gate closes", func() bool { return m.CheckNewTurn(bg) != nil })
		var st Status
		select {
		case st = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("On did not return")
		}
		calls, at := f.Calls()
		if want := []string{"interrupt:ses_B", "shutdown:maintenance"}; !reflect.DeepEqual(calls, want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		if at[0] < 300*time.Millisecond {
			t.Fatalf("interrupted at %v, before the 300ms drain timeout", at[0])
		}
		// ses_B ignores its interrupt, so shutdown waits out the grace.
		if at[1]-at[0] < 150*time.Millisecond {
			t.Fatalf("shut down %v after the interrupt, before the grace", at[1]-at[0])
		}
		if !st.Enabled || st.Reason != "token rotation" || st.OpenHosts != 0 {
			t.Fatalf("status = %#v", st)
		}
	})

	t.Run("an interrupted turn that ends lets shutdown go at once", func(t *testing.T) {
		f := newFleet(1, "ses_A")
		f.endOnInterrupt = true
		m, _ := newTest(t, &fakeSettings{}, f)
		if _, err := m.On(bg, "", 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		calls, at := f.Calls()
		if want := []string{"interrupt:ses_A", "shutdown:maintenance"}; !reflect.DeepEqual(calls, want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		if at[1]-at[0] >= 150*time.Millisecond {
			t.Fatalf("shutdown waited %v after the turn ended", at[1]-at[0])
		}
	})

	t.Run("turns that finish inside the drain are not interrupted", func(t *testing.T) {
		f := newFleet(2, "ses_A")
		m, _ := newTest(t, &fakeSettings{}, f)
		go func() {
			time.Sleep(30 * time.Millisecond)
			f.end("ses_A")
		}()
		if _, err := m.On(bg, "", 2*time.Second); err != nil {
			t.Fatal(err)
		}
		calls, at := f.Calls()
		if want := []string{"shutdown:maintenance"}; !reflect.DeepEqual(calls, want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		if at[0] >= time.Second {
			t.Fatalf("shut down at %v: the drain waited past the last turn", at[0])
		}
	})

	t.Run("Off during the drain stops it", func(t *testing.T) {
		f := newFleet(1, "ses_A")
		m, _ := newTest(t, &fakeSettings{}, f)
		done := make(chan Status, 1)
		go func() {
			s, _ := m.On(bg, "", 2*time.Second)
			done <- s
		}()
		eventually(t, "the gate closes", func() bool { return m.Enabled() })
		if _, err := m.Off(bg); err != nil {
			t.Fatal(err)
		}
		select {
		case s := <-done:
			if s.Enabled {
				t.Fatalf("On returned %#v after Off", s)
			}
		case <-time.After(time.Second):
			t.Fatal("On kept draining after Off")
		}
		if calls, _ := f.Calls(); len(calls) != 0 {
			t.Fatalf("calls after an aborted drain = %v", calls)
		}
		if m.CheckNewTurn(bg) != nil {
			t.Fatal("the gate is still closed after Off")
		}
	})

	t.Run("a failed save keeps the gate closed and reports the error", func(t *testing.T) {
		f := newFleet(0)
		st := &fakeSettings{err: errors.New("db down")}
		m, _ := newTest(t, st, f)
		if _, err := m.On(bg, "", 0); err == nil {
			t.Fatal("On with an unwritable setting returned nil")
		}
		if m.CheckNewTurn(bg) == nil {
			t.Fatal("the gate opened after a failed save")
		}
		// Off fails closed too: maintenance stays on until it is saved off.
		if _, err := m.Off(bg); err == nil {
			t.Fatal("Off with an unwritable setting returned nil")
		}
		if !m.Enabled() {
			t.Fatal("maintenance went off without being saved")
		}
	})
}

func TestMaintenancePersistsAcrossRestart(t *testing.T) {
	st := storetest.New(t)
	f := newFleet(0)
	m1, _ := newTest(t, st, f)
	before := time.Now().Add(-time.Second)
	if _, err := m1.On(bg, "node upgrade", 0); err != nil {
		t.Fatal(err)
	}

	// The stored value is settings.maintenance = {enabled, reason, since}.
	raw, err := st.Setting(bg, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(stored))
	for k := range stored {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !reflect.DeepEqual(keys, []string{"enabled", "reason", "since"}) || stored["enabled"] != true || stored["reason"] != "node upgrade" {
		t.Fatalf("settings.maintenance = %s", raw)
	}

	// A restarted Core comes up in maintenance, with the same reason and since.
	m2, reg := newTest(t, st, f)
	if !m2.Enabled() || m2.CheckNewTurn(bg) == nil || enabledGauge(t, reg) != 1 {
		t.Fatal("maintenance did not survive a restart")
	}
	s := m2.Status()
	if s.Reason != "node upgrade" || s.Since.Before(before) || !s.Since.Equal(m1.Status().Since) {
		t.Fatalf("status after restart = %#v", s)
	}

	if _, err := m2.Off(bg); err != nil {
		t.Fatal(err)
	}
	m3, _ := newTest(t, st, f)
	if m3.Enabled() || m3.CheckNewTurn(bg) != nil {
		t.Fatal("Off did not survive a restart")
	}
}

func TestMaintenanceStartsOffWithNothingStored(t *testing.T) {
	m, _ := newTest(t, &fakeSettings{}, newFleet(0))
	if m.Enabled() {
		t.Fatal("enabled with no stored setting")
	}
	// A stored value that does not decode is a startup error, not "off".
	bad := &fakeSettings{vals: map[string]json.RawMessage{"maintenance": json.RawMessage(`"yes"`)}}
	if _, err := New(bg, bad, newFleet(0), metrics.NewCore(prometheus.NewRegistry())); err == nil {
		t.Fatal("New accepted an undecodable settings.maintenance")
	}
}

func TestAdminListenerIsLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8082", ":8082", "[::]:8082", "localhost:8082", "192.0.2.1:8082", "8082"} {
		if l, err := ListenAdmin(addr); err == nil {
			_ = l.Close()
			t.Fatalf("ListenAdmin(%q) listened", addr)
		}
		if err := CheckAdminAddr(addr); err == nil {
			t.Fatalf("CheckAdminAddr(%q) = nil", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		if err := CheckAdminAddr(addr); err != nil {
			t.Fatalf("CheckAdminAddr(%q) = %v", addr, err)
		}
		l, err := ListenAdmin(addr)
		if errors.Is(err, syscall.EADDRNOTAVAIL) && strings.Contains(addr, "::1") {
			t.Logf("no IPv6 loopback on this host: %v", err)
			continue
		}
		if err != nil {
			t.Fatalf("ListenAdmin(%q) = %v", addr, err)
		}
		if ip := l.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
			t.Fatalf("ListenAdmin(%q) listens on %v", addr, ip)
		}
		_ = l.Close()
	}
}

func TestAdminHandler(t *testing.T) {
	f := newFleet(2, "ses_A")
	f.endOnInterrupt = true
	m, _ := newTest(t, &fakeSettings{}, f)
	srv := httptest.NewServer(m.AdminHandler())
	defer srv.Close()

	do := func(method, ctype, body string) (int, Status) {
		t.Helper()
		req, err := http.NewRequestWithContext(bg, method, srv.URL+"/maintenance", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var s Status
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, s
	}

	code, s := do(http.MethodGet, "", "")
	if code != http.StatusOK || s.Enabled || s.RunningTurns != 1 || s.OpenHosts != 2 {
		t.Fatalf("GET = %d %#v", code, s)
	}
	code, s = do(http.MethodPost, "application/json", `{"enabled":true,"reason":"upgrade","drain_timeout":"50ms"}`)
	if code != http.StatusOK || !s.Enabled || s.Reason != "upgrade" || s.RunningTurns != 0 || s.OpenHosts != 0 {
		t.Fatalf("POST on = %d %#v", code, s)
	}
	if calls, _ := f.Calls(); !reflect.DeepEqual(calls, []string{"interrupt:ses_A", "shutdown:maintenance"}) {
		t.Fatalf("calls = %v", calls)
	}
	code, s = do(http.MethodPost, "application/json", `{"enabled":false}`)
	if code != http.StatusOK || s.Enabled {
		t.Fatalf("POST off = %d %#v", code, s)
	}

	for _, tc := range []struct {
		method, ctype, body string
		want                int
	}{
		{http.MethodPost, "text/plain", `{"enabled":true}`, http.StatusUnsupportedMediaType},
		{http.MethodPost, "application/json", `{"enabled":`, http.StatusBadRequest},
		{http.MethodPost, "application/json", `{}`, http.StatusBadRequest},
		{http.MethodPost, "application/json", `{"enabled":true,"drain_timeout":"soon"}`, http.StatusBadRequest},
		{http.MethodPost, "application/json", `{"enabled":true,"drain_timeout":"-1s"}`, http.StatusBadRequest},
		{http.MethodPost, "application/json", `{"enabled":true,"reason":"` + strings.Repeat("x", 5000) + `"}`, http.StatusBadRequest},
		{http.MethodDelete, "", "", http.StatusMethodNotAllowed},
	} {
		if code, _ := do(tc.method, tc.ctype, tc.body); code != tc.want {
			t.Errorf("%s %s %.40s = %d, want %d", tc.method, tc.ctype, tc.body, code, tc.want)
		}
	}
	if m.Enabled() {
		t.Fatal("a refused request turned maintenance on")
	}
}

// The admin handler answers a status line the CLI prints as-is.
func TestStatusLine(t *testing.T) {
	s := Status{Enabled: true, RunningTurns: 2, OpenHosts: 3}
	if got := s.Line(); got != "enabled=true running_turns=2 open_hosts=3" {
		t.Fatalf("Line() = %q", got)
	}
	var buf bytes.Buffer
	buf.WriteString(Status{}.Line())
	if buf.String() != "enabled=false running_turns=0 open_hosts=0" {
		t.Fatalf("zero Line() = %q", buf.String())
	}
}
