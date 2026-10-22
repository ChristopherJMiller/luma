package metrics_test

import (
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	{"luma_late_turn_results_total", nil, "LateTurnResultsTotal"},
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
		case prometheus.Gauge, prometheus.Counter, *metrics.FuncGauge:
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

func TestFuncGaugeReadsItsFunctionAtScrape(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	core := metrics.NewCore(reg)
	read := func() float64 {
		t.Helper()
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range mfs {
			if mf.GetName() == "luma_budget_spent_usd" {
				return mf.GetMetric()[0].GetGauge().GetValue()
			}
		}
		t.Fatal("luma_budget_spent_usd not gathered")
		return 0
	}
	if v := read(); v != 0 {
		t.Errorf("before SetFunc: %v, want 0", v)
	}
	v := 12.5
	core.BudgetSpentUSD.SetFunc(func() float64 { return v })
	if got := read(); got != 12.5 {
		t.Errorf("after SetFunc: %v, want 12.5", got)
	}
	v = 13
	if got := read(); got != 13 {
		t.Errorf("a later scrape: %v, want 13 (read at each scrape)", got)
	}
}

// recordingRegisterer keeps every collector registered through it, so a test
// can walk their descriptors.
type recordingRegisterer struct {
	prometheus.Registerer
	collectors []prometheus.Collector
}

func (r *recordingRegisterer) Register(c prometheus.Collector) error {
	r.collectors = append(r.collectors, c)
	return r.Registerer.Register(c)
}

func (r *recordingRegisterer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}

// descRE reads a descriptor's name and its constant and variable label lists
// from Desc.String(), the only public view of them.
var descRE = regexp.MustCompile(`^Desc\{fqName: "([^"]*)", .*constLabels: \{(.*)\}, variableLabels: \{([^{}]*)\}\}$`)

// constLabelRE finds each constant label's name in constLabels' name="value"
// list.
var constLabelRE = regexp.MustCompile(`(?:^|,)([a-zA-Z_][a-zA-Z0-9_]*)="`)

// descLabels returns a descriptor's metric name and every label name it
// carries, constant then variable. A descriptor it cannot read fails the
// test, so a change in client_golang's format cannot pass the audit
// vacuously.
func descLabels(t *testing.T, d *prometheus.Desc) (string, []string) {
	t.Helper()
	m := descRE.FindStringSubmatch(d.String())
	if m == nil {
		t.Fatalf("cannot read the labels of %s", d)
	}
	var labels []string
	for _, c := range constLabelRE.FindAllStringSubmatch(m[2], -1) {
		labels = append(labels, c[1])
	}
	if m[3] != "" {
		for _, v := range strings.Split(m[3], ",") {
			labels = append(labels, strings.TrimSuffix(strings.TrimPrefix(v, "c("), ")"))
		}
	}
	return m[1], labels
}

// sessionLabel is the C7 ban: a label that names a session (session,
// session_id, or any other spelling of one).
func sessionLabel(l string) bool { return strings.Contains(l, "session") }

// TestNoSessionLabels walks every descriptor NewCore registers and fails on
// a label naming a session: session ids never become labels (C7, Task 14).
func TestNoSessionLabels(t *testing.T) {
	rec := &recordingRegisterer{Registerer: prometheus.NewPedanticRegistry()}
	metrics.NewCore(rec)

	ch := make(chan *prometheus.Desc)
	go func() {
		for _, c := range rec.collectors {
			c.Describe(ch)
		}
		close(ch)
	}()
	var descs []*prometheus.Desc
	for d := range ch {
		descs = append(descs, d)
	}
	if len(descs) != len(c7Core) {
		t.Errorf("walked %d descriptors, C7 lists %d metrics", len(descs), len(c7Core))
	}
	for _, d := range descs {
		name, labels := descLabels(t, d)
		for _, l := range labels {
			if sessionLabel(l) {
				t.Errorf("%s has label %q: session ids never become labels (C7)", name, l)
			}
		}
	}

	// The audit catches what it bans, as a variable or a constant label.
	for _, d := range []*prometheus.Desc{
		prometheus.NewDesc("x_total", "x", []string{"tier", "session"}, nil),
		prometheus.NewDesc("x_total", "x", []string{"session_id"}, nil),
		prometheus.NewDesc("x_total", "x", nil, prometheus.Labels{"session_id": "ses_1", "tier": "fast"}),
		prometheus.NewDesc("x_total", "x", []string{"tier"}, prometheus.Labels{"sdk_session": "s"}),
	} {
		_, labels := descLabels(t, d)
		caught := false
		for _, l := range labels {
			caught = caught || sessionLabel(l)
		}
		if !caught {
			t.Errorf("the audit missed the session label of %s (read %v)", d, labels)
		}
	}
	// And reads an innocent descriptor's labels exactly.
	_, labels := descLabels(t, prometheus.NewDesc("y_total", "y", []string{"agent", "tier"}, prometheus.Labels{"k": "v"}))
	if want := []string{"k", "agent", "tier"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("descLabels read %v, want %v", labels, want)
	}
}
