package reconcile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ChristopherJMiller/luma/core/internal/internalauth"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/reconcile"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

var bg = context.Background()

const audience = "luma-internal"

var principals = map[string]internalauth.Principal{
	"system:serviceaccount:luma:luma-evals":           internalauth.PrincipalEvals,
	"system:serviceaccount:luma:luma-usage-reconcile": internalauth.PrincipalReconcile,
}

type fakeReviewer struct{}

func (fakeReviewer) Review(_ context.Context, token string) (internalauth.Identity, error) {
	user := map[string]string{
		"tok.evals.x":     "system:serviceaccount:luma:luma-evals",
		"tok.reconcile.x": "system:serviceaccount:luma:luma-usage-reconcile",
	}[token]
	if user == "" {
		return internalauth.Identity{}, internalauth.ErrUnauthenticated
	}
	return internalauth.Identity{Username: user, Audiences: []string{audience}, PodName: "pod-x"}, nil
}

type env struct {
	t  *testing.T
	st *store.Store
	m  *metrics.Core
	h  http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := storetest.New(t)
	m := metrics.NewCore(prometheus.NewRegistry())
	// Mounted as Core does: behind internalauth, reconcile only.
	h := internalauth.Middleware(fakeReviewer{}, audience, principals, internalauth.PrincipalReconcile, internalauth.PrincipalEvals)(reconcile.Handler(st, m))
	return &env{t: t, st: st, m: m, h: h}
}

func (e *env) post(token string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/usage-reconciliations", bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) spend(at time.Time, usd float64, ws string) {
	e.t.Helper()
	if _, err := e.st.RecordLedger(bg, store.LedgerRow{PersonID: "chris", AgentID: "luma", Tier: "fast", Source: "chat",
		APIWorkspace: ws, Model: "m", TurnID: "t" + at.Format(time.RFC3339Nano), CostUSD: usd, CreatedAt: at}); err != nil {
		e.t.Fatal(err)
	}
}

func body(day string, ws ...protocol.ReconcileWorkspace) protocol.UsageReconciliation {
	return protocol.UsageReconciliation{Day: day, Workspaces: ws}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func gauge(t *testing.T, g prometheus.Gauge) float64 { t.Helper(); return val(t, g) }

func TestHandlerRequiresReconcilePrincipal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := body("2026-10-14", protocol.ReconcileWorkspace{Name: "luma-ci", APIUSD: 1})
	for token, want := range map[string]int{"": 401, "garbage": 401, "tok.evals.x": 403} {
		if rec := e.post(token, b); rec.Code != want {
			t.Errorf("token %q: %d, want %d", token, rec.Code, want)
		}
	}
	// Mounted wider than intended, the handler still refuses the evals caller.
	if rec := e.post("tok.evals.x", b); rec.Code != 403 {
		t.Errorf("evals through a wide mount: %d", rec.Code)
	}
	if rows, _ := e.st.Reconciliations(bg, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)); len(rows) != 0 {
		t.Errorf("a refused caller stored %d rows", len(rows))
	}
	if rec := e.post("tok.reconcile.x", b); rec.Code != 200 {
		t.Errorf("reconcile: %d %s", rec.Code, rec.Body)
	}
}

func TestDriftComputedFromLedgerDay(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, time.UTC) }
	e.spend(d(13, 23, 59), 9.00, "interactive") // the day before
	e.spend(d(14, 0, 0), 4.00, "interactive")   // first instant of the day
	e.spend(d(14, 23, 59), 6.00, "interactive") // last minute of the day
	e.spend(d(15, 0, 0), 7.00, "interactive")   // the day after
	rec := e.post("tok.reconcile.x", body("2026-10-14", protocol.ReconcileWorkspace{Name: "luma-interactive", APIUSD: 8}))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var res protocol.UsageReconciliationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// ledger 10.00, api 8.00: drift +2.00, ratio 0.25.
	if len(res.Workspaces) != 1 || res.Workspaces[0].Name != "luma-interactive" || !near(res.Workspaces[0].APIUSD, 8) ||
		!near(res.Workspaces[0].LedgerUSD, 10) || !near(res.Workspaces[0].DriftRatio, 0.25) {
		t.Errorf("result = %+v", res)
	}
	if v := val(t, e.m.UsageReconcileDriftUSD.WithLabelValues("luma-interactive")); !near(v, 2) {
		t.Errorf("drift_usd = %v, want 2", v)
	}
	if v := val(t, e.m.UsageReconcileDriftRatio.WithLabelValues("luma-interactive")); !near(v, 0.25) {
		t.Errorf("drift_ratio = %v, want 0.25", v)
	}
	if v := gauge(t, e.m.UsageReconcileLastSuccessTimestampSeconds); math.Abs(v-float64(time.Now().Unix())) > 30 {
		t.Errorf("last success = %v", v)
	}
	// A zero API day divides by the 0.01 floor: ledger 1.00 -> ratio 100.
	e.spend(d(16, 1, 0), 1.00, "ci")
	e.post("tok.reconcile.x", body("2026-10-16", protocol.ReconcileWorkspace{Name: "luma-ci", APIUSD: 0}))
	if v := val(t, e.m.UsageReconcileDriftRatio.WithLabelValues("luma-ci")); !near(v, 100) {
		t.Errorf("ci drift_ratio = %v, want 100", v)
	}
}

func TestWorkspaceNameMapping(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	e.spend(at, 3, "ci")
	e.spend(at.Add(time.Minute), 5, "interactive")
	e.spend(at.Add(2*time.Minute), 7, "interactive")
	rec := e.post("tok.reconcile.x", body("2026-10-14",
		protocol.ReconcileWorkspace{Name: "luma-ci", APIUSD: 3}, protocol.ReconcileWorkspace{Name: "luma-interactive", APIUSD: 12}))
	var res protocol.UsageReconciliationResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || len(res.Workspaces) != 2 || !near(res.Workspaces[0].LedgerUSD, 3) || !near(res.Workspaces[1].LedgerUSD, 12) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rows, _ := e.st.Reconciliations(bg, at)
	if len(rows) != 2 || rows[0].Workspace != "luma-ci" || rows[1].Workspace != "luma-interactive" {
		t.Errorf("rows store the Anthropic names: %+v", rows)
	}
	// An unknown name is 422 and the valid one beside it is not stored either.
	other := newEnv(t)
	rec = other.post("tok.reconcile.x", body("2026-10-14",
		protocol.ReconcileWorkspace{Name: "luma-ci", APIUSD: 1}, protocol.ReconcileWorkspace{Name: "luma-other", APIUSD: 1}))
	if rec.Code != 422 {
		t.Errorf("luma-other: %d, want 422", rec.Code)
	}
	if rows, _ := other.st.Reconciliations(bg, at); len(rows) != 0 {
		t.Errorf("a refused request stored %+v", rows)
	}
	for name, b := range map[string]any{
		"bad day":      body("14-10-2026", protocol.ReconcileWorkspace{Name: "luma-ci", APIUSD: 1}),
		"negative usd": body("2026-10-14", protocol.ReconcileWorkspace{Name: "luma-ci", APIUSD: -1}),
	} {
		if rec := other.post("tok.reconcile.x", b); rec.Code != 422 {
			t.Errorf("%s: %d, want 422", name, rec.Code)
		}
	}
	if rec := other.post("tok.reconcile.x", map[string]any{"day": "2026-10-14"}); rec.Code != 400 {
		t.Errorf("missing workspaces: %d, want 400", rec.Code)
	}
}

func TestUpsertIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	e.spend(day.Add(time.Hour), 4, "ci")
	for _, api := range []float64{4, 4, 5} {
		if rec := e.post("tok.reconcile.x", body("2026-10-14", protocol.ReconcileWorkspace{Name: "luma-ci", APIUSD: api})); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	rows, _ := e.st.Reconciliations(bg, day)
	if len(rows) != 1 || !near(rows[0].APIUSD, 5) || !near(rows[0].LedgerUSD, 4) {
		t.Errorf("rows = %+v, want one with api 5 ledger 4", rows)
	}
	if v := val(t, e.m.UsageReconcileDriftUSD.WithLabelValues("luma-ci")); !near(v, -1) {
		t.Errorf("drift_usd = %v, want -1", v)
	}
}

func TestReconcileGaugesInitialisedFromDB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, r := range []store.Reconciliation{
		{Day: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), Workspace: "luma-ci", APIUSD: 1, LedgerUSD: 9}, // older: ignored
		{Day: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), Workspace: "luma-ci", APIUSD: 2, LedgerUSD: 3},
		{Day: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), Workspace: "luma-interactive", APIUSD: 10, LedgerUSD: 9},
	} {
		if err := e.st.UpsertReconciliation(bg, r); err != nil {
			t.Fatal(err)
		}
	}
	// A restarted Core: a fresh registry and nothing posted since.
	m := metrics.NewCore(prometheus.NewRegistry())
	if err := reconcile.InitGauges(bg, e.st, m); err != nil {
		t.Fatal(err)
	}
	for ws, want := range map[string][2]float64{"luma-ci": {1, 0.5}, "luma-interactive": {-1, -0.1}} {
		if v := val(t, m.UsageReconcileDriftUSD.WithLabelValues(ws)); !near(v, want[0]) {
			t.Errorf("%s drift_usd = %v, want %v", ws, v, want[0])
		}
		if v := val(t, m.UsageReconcileDriftRatio.WithLabelValues(ws)); !near(v, want[1]) {
			t.Errorf("%s drift_ratio = %v, want %v", ws, v, want[1])
		}
	}
	if v := gauge(t, m.UsageReconcileLastSuccessTimestampSeconds); math.Abs(v-float64(time.Now().Unix())) > 60 {
		t.Errorf("last success = %v, want about now (the rows' created_at)", v)
	}
	// A store with no rows leaves the timestamp at 0 and no drift series.
	empty := metrics.NewCore(prometheus.NewRegistry())
	if err := reconcile.InitGauges(bg, storetest.New(t), empty); err != nil {
		t.Fatal(err)
	}
	if v := gauge(t, empty.UsageReconcileLastSuccessTimestampSeconds); v != 0 || series(empty.UsageReconcileDriftUSD) != 0 {
		t.Errorf("empty store: last success %v, series %d", v, series(empty.UsageReconcileDriftUSD))
	}
}

func val(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	var m dto.Metric
	if err := (<-ch).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

func series(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 16)
	c.Collect(ch)
	close(ch)
	return len(ch)
}
