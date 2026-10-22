package metrics_test

import (
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/luma/core/internal/metrics"
)

// c7Core is the Core list of contract C7, typed by hand: metric name, then its
// label names in order, and the Core field that carries it.
var c7Core = []struct {
	name   string
	labels []string
	field  string
}{
	{"luma_cost_usd_total", []string{"person", "agent", "tier", "source", "model"}, "CostUSDTotal"},
	{"luma_tokens_total", []string{"person", "agent", "tier", "source", "model", "type"}, "TokensTotal"},
	{"luma_web_search_requests_total", []string{"person", "agent", "tier", "source"}, "WebSearchRequestsTotal"},
	{"luma_main_request_tokens_total", []string{"agent", "tier", "source", "type", "slo"}, "MainRequestTokensTotal"},
	{"luma_main_request_exclusions_total", []string{"agent", "tier", "reason"}, "MainRequestExclusionsTotal"},
	{"luma_main_requests_over_100k_total", []string{"tier"}, "MainRequestsOver100kTotal"},
	{"luma_budget_spent_usd", nil, "BudgetSpentUSD"},
	{"luma_budget_cap_usd", nil, "BudgetCapUSD"},
	{"luma_usage_reconcile_drift_ratio", []string{"workspace"}, "UsageReconcileDriftRatio"},
	{"luma_usage_reconcile_drift_usd", []string{"workspace"}, "UsageReconcileDriftUSD"},
	{"luma_usage_reconcile_last_success_timestamp_seconds", nil, "UsageReconcileLastSuccessTimestampSeconds"},
	{"luma_usage_anomalies_total", []string{"reason"}, "UsageAnomaliesTotal"},
	{"luma_unbound_model_requests_total", []string{"model"}, "UnboundModelRequestsTotal"},
	{"luma_cost_basis_unexpected_total", nil, "CostBasisUnexpectedTotal"},
	{"luma_turns_total", []string{"agent", "tier", "source", "outcome"}, "TurnsTotal"},
	{"luma_turn_duration_seconds", []string{"agent", "tier"}, "TurnDurationSeconds"},
	{"luma_conductor_connected", nil, "ConductorConnected"},
	{"luma_open_hosts", nil, "OpenHosts"},
	{"luma_link_rejected_total", []string{"reason"}, "LinkRejectedTotal"},
	{"luma_thinking_binding_errors_total", nil, "ThinkingBindingErrorsTotal"},
	{"luma_memory_writes_total", []string{"op", "status"}, "MemoryWritesTotal"},
	{"luma_agents_reload_total", []string{"result"}, "AgentsReloadTotal"},
	{"luma_eval_last_run_timestamp_seconds", []string{"suite"}, "EvalLastRunTimestampSeconds"},
	{"luma_eval_last_run_success", []string{"suite"}, "EvalLastRunSuccess"},
	{"luma_eval_cost_usd_total", []string{"suite"}, "EvalCostUSDTotal"},
	{"luma_maintenance_enabled", nil, "MaintenanceEnabled"},
}

func TestEveryC7CoreMetricRegistered(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	core := metrics.NewCore(reg)

	// Give every vector one child so Gather reports it, with as many label
	// values as the C7 table lists (a wrong label count panics, failing the test).
	cv := reflect.ValueOf(core).Elem()
	for _, m := range c7Core {
		f := cv.FieldByName(m.field)
		if !f.IsValid() {
			t.Errorf("Core has no field %s for %s", m.field, m.name)
			continue
		}
		vals := make([]string, len(m.labels))
		for i := range vals {
			vals[i] = "v" + strconv.Itoa(i) // a distinct value per position
		}
		switch c := f.Interface().(type) {
		case *prometheus.CounterVec:
			c.WithLabelValues(vals...).Add(0)
		case *prometheus.GaugeVec:
			c.WithLabelValues(vals...).Set(0)
		case *prometheus.HistogramVec:
			c.WithLabelValues(vals...).Observe(0)
		case prometheus.Gauge, prometheus.Counter:
			if len(m.labels) != 0 {
				t.Errorf("%s has labels in C7 but Core.%s is a plain metric", m.name, m.field)
			}
		default:
			t.Errorf("Core.%s has unexpected type %T", m.field, c)
		}
	}
	if got, want := cv.NumField(), len(c7Core); got != want {
		t.Errorf("Core has %d fields, C7 lists %d metrics", got, want)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	byLabel := map[string]string{} // metric/label -> the value it was given
	for _, mf := range mfs {
		var labels []string
		for _, l := range mf.GetMetric()[0].GetLabel() {
			labels = append(labels, l.GetName())
			byLabel[mf.GetName()+"/"+l.GetName()] = l.GetValue()
		}
		got[mf.GetName()] = labels
	}
	want := map[string][]string{}
	for _, m := range c7Core {
		want[m.name] = m.labels
	}
	// Label order: the value given at position i must land on C7's i-th label.
	for _, m := range c7Core {
		for i, l := range m.labels {
			if v := byLabel[m.name+"/"+l]; v != "v"+strconv.Itoa(i) {
				t.Errorf("%s: label %s is at the wrong position (got value %q, want v%d)", m.name, l, v, i)
			}
		}
	}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, ok := want[n]
		if !ok {
			t.Errorf("registered metric %s is not in C7", n)
			continue
		}
		g := append([]string(nil), got[n]...)
		sort.Strings(g)
		w = append([]string(nil), w...)
		sort.Strings(w)
		if !reflect.DeepEqual(g, w) && (len(g) != 0 || len(w) != 0) {
			t.Errorf("%s labels = %v, C7 says %v", n, g, w)
		}
		delete(want, n)
	}
	for n := range want {
		t.Errorf("C7 metric %s is not registered", n)
	}
	for _, n := range names {
		for _, banned := range []string{"session", "session_id"} {
			for _, l := range got[n] {
				if l == banned {
					t.Errorf("%s has banned label %s", n, banned)
				}
			}
		}
	}
}

func TestLabelMapsUnknownToOther(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"applied", "applied"}, {"pending", "pending"}, {"surprise", "other"}, {"", "other"},
	} {
		if got := metrics.Label(tt.in, "applied", "pending", "rejected"); got != tt.want {
			t.Errorf("Label(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := metrics.Label("x"); got != "other" {
		t.Errorf("Label with no allowed values = %q", got)
	}
}
