package ledger_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/ledger"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/sessions"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

// The ledger is the session service's Ledger (S3) and the budget is a Gate (S1).
var (
	_ sessions.Ledger = (*ledger.Ledger)(nil)
	_ sessions.Gate   = (*ledger.Budget)(nil)
)

// labels of a fast luma turn of chris's on the web, for one model.
func costLabels(model string) map[string]string {
	return map[string]string{"person": "chris", "agent": "luma", "tier": "fast", "source": "web", "model": model}
}

func tokenLabels(model, typ string) map[string]string {
	l := costLabels(model)
	l["type"] = typ
	return l
}

func TestRecordTurnOneRowPerModel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_a", "interactive", haiku, sonnet)
	u := protocol.TurnUsage{CostUSD: 0.0143, Models: map[string]protocol.ModelUsage{
		haiku:  {Input: 100, Output: 50, CacheRead: 800, CacheCreation: 20, WebSearchRequests: 2, CostUSD: 0.0123},
		sonnet: {Input: 10, Output: 5, CostUSD: 0.002},
		// A model of the running totals that this turn did not use: its
		// delta is all zero and makes no row.
		opus: {},
	}}
	if err := h.l.RecordTurn(bg, sess, "trn_1", u, ptr("list")); err != nil {
		t.Fatal(err)
	}
	sid := "ses_a"
	want := []ledgerRow{
		{SessionID: &sid, PersonID: "chris", AgentID: "luma", Tier: "fast", Source: "web", Workspace: "interactive",
			Model: haiku, TurnID: "trn_1", Input: 100, Output: 50, CacheRead: 800, CacheCreation: 20, WS: 2, CostUSD: 0.0123},
		{SessionID: &sid, PersonID: "chris", AgentID: "luma", Tier: "fast", Source: "web", Workspace: "interactive",
			Model: sonnet, TurnID: "trn_1", Input: 10, Output: 5, CostUSD: 0.002},
	}
	if got := h.rows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %+v\nwant %+v", got, want)
	}
	if n := len(h.anomalies()); n != 0 {
		t.Errorf("%d anomalies, want 0", n)
	}

	// The metrics carry the same numbers.
	if v := h.value("luma_cost_usd_total", costLabels(haiku)); !near(v, 0.0123) {
		t.Errorf("haiku cost = %v", v)
	}
	if v := h.value("luma_cost_usd_total", costLabels(sonnet)); !near(v, 0.002) {
		t.Errorf("sonnet cost = %v", v)
	}
	for typ, want := range map[string]float64{"input": 100, "output": 50, "cache_read": 800, "cache_creation": 20} {
		if v := h.value("luma_tokens_total", tokenLabels(haiku, typ)); v != want {
			t.Errorf("haiku %s tokens = %v, want %v", typ, v, want)
		}
	}
	if v := h.value("luma_web_search_requests_total",
		map[string]string{"person": "chris", "agent": "luma", "tier": "fast", "source": "web"}); v != 2 {
		t.Errorf("web searches = %v, want 2", v)
	}
	if _, ok := h.metric("luma_cost_usd_total", costLabels(opus)); ok {
		t.Error("a zero delta made a cost series")
	}
	if n := h.series("luma_unbound_model_requests_total"); n != 0 {
		t.Errorf("%d unbound series for bound models", n)
	}

	// The row's api_workspace is the session's (S5).
	ci := h.session("ses_ci", "ci", haiku)
	if err := h.l.RecordTurn(bg, ci, "trn_2", protocol.TurnUsage{CostUSD: 0.01,
		Models: map[string]protocol.ModelUsage{haiku: {Input: 1, CostUSD: 0.01}}}, nil); err != nil {
		t.Fatal(err)
	}
	rows := h.rows()
	var found bool
	for _, r := range rows {
		if r.TurnID == "trn_2" {
			found = true
			if r.Workspace != "ci" {
				t.Errorf("ci session's row has api_workspace %q", r.Workspace)
			}
		}
	}
	if !found {
		t.Error("the ci session's turn made no row")
	}
}

func TestRowsAreDatedByCoresClock(t *testing.T) {
	t.Parallel()
	// The budget's month is Core's clock, so the rows are dated by it too,
	// not by the database's now().
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_k", "interactive", haiku)
	jan := time.Date(2027, 1, 5, 9, 0, 0, 0, time.UTC)
	h.setNow(jan)
	if err := h.l.RecordTurn(bg, sess, "trn_jan", protocol.TurnUsage{CostUSD: 0.5,
		Models: map[string]protocol.ModelUsage{haiku: {Input: 1, CostUSD: 0.5}}}, nil); err != nil {
		t.Fatal(err)
	}
	if s := h.l.Budget().Status(bg, jan); !near(s.SpentUSD, 0.5) {
		t.Errorf("January spent = %v, want 0.5", s.SpentUSD)
	}
	if s := h.l.Budget().Status(bg, testNow); s.SpentUSD != 0 {
		t.Errorf("October spent = %v, want 0", s.SpentUSD)
	}
	// The gauge is the clock's month, whatever month was read last.
	if v := h.value("luma_budget_spent_usd", nil); !near(v, 0.5) {
		t.Errorf("luma_budget_spent_usd = %v in January, want 0.5", v)
	}
}

// Task 14 review: a turn's rows are written once, all or none.
func TestRedeliveredTurnIsCountedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_r", "interactive", haiku)
	if v := h.value("luma_budget_spent_usd", nil); v != 0 {
		t.Fatalf("luma_budget_spent_usd = %v before any spend", v)
	}
	u := protocol.TurnUsage{CostUSD: 0.03, Models: map[string]protocol.ModelUsage{
		haiku: {Input: 10, CostUSD: 0.01}, opus: {Input: 5, CostUSD: 0.02}}} // opus is unbound
	for range 2 {
		if err := h.l.RecordTurn(bg, sess, "trn_r", u, nil); err != nil {
			t.Fatal(err)
		}
	}
	if rows := h.rows(); len(rows) != 2 {
		t.Errorf("%d rows after delivering a turn twice, want 2 (one per model)", len(rows))
	}
	if v := h.value("luma_cost_usd_total", costLabels(haiku)); !near(v, 0.01) {
		t.Errorf("haiku cost = %v, want 0.01 (counted once)", v)
	}
	if v := h.value("luma_tokens_total", tokenLabels(haiku, "input")); v != 10 {
		t.Errorf("haiku input = %v, want 10", v)
	}
	if v := h.value("luma_unbound_model_requests_total", map[string]string{"model": opus}); v != 1 {
		t.Errorf("unbound{%s} = %v, want 1", opus, v)
	}
	// The month's figure, read before the turn, moved with it.
	if v := h.value("luma_budget_spent_usd", nil); !near(v, 0.03) {
		t.Errorf("luma_budget_spent_usd = %v, want 0.03", v)
	}
}

func TestTurnRowsAreAllOrNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_f", "interactive", haiku, sonnet)
	// The second row (sonnet, after haiku by name) fails inside the database.
	conn := storetest.ConnAs(t, h.st, storetest.Superuser)
	if _, err := conn.Exec(bg, `create function fail_sonnet() returns trigger language plpgsql as $$
		begin if new.model = 'claude-sonnet-5-5' then raise exception 'injected'; end if; return new; end $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(bg, `create trigger fail_sonnet before insert on cost_ledger
		for each row execute function fail_sonnet()`); err != nil {
		t.Fatal(err)
	}
	u := protocol.TurnUsage{CostUSD: 0.03, Models: map[string]protocol.ModelUsage{
		haiku: {Input: 10, CostUSD: 0.01}, sonnet: {Input: 5, CostUSD: 0.02}}}
	if err := h.l.RecordTurn(bg, sess, "trn_f", u, nil); err == nil {
		t.Fatal("RecordTurn succeeded with a row the database refused")
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Errorf("%d rows after a failure midway, want 0", len(rows))
	}
	if n := h.series("luma_cost_usd_total"); n != 0 {
		t.Errorf("a failed turn made %d cost series", n)
	}
	// Once the database takes it, the same turn is ledgered whole.
	if _, err := conn.Exec(bg, `drop trigger fail_sonnet on cost_ledger`); err != nil {
		t.Fatal(err)
	}
	if err := h.l.RecordTurn(bg, sess, "trn_f", u, nil); err != nil {
		t.Fatal(err)
	}
	if rows := h.rows(); len(rows) != 2 {
		t.Errorf("%d rows after the retry, want 2", len(rows))
	}
}

// Not parallel: it replaces the default logger.
func TestCostMismatchWarns(t *testing.T) {
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_w", "interactive", haiku)
	var buf syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	matching := protocol.TurnUsage{CostUSD: 0.0300000001, Models: map[string]protocol.ModelUsage{
		haiku: {Input: 10, CostUSD: 0.01}, sonnet: {Input: 1, CostUSD: 0.02}}}
	if err := h.l.RecordTurn(bg, sess, "trn_w1", matching, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "differs from the sum") {
		t.Errorf("a cost within 1e-6 of its models' warned: %s", buf.String())
	}
	off := protocol.TurnUsage{CostUSD: 0.05, Models: map[string]protocol.ModelUsage{haiku: {Input: 10, CostUSD: 0.04}}}
	if err := h.l.RecordTurn(bg, sess, "trn_w2", off, nil); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{"level=WARN", "differs from the sum", "turn=trn_w2", "cost_usd=0.05", "models_cost_usd=0.04"} {
		if !strings.Contains(line, want) {
			t.Errorf("warning %q lacks %q", line, want)
		}
	}
	// The rows carry the models' figures.
	var got float64
	for _, r := range h.rows() {
		if r.TurnID == "trn_w2" {
			got += r.CostUSD
		}
	}
	if !near(got, 0.04) {
		t.Errorf("trn_w2 rows = $%v, want $0.04", got)
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestNegativeDeltaGoesToAnomalies(t *testing.T) {
	t.Parallel()
	cases := map[string]protocol.TurnUsage{
		"negative turn cost": {CostUSD: -0.5, Models: map[string]protocol.ModelUsage{haiku: {Input: 10, CostUSD: 0.01}}},
		"negative tokens":    {CostUSD: 0.01, Models: map[string]protocol.ModelUsage{haiku: {Input: 10, CacheRead: -1000, CostUSD: 0.01}}},
		"negative model cost": {CostUSD: 0.01, Models: map[string]protocol.ModelUsage{
			haiku: {Input: 10, CostUSD: 0.02}, sonnet: {Output: 3, CostUSD: -0.01}}},
		"negative web searches": {CostUSD: 0.01, Models: map[string]protocol.ModelUsage{haiku: {WebSearchRequests: -1, CostUSD: 0.01}}},
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, ledger.Config{})
			sess := h.session("ses_n", "interactive", haiku, sonnet)
			if err := h.l.RecordTurn(bg, sess, "trn_n", u, ptr("list")); err != nil {
				t.Fatal(err)
			}
			if rows := h.rows(); len(rows) != 0 {
				t.Errorf("anomalous usage made %d ledger rows", len(rows))
			}
			as := h.anomalies()
			if len(as) != 1 {
				t.Fatalf("%d anomalies, want 1", len(as))
			}
			a := as[0]
			if a.SessionID == nil || *a.SessionID != "ses_n" || a.TurnID != "trn_n" || a.Reason != "negative" {
				t.Errorf("anomaly = %+v", a)
			}
			usage, _ := a.Payload["usage"].(map[string]any)
			if usage == nil || usage["cost_usd"] != u.CostUSD {
				t.Errorf("payload usage = %v, want the turn's usage", a.Payload["usage"])
			}
			if v := h.value("luma_usage_anomalies_total", map[string]string{"reason": "negative"}); v != 1 {
				t.Errorf("luma_usage_anomalies_total{negative} = %v, want 1", v)
			}
			if n := h.series("luma_cost_usd_total"); n != 0 {
				t.Errorf("an anomaly made %d cost series", n)
			}
			if v := h.value("luma_budget_spent_usd", nil); v != 0 {
				t.Errorf("budget spent = %v after an anomaly", v)
			}
		})
	}
}

func TestOversizedDeltaGoesToAnomalies(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{MaxTurnCostUSD: 5})
	sess := h.session("ses_o", "interactive", haiku)

	over := protocol.TurnUsage{CostUSD: 5.01, Models: map[string]protocol.ModelUsage{haiku: {Input: 10, CostUSD: 5.01}}}
	if err := h.l.RecordTurn(bg, sess, "trn_over", over, nil); err != nil {
		t.Fatal(err)
	}
	// The per-model dollars are what the rows would sum to, so they are
	// bounded too.
	modelsOver := protocol.TurnUsage{CostUSD: 1, Models: map[string]protocol.ModelUsage{haiku: {Input: 10, CostUSD: 6}}}
	if err := h.l.RecordTurn(bg, sess, "trn_models", modelsOver, nil); err != nil {
		t.Fatal(err)
	}
	// Exactly the cap is not oversized.
	at := protocol.TurnUsage{CostUSD: 5, Models: map[string]protocol.ModelUsage{haiku: {Input: 10, CostUSD: 5}}}
	if err := h.l.RecordTurn(bg, sess, "trn_at", at, nil); err != nil {
		t.Fatal(err)
	}

	rows := h.rows()
	if len(rows) != 1 || rows[0].TurnID != "trn_at" || !near(rows[0].CostUSD, 5) {
		t.Errorf("rows = %+v, want only trn_at's $5", rows)
	}
	as := h.anomalies()
	if len(as) != 2 || as[0].TurnID != "trn_over" || as[1].TurnID != "trn_models" ||
		as[0].Reason != "oversized" || as[1].Reason != "oversized" {
		t.Errorf("anomalies = %+v, want trn_over and trn_models oversized", as)
	}
	if v := h.value("luma_usage_anomalies_total", map[string]string{"reason": "oversized"}); v != 2 {
		t.Errorf("luma_usage_anomalies_total{oversized} = %v, want 2", v)
	}
	if v := h.value("luma_budget_spent_usd", nil); !near(v, 5) {
		t.Errorf("budget spent = %v, want 5", v)
	}
}

func TestUnboundModelCounted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_u", "interactive", haiku)
	u := protocol.TurnUsage{CostUSD: 0.06, Models: map[string]protocol.ModelUsage{
		haiku:              {Input: 10, CostUSD: 0.01},
		opus:               {Input: 20, CostUSD: 0.03}, // in models.yaml, not bound
		"claude-mystery-9": {Input: 30, CostUSD: 0.02}, // not in models.yaml
	}}
	if err := h.l.RecordTurn(bg, sess, "trn_u", u, nil); err != nil {
		t.Fatal(err)
	}
	// Every row is still recorded: it is real spend (P29).
	rows := h.rows()
	var models []string
	for _, r := range rows {
		models = append(models, r.Model)
	}
	if want := []string{haiku, opus, "claude-mystery-9"}; len(models) != 3 ||
		!contains(models, want[0]) || !contains(models, want[1]) || !contains(models, want[2]) {
		t.Errorf("rows for %v, want %v", models, want)
	}
	if v := h.value("luma_unbound_model_requests_total", map[string]string{"model": opus}); v != 1 {
		t.Errorf("unbound{%s} = %v, want 1", opus, v)
	}
	if v := h.value("luma_unbound_model_requests_total", map[string]string{"model": "other"}); v != 1 {
		t.Errorf("unbound{other} = %v, want 1", v)
	}
	if _, ok := h.metric("luma_unbound_model_requests_total", map[string]string{"model": haiku}); ok {
		t.Error("the bound model was counted as unbound")
	}
	// A model outside models.yaml is labelled other everywhere.
	if v := h.value("luma_cost_usd_total", costLabels("other")); !near(v, 0.02) {
		t.Errorf("cost{model=other} = %v, want 0.02", v)
	}
	if _, ok := h.metric("luma_cost_usd_total", costLabels("claude-mystery-9")); ok {
		t.Error("a model outside models.yaml became a label value")
	}

	// A model.switched target the session accepted is bound (P29).
	sess.BoundModels = append(sess.BoundModels, opus)
	if err := h.l.RecordTurn(bg, sess, "trn_u2", protocol.TurnUsage{CostUSD: 0.03,
		Models: map[string]protocol.ModelUsage{opus: {Input: 20, CostUSD: 0.03}}}, nil); err != nil {
		t.Fatal(err)
	}
	if v := h.value("luma_unbound_model_requests_total", map[string]string{"model": opus}); v != 1 {
		t.Errorf("unbound{%s} = %v after the switch was bound, want still 1", opus, v)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestCostBasisUnexpected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_c", "interactive", haiku)
	u := protocol.TurnUsage{CostUSD: 0.01, Models: map[string]protocol.ModelUsage{haiku: {Input: 1, CostUSD: 0.01}}}
	for i, tc := range []struct {
		basis *string
		want  float64
	}{
		{nil, 0}, {ptr("list"), 0}, {ptr("managed"), 1}, {ptr("unknown"), 2}, {ptr(""), 3},
	} {
		if err := h.l.RecordTurn(bg, sess, fmt.Sprintf("trn_c%d", i), u, tc.basis); err != nil {
			t.Fatal(err)
		}
		if v := h.value("luma_cost_basis_unexpected_total", nil); v != tc.want {
			t.Errorf("after case %d: luma_cost_basis_unexpected_total = %v, want %v", i, v, tc.want)
		}
	}
	// The basis is a signal only: every turn was still ledgered.
	if n := len(h.rows()); n != 5 {
		t.Errorf("%d rows, want 5", n)
	}
}

func TestLedgerSurvivesTreeDelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_d", "interactive", haiku)
	if err := h.l.RecordTurn(bg, sess, "trn_d", protocol.TurnUsage{CostUSD: 0.25,
		Models: map[string]protocol.ModelUsage{haiku: {Input: 10, CostUSD: 0.25}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.l.RecordTurn(bg, sess, "trn_bad", protocol.TurnUsage{CostUSD: -1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DeleteTree(bg, "chris", "ses_d"); err != nil {
		t.Fatal(err)
	}
	rows := h.rows()
	if len(rows) != 1 || rows[0].SessionID != nil || rows[0].TurnID != "trn_d" || !near(rows[0].CostUSD, 0.25) {
		t.Errorf("rows after delete = %+v, want trn_d's row with a null session", rows)
	}
	as := h.anomalies()
	if len(as) != 1 || as[0].SessionID != nil || as[0].TurnID != "trn_bad" {
		t.Errorf("anomalies after delete = %+v, want trn_bad's with a null session", as)
	}
	// The spend still counts toward the month.
	if s := h.l.Budget().Status(bg, testNow); !near(s.SpentUSD, 0.25) {
		t.Errorf("spent after delete = %v, want 0.25", s.SpentUSD)
	}
}

// ---- the SLO (P10, S28) ----

func mainLabels(typ, slo string) map[string]string {
	return map[string]string{"agent": "luma", "tier": "fast", "source": "web", "type": typ, "slo": slo}
}

func TestSLOExclusionsLabelled(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_s", "interactive", haiku)
	for _, u := range []protocol.MainRequestUsage{
		{APIMessageID: "msg_1", Model: haiku, SLOExclusion: "first", Input: 1000, CacheRead: 0, CacheCreate: 9000},
		{APIMessageID: "msg_2", Model: haiku, SLOExclusion: "none", Input: 100, CacheRead: 9000, CacheCreate: 50},
		{APIMessageID: "msg_3", Model: haiku, SLOExclusion: "cold_resume", Input: 2000, CacheRead: 0, CacheCreate: 8000},
		{APIMessageID: "msg_4", Model: haiku, SLOExclusion: "post_compaction", Input: 3000, CacheRead: 10, CacheCreate: 7000},
		{APIMessageID: "msg_5", Model: haiku, SLOExclusion: "none", Input: 20, CacheRead: 9500, CacheCreate: 0},
	} {
		h.l.RecordMainRequest(sess, u)
	}
	for _, c := range []struct {
		typ, slo string
		want     float64
	}{
		{"input", "included", 120}, {"cache_read", "included", 18500}, {"cache_creation", "included", 50},
		{"input", "excluded", 6000}, {"cache_read", "excluded", 10}, {"cache_creation", "excluded", 24000},
	} {
		if v := h.value("luma_main_request_tokens_total", mainLabels(c.typ, c.slo)); v != c.want {
			t.Errorf("main tokens {type=%s, slo=%s} = %v, want %v", c.typ, c.slo, v, c.want)
		}
	}
	for _, reason := range []string{"first", "cold_resume", "post_compaction"} {
		if v := h.value("luma_main_request_exclusions_total",
			map[string]string{"agent": "luma", "tier": "fast", "reason": reason}); v != 1 {
			t.Errorf("exclusions{%s} = %v, want 1", reason, v)
		}
	}
	if n := h.series("luma_main_request_exclusions_total"); n != 3 {
		t.Errorf("%d exclusion series, want 3 (none is not an exclusion)", n)
	}

	// An exclusion outside the contract's set is still excluded, as other.
	h.l.RecordMainRequest(sess, protocol.MainRequestUsage{APIMessageID: "msg_6", Model: haiku,
		SLOExclusion: "surprise", Input: 7, CacheRead: 0, CacheCreate: 0})
	if v := h.value("luma_main_request_exclusions_total",
		map[string]string{"agent": "luma", "tier": "fast", "reason": "other"}); v != 1 {
		t.Errorf("exclusions{other} = %v, want 1", v)
	}
	if v := h.value("luma_main_request_tokens_total", mainLabels("input", "excluded")); v != 6007 {
		t.Errorf("excluded input = %v, want 6007", v)
	}
}

func TestOver100KCounted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_k", "interactive", haiku)
	// The prompt is input + cache read + cache creation (the price step is
	// on the whole prompt).
	h.l.RecordMainRequest(sess, protocol.MainRequestUsage{APIMessageID: "m1", Model: haiku, SLOExclusion: "none",
		Input: 1000, CacheRead: 99000, CacheCreate: 0}) // exactly 100K: not over
	if v := h.value("luma_main_requests_over_100k_total", map[string]string{"tier": "fast"}); v != 0 {
		t.Errorf("over_100k = %v after a 100,000-token prompt, want 0", v)
	}
	h.l.RecordMainRequest(sess, protocol.MainRequestUsage{APIMessageID: "m2", Model: haiku, SLOExclusion: "none",
		Input: 1000, CacheRead: 99000, CacheCreate: 1})
	h.l.RecordMainRequest(sess, protocol.MainRequestUsage{APIMessageID: "m3", Model: haiku, SLOExclusion: "post_compaction",
		Input: 150000, CacheRead: 0, CacheCreate: 0}) // excluded from the SLO, still over
	if v := h.value("luma_main_requests_over_100k_total", map[string]string{"tier": "fast"}); v != 2 {
		t.Errorf("over_100k = %v, want 2", v)
	}
}

func TestSLOInputIsUncached(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_i", "interactive", haiku)
	h.l.RecordMainRequest(sess, protocol.MainRequestUsage{APIMessageID: "m1", Model: haiku, SLOExclusion: "none",
		Input: 200, CacheRead: 800, CacheCreate: 0})
	in, okIn := h.metric("luma_main_request_tokens_total", mainLabels("input", "included"))
	cr, okCR := h.metric("luma_main_request_tokens_total", mainLabels("cache_read", "included"))
	cc, okCC := h.metric("luma_main_request_tokens_total", mainLabels("cache_creation", "included"))
	if !okIn || !okCR || !okCC {
		t.Fatalf("series present: input %v, cache_read %v, cache_creation %v; want all three", okIn, okCR, okCC)
	}
	if in != 200 || cr != 800 || cc != 0 {
		t.Errorf("input/cache_read/cache_creation = %v/%v/%v, want 200/800/0", in, cr, cc)
	}
	if share := cr / (in + cr + cc); share != 0.8 {
		t.Errorf("share = %v, want 0.8", share)
	}
}

func TestMainRequestWithNegativeTokensIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	sess := h.session("ses_m", "interactive", haiku)
	// A counter cannot go down (Add panics), so a broken frame is dropped.
	h.l.RecordMainRequest(sess, protocol.MainRequestUsage{APIMessageID: "m1", Model: haiku, SLOExclusion: "none",
		Input: -5, CacheRead: 800, CacheCreate: 0})
	if n := h.series("luma_main_request_tokens_total"); n != 0 {
		t.Errorf("a negative frame made %d series", n)
	}
}
