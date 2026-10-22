package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/ledger"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/store"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

var bg = context.Background()

// testNow is mid-October 2026, UTC.
var testNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

const (
	haiku  = "claude-haiku-5-5"
	sonnet = "claude-sonnet-5-5"
	opus   = "claude-opus-5-5"
)

type harness struct {
	t   *testing.T
	st  *store.Store
	reg *prometheus.Registry
	m   *metrics.Core
	l   *ledger.Ledger

	mu    sync.Mutex
	now   time.Time
	seeds int // spend's turn ids
}

// newHarness builds a Ledger over a fresh Postgres, the repo's agents/
// catalog and a fixed clock. cfg's zero fields keep their defaults, and its
// Clock is the harness's.
func newHarness(t *testing.T, cfg ledger.Config) *harness {
	t.Helper()
	st := storetest.New(t)
	h := &harness{t: t, st: st, now: testNow}
	h.l = h.ledger(cfg)
	return h
}

// ledger builds another Ledger over the harness's store, with a fresh
// registry: a second one stands for a restarted Core.
func (h *harness) ledger(cfg ledger.Config) *ledger.Ledger {
	h.t.Helper()
	cat, err := agents.LoadCatalog("../../../agents")
	if err != nil {
		h.t.Fatal(err)
	}
	h.reg = prometheus.NewPedanticRegistry()
	h.m = metrics.NewCore(h.reg)
	cfg.Clock = h.clock
	return ledger.New(h.st, h.m, agents.NewLive(cat), cfg)
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) setNow(t time.Time) {
	h.mu.Lock()
	h.now = t
	h.mu.Unlock()
}

// session stores a fast-tier root session of chris's on the luma agent, bound
// to the given models.
func (h *harness) session(id, apiWorkspace string, bound ...string) store.Session {
	h.t.Helper()
	s, err := h.st.CreateSession(bg, store.Session{
		ID: id, PersonID: "chris", AgentID: "luma", AgentVersion: "abc123",
		Tier: "fast", Model: haiku, HelperModel: haiku, Effort: "low", CacheTTL: "1h",
		APIWorkspace: apiWorkspace, Source: "web", BoundModels: bound,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// spend stores a ledger row of usd at the given time, outside any session.
// Each seed has its own turn id: (turn_id, model) is unique (migration 0003).
func (h *harness) spend(at time.Time, usd float64, apiWorkspace string) {
	h.t.Helper()
	h.mu.Lock()
	h.seeds++
	turn := fmt.Sprintf("t_seed_%d", h.seeds)
	h.mu.Unlock()
	if _, err := h.st.RecordLedger(bg, store.LedgerRow{PersonID: "chris", AgentID: "luma", Tier: "fast",
		Source: "web", APIWorkspace: apiWorkspace, Model: haiku, TurnID: turn, CostUSD: usd, CreatedAt: at}); err != nil {
		h.t.Fatal(err)
	}
}

// ledgerRow is a cost_ledger row as the database holds it.
type ledgerRow struct {
	SessionID                                   *string
	PersonID, AgentID, Tier, Source, Workspace  string
	Model, TurnID                               string
	Input, Output, CacheRead, CacheCreation, WS int64
	CostUSD                                     float64
}

func (h *harness) rows() []ledgerRow {
	h.t.Helper()
	conn := storetest.ConnAs(h.t, h.st, storetest.Superuser)
	rs, err := conn.Query(bg, `select session_id, person_id, agent_id, tier, source, api_workspace, model, turn_id,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, web_search_requests, cost_usd::float8
		from cost_ledger where turn_id not like 't_seed_%' order by model, id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rs.Close()
	var out []ledgerRow
	for rs.Next() {
		var r ledgerRow
		if err := rs.Scan(&r.SessionID, &r.PersonID, &r.AgentID, &r.Tier, &r.Source, &r.Workspace, &r.Model, &r.TurnID,
			&r.Input, &r.Output, &r.CacheRead, &r.CacheCreation, &r.WS, &r.CostUSD); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// anomaly is a usage_anomalies row as the database holds it.
type anomaly struct {
	SessionID *string
	TurnID    string
	Reason    string
	Payload   map[string]any
}

func (h *harness) anomalies() []anomaly {
	h.t.Helper()
	conn := storetest.ConnAs(h.t, h.st, storetest.Superuser)
	rs, err := conn.Query(bg, `select session_id, turn_id, reason, payload from usage_anomalies order by id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rs.Close()
	var out []anomaly
	for rs.Next() {
		var a anomaly
		var payload []byte
		if err := rs.Scan(&a.SessionID, &a.TurnID, &a.Reason, &payload); err != nil {
			h.t.Fatal(err)
		}
		if err := json.Unmarshal(payload, &a.Payload); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rs.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// metric returns the value of the series of name with exactly labels, and
// whether that series exists.
func (h *harness) metric(name string, labels map[string]string) (float64, bool) {
	h.t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			if len(got) != len(labels) || (len(labels) != 0 && !reflect.DeepEqual(got, labels)) {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// value is metric's value, 0 when the series does not exist.
func (h *harness) value(name string, labels map[string]string) float64 {
	h.t.Helper()
	v, _ := h.metric(name, labels)
	return v
}

// series counts the series of name.
func (h *harness) series(name string) int {
	h.t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return len(mf.GetMetric())
		}
	}
	return 0
}

func ptr[T any](v T) *T { return &v }

// near compares dollars stored as numeric(12,6).
func near(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
