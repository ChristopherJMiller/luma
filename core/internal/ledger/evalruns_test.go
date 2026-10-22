package ledger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/internalauth"
	"github.com/ChristopherJMiller/luma/core/internal/ledger"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

const audience = "luma-internal"

var principals = map[string]internalauth.Principal{
	"system:serviceaccount:luma:luma-conductor": internalauth.PrincipalConductor,
	"system:serviceaccount:luma:luma-evals":     internalauth.PrincipalEvals,
}

// fakeReviewer authenticates a bearer that is a ServiceAccount username.
type fakeReviewer struct{}

func (fakeReviewer) Review(_ context.Context, token string) (internalauth.Identity, error) {
	user := map[string]string{
		"tok.conductor.x": "system:serviceaccount:luma:luma-conductor",
		"tok.evals.x":     "system:serviceaccount:luma:luma-evals",
	}[token]
	if user == "" {
		return internalauth.Identity{}, internalauth.ErrUnauthenticated
	}
	return internalauth.Identity{Username: user, Audiences: []string{audience}, PodName: "pod-x"}, nil
}

// evalsOnly is the route as Core mounts it: behind internalauth, evals only.
func evalsOnly(h http.Handler) http.Handler {
	return internalauth.Middleware(fakeReviewer{}, audience, principals, internalauth.PrincipalEvals)(h)
}

func evalRun(suite string, cost float64, passed, failed int, finished time.Time) protocol.EvalRun {
	return protocol.EvalRun{Suite: suite, SDKVersion: "0.9.1", CLIVersion: "2.1.301", Image: "ghcr.io/x/luma-conductor:sha-abcdef0",
		StartedAt: finished.Add(-10 * time.Minute), FinishedAt: finished, Passed: passed, Failed: failed, CostUSD: cost,
		Results: []protocol.EvalResult{{ID: "cache.identical", Passed: failed == 0, CostUSD: cost}}}
}

func post(t *testing.T, h http.Handler, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	switch v := body.(type) {
	case string:
		b = []byte(v)
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/eval-runs", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (h *harness) evalRunCount() int {
	h.t.Helper()
	var n int
	if err := storetest.ConnAs(h.t, h.st, storetest.Superuser).QueryRow(bg, `select count(*) from eval_runs`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestEvalRunRequiresEvalsPrincipal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	handler := ledger.EvalRunsHandler(h.l, h.st, 25)
	run := evalRun("nightly", 1.5, 5, 0, testNow)

	routed := evalsOnly(handler)
	for _, c := range []struct {
		name, token string
		want        int
	}{
		{"no bearer", "", http.StatusUnauthorized},
		{"unknown bearer", "tok.nobody.x", http.StatusUnauthorized},
		{"the conductor", "tok.conductor.x", http.StatusForbidden},
	} {
		if rec := post(t, routed, c.token, run); rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
	// The handler checks the principal itself: a wider middleware, or none,
	// lets nobody else in.
	wide := internalauth.Middleware(fakeReviewer{}, audience, principals,
		internalauth.PrincipalConductor, internalauth.PrincipalEvals)(handler)
	if rec := post(t, wide, "tok.conductor.x", run); rec.Code != http.StatusForbidden {
		t.Errorf("conductor behind a wider middleware: status %d, want 403", rec.Code)
	}
	if rec := post(t, handler, "tok.evals.x", run); rec.Code != http.StatusForbidden {
		t.Errorf("no middleware: status %d, want 403", rec.Code)
	}
	if n := h.evalRunCount(); n != 0 {
		t.Errorf("refused posts stored %d eval runs", n)
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Errorf("refused posts made %d ledger rows", len(rows))
	}

	if rec := post(t, routed, "tok.evals.x", run); rec.Code != http.StatusCreated {
		t.Errorf("evals: status %d, want 201 (%s)", rec.Code, rec.Body)
	}
	if n := h.evalRunCount(); n != 1 {
		t.Errorf("%d eval runs stored, want 1", n)
	}
}

func TestEvalRunCostBounds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	routed := evalsOnly(ledger.EvalRunsHandler(h.l, h.st, 25))
	for _, c := range []struct {
		cost float64
		want int
	}{
		{-0.01, http.StatusUnprocessableEntity},
		{25.01, http.StatusUnprocessableEntity},
		{0, http.StatusCreated},
		{25, http.StatusCreated},
	} {
		run := evalRun("gate", c.cost, 3, 0, testNow)
		run.Results[0].CostUSD = 0 // only the run's own cost_usd is out of bounds
		rec := post(t, routed, "tok.evals.x", run)
		if rec.Code != c.want {
			t.Errorf("cost %v: status %d, want %d", c.cost, rec.Code, c.want)
		}
		if c.want == http.StatusUnprocessableEntity {
			var e protocol.ErrorInfo
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != protocol.CodeInvalidRequest {
				t.Errorf("cost %v: body %s, want an invalid_request ErrorInfo", c.cost, rec.Body)
			}
		}
	}
	if n := h.evalRunCount(); n != 2 {
		t.Errorf("%d eval runs stored, want the two in bounds", n)
	}
	if s := h.l.Budget().Status(bg, testNow); !near(s.SpentUSD, 25) {
		t.Errorf("spent = %v, want 25 (the $0 run adds nothing)", s.SpentUSD)
	}
}

func TestEvalRunRejectsMalformedBodies(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	routed := evalsOnly(ledger.EvalRunsHandler(h.l, h.st, 25))
	good, err := json.Marshal(evalRun("weekly", 1, 3, 1, testNow))
	if err != nil {
		t.Fatal(err)
	}
	without := func(key string) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		delete(m, key)
		b, _ := json.Marshal(m)
		return string(b)
	}
	with := func(key, raw string) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		m[key] = json.RawMessage(raw)
		b, _ := json.Marshal(m)
		return string(b)
	}
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"not JSON", "{", http.StatusBadRequest},
		{"no cost_usd", without("cost_usd"), http.StatusBadRequest},
		{"no suite", without("suite"), http.StatusBadRequest},
		{"no results", without("results"), http.StatusBadRequest},
		{"a result without its id", with("results", `[{"passed":true,"failures":[],"cost_usd":0}]`), http.StatusBadRequest},
		{"cost as a string", with("cost_usd", `"1"`), http.StatusBadRequest},
		{"an empty suite", with("suite", `""`), http.StatusUnprocessableEntity},
		{"negative failed", with("failed", `-1`), http.StatusUnprocessableEntity},
		{"finished before it started", with("finished_at", `"2026-10-15T11:00:00Z"`), http.StatusUnprocessableEntity},
		{"a negative result cost", with("results", `[{"id":"a","passed":true,"failures":[],"cost_usd":-1}]`), http.StatusUnprocessableEntity},
		{"a null cost", with("cost_usd", `null`), http.StatusBadRequest},
		{"over 1 MiB", with("image", `"`+strings.Repeat("x", 1<<20)+`"`), http.StatusRequestEntityTooLarge},
		{"an unknown key is ignored (S37)", with("extra", `{"a":1}`), http.StatusCreated},
	} {
		if rec := post(t, routed, "tok.evals.x", c.body); rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
	}
	if n := h.evalRunCount(); n != 1 {
		t.Errorf("%d eval runs stored, want 1", n)
	}
}

func TestEvalRunAcceptsTheContractFixture(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	routed := evalsOnly(ledger.EvalRunsHandler(h.l, h.st, 25))
	b, err := os.ReadFile("../../../protocol/fixtures/internal/eval-run.json")
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(t, routed, "tok.evals.x", string(b)); rec.Code != http.StatusCreated {
		t.Fatalf("the C10 fixture: status %d (%s)", rec.Code, rec.Body)
	}
	got, err := h.st.LastEvalRun(bg, "golden")
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed != 1 || got.Failed != 1 || !near(got.CostUSD, 1.25) || len(got.Results) != 2 ||
		got.Results[1].Failures[0] != "expected a WebSearch tool use" {
		t.Errorf("stored run = %+v", got)
	}
	// "golden" is not one of the runner's suites: its series are "other".
	if v := h.value("luma_eval_cost_usd_total", map[string]string{"suite": "other"}); !near(v, 1.25) {
		t.Errorf("luma_eval_cost_usd_total{other} = %v, want 1.25", v)
	}
	if v, ok := h.metric("luma_eval_last_run_success", map[string]string{"suite": "other"}); !ok || v != 0 {
		t.Errorf("luma_eval_last_run_success{other} = %v (present %v), want 0", v, ok)
	}
	if _, ok := h.metric("luma_eval_cost_usd_total", map[string]string{"suite": "golden"}); ok {
		t.Error("a suite outside the runner's became a label value")
	}
}

func TestEvalRunCostCountsTowardBudget(t *testing.T) {
	t.Parallel()
	// The e2e budget (S6): one $17 run crosses 80 % of $20.
	h := newHarness(t, ledger.Config{MonthlyUSD: 20})
	routed := evalsOnly(ledger.EvalRunsHandler(h.l, h.st, 25))
	if rec := post(t, routed, "tok.evals.x", evalRun("nightly", 17, 6, 0, testNow)); rec.Code != http.StatusCreated {
		t.Fatalf("status %d (%s)", rec.Code, rec.Body)
	}
	// The post itself moves the gauge (read before anything calls Status).
	if v := h.value("luma_budget_spent_usd", nil); !near(v, 17) {
		t.Errorf("luma_budget_spent_usd = %v, want 17", v)
	}
	s := h.l.Budget().Status(bg, testNow)
	if !near(s.SpentUSD, 17) || !s.Warn || s.Exhausted || !near(s.Ratio, 0.85) {
		t.Errorf("after a $17 eval run: %+v, want spent 17, ratio 0.85, warn", s)
	}
	if v := h.value("luma_eval_cost_usd_total", map[string]string{"suite": "nightly"}); !near(v, 17) {
		t.Errorf("luma_eval_cost_usd_total{nightly} = %v, want 17", v)
	}
	if rec := post(t, routed, "tok.evals.x", evalRun("weekly", 3, 2, 0, testNow)); rec.Code != http.StatusCreated {
		t.Fatalf("status %d (%s)", rec.Code, rec.Body)
	}
	if e := h.l.Budget().CheckNewTurn(bg); e == nil || e.Code != protocol.CodeBudgetExhausted {
		t.Errorf("after $20 of eval runs: %+v, want budget_exhausted", e)
	}
}

func TestEvalRunLedgerRowIsCiWorkspace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	routed := evalsOnly(ledger.EvalRunsHandler(h.l, h.st, 25))
	finished := testNow.Add(-time.Minute)
	if rec := post(t, routed, "tok.evals.x", evalRun("gate", 1.25, 14, 1, finished)); rec.Code != http.StatusCreated {
		t.Fatalf("status %d (%s)", rec.Code, rec.Body)
	}
	rows := h.rows()
	if len(rows) != 1 {
		t.Fatalf("%d ledger rows, want 1", len(rows))
	}
	r := rows[0]
	if r.SessionID != nil || r.PersonID != "ci" || r.Source != "ci" || r.Workspace != "ci" || !near(r.CostUSD, 1.25) {
		t.Errorf("row = %+v, want person ci, source ci, api_workspace ci, no session, $1.25", r)
	}
	if r.AgentID != "evals" || r.Tier != "ci" || r.Model != "eval-run" || r.TurnID != "eval_gate_20261015T115900Z" {
		t.Errorf("row = %+v, want agent evals, tier ci, model eval-run, turn eval_gate_20261015T115900Z", r)
	}
	got, err := h.st.LastEvalRun(bg, "gate")
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed != 14 || got.Failed != 1 || !got.FinishedAt.Equal(finished) || len(got.Results) != 1 {
		t.Errorf("stored run = %+v", got)
	}
	// The gauges move with the post.
	if v := h.value("luma_eval_last_run_timestamp_seconds", map[string]string{"suite": "gate"}); v != float64(finished.Unix()) {
		t.Errorf("last run timestamp = %v, want %d", v, finished.Unix())
	}
	if v, ok := h.metric("luma_eval_last_run_success", map[string]string{"suite": "gate"}); !ok || v != 0 {
		t.Errorf("last run success = %v (present %v), want 0 for a run with a failure", v, ok)
	}
	// An older run posted late does not move the gauges back.
	if rec := post(t, routed, "tok.evals.x", evalRun("gate", 0.5, 15, 0, finished.Add(-time.Hour))); rec.Code != http.StatusCreated {
		t.Fatalf("status %d (%s)", rec.Code, rec.Body)
	}
	if v := h.value("luma_eval_last_run_timestamp_seconds", map[string]string{"suite": "gate"}); v != float64(finished.Unix()) {
		t.Errorf("after a late older run: timestamp = %v, want %d", v, finished.Unix())
	}
	if v := h.value("luma_eval_last_run_success", map[string]string{"suite": "gate"}); v != 0 {
		t.Errorf("after a late older run: success = %v, want 0", v)
	}
}

func TestEvalGaugesInitialisedFromDB(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{MonthlyUSD: 150})
	nightly := time.Date(2026, 10, 15, 3, 10, 0, 0, time.UTC)
	weekly := time.Date(2026, 10, 12, 4, 30, 0, 0, time.UTC)
	for _, r := range []protocol.EvalRun{
		evalRun("nightly", 0.4, 2, 3, nightly.Add(-24*time.Hour)), // older, failed
		evalRun("nightly", 0.5, 5, 0, nightly),                    // newest, passed
		evalRun("weekly", 2.0, 8, 1, weekly),                      // failed
	} {
		if _, err := h.st.InsertEvalRun(bg, r); err != nil {
			t.Fatal(err)
		}
	}
	h.spend(testNow.Add(-time.Hour), 12.5, "interactive")

	// A restarted Core: a fresh registry, nothing posted since.
	h.l = h.ledger(ledger.Config{MonthlyUSD: 150})
	if err := h.l.InitGauges(bg); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		suite   string
		ts      time.Time
		success float64
	}{
		{"nightly", nightly, 1}, {"weekly", weekly, 0},
	} {
		if v := h.value("luma_eval_last_run_timestamp_seconds", map[string]string{"suite": c.suite}); v != float64(c.ts.Unix()) {
			t.Errorf("%s timestamp = %v, want %d", c.suite, v, c.ts.Unix())
		}
		if v, ok := h.metric("luma_eval_last_run_success", map[string]string{"suite": c.suite}); !ok || v != c.success {
			t.Errorf("%s success = %v (present %v), want %v", c.suite, v, ok, c.success)
		}
	}
	// A suite that never ran has no series (an alert on it stays empty, not 0).
	if n := h.series("luma_eval_last_run_success"); n != 2 {
		t.Errorf("%d success series, want 2", n)
	}
	if v := h.value("luma_budget_spent_usd", nil); !near(v, 12.5) {
		t.Errorf("luma_budget_spent_usd = %v, want 12.5", v)
	}
	if v := h.value("luma_budget_cap_usd", nil); v != 150 {
		t.Errorf("luma_budget_cap_usd = %v, want 150", v)
	}
}
