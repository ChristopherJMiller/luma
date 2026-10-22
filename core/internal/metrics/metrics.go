// Package metrics holds Core's Prometheus descriptors: every Core metric of
// contract C7, created in one place so the packages that update them share a
// single registry (S3). Session ids never become labels; a label value from
// outside a fixed set maps to "other" (Label). Task 14 adds the label audit.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Core is the C7 Core metric set. Each field is named in CamelCase after its
// metric, without the luma_ prefix.
type Core struct {
	// Cost and tokens.
	CostUSDTotal           *prometheus.CounterVec // luma_cost_usd_total{person,agent,tier,source,model}
	TokensTotal            *prometheus.CounterVec // luma_tokens_total{person,agent,tier,source,model,type}
	WebSearchRequestsTotal *prometheus.CounterVec // luma_web_search_requests_total{person,agent,tier,source}

	// SLO input.
	MainRequestTokensTotal     *prometheus.CounterVec // luma_main_request_tokens_total{agent,tier,source,type,slo}
	MainRequestExclusionsTotal *prometheus.CounterVec // luma_main_request_exclusions_total{agent,tier,reason}
	MainRequestsOver100kTotal  *prometheus.CounterVec // luma_main_requests_over_100k_total{tier}

	// Budget and reconcile.
	BudgetSpentUSD                            prometheus.Gauge     // luma_budget_spent_usd
	BudgetCapUSD                              prometheus.Gauge     // luma_budget_cap_usd
	UsageReconcileDriftRatio                  *prometheus.GaugeVec // luma_usage_reconcile_drift_ratio{workspace}
	UsageReconcileDriftUSD                    *prometheus.GaugeVec // luma_usage_reconcile_drift_usd{workspace}
	UsageReconcileLastSuccessTimestampSeconds prometheus.Gauge     // luma_usage_reconcile_last_success_timestamp_seconds

	// Anomalies.
	UsageAnomaliesTotal       *prometheus.CounterVec // luma_usage_anomalies_total{reason}
	UnboundModelRequestsTotal *prometheus.CounterVec // luma_unbound_model_requests_total{model}
	CostBasisUnexpectedTotal  prometheus.Counter     // luma_cost_basis_unexpected_total

	// Turns.
	TurnsTotal          *prometheus.CounterVec   // luma_turns_total{agent,tier,source,outcome}
	TurnDurationSeconds *prometheus.HistogramVec // luma_turn_duration_seconds{agent,tier}

	// Conductor link.
	ConductorConnected prometheus.Gauge       // luma_conductor_connected
	OpenHosts          prometheus.Gauge       // luma_open_hosts
	LinkRejectedTotal  *prometheus.CounterVec // luma_link_rejected_total{reason}

	// Errors and memory.
	ThinkingBindingErrorsTotal prometheus.Counter     // luma_thinking_binding_errors_total
	MemoryWritesTotal          *prometheus.CounterVec // luma_memory_writes_total{op,status}
	AgentsReloadTotal          *prometheus.CounterVec // luma_agents_reload_total{result=ok|error}

	// Evals.
	EvalLastRunTimestampSeconds *prometheus.GaugeVec   // luma_eval_last_run_timestamp_seconds{suite}
	EvalLastRunSuccess          *prometheus.GaugeVec   // luma_eval_last_run_success{suite}
	EvalCostUSDTotal            *prometheus.CounterVec // luma_eval_cost_usd_total{suite}

	// Maintenance.
	MaintenanceEnabled prometheus.Gauge // luma_maintenance_enabled
}

// NewCore creates every C7 Core metric and registers it with reg. It panics
// if a name is already registered, like prometheus.MustRegister.
func NewCore(reg prometheus.Registerer) *Core {
	f := factory{reg}
	return &Core{
		CostUSDTotal: f.counterVec("luma_cost_usd_total", "Dollars spent, from SDK totals as per-turn deltas.",
			"person", "agent", "tier", "source", "model"),
		TokensTotal: f.counterVec("luma_tokens_total", "Tokens used, by type (input, output, cache_read, cache_creation).",
			"person", "agent", "tier", "source", "model", "type"),
		WebSearchRequestsTotal: f.counterVec("luma_web_search_requests_total", "Web searches run.",
			"person", "agent", "tier", "source"),

		MainRequestTokensTotal: f.counterVec("luma_main_request_tokens_total",
			"Tokens of main-class requests; input is uncached, so the three types sum to the whole prompt.",
			"agent", "tier", "source", "type", "slo"),
		MainRequestExclusionsTotal: f.counterVec("luma_main_request_exclusions_total",
			"Main-class requests excluded from the cache SLO.", "agent", "tier", "reason"),
		MainRequestsOver100kTotal: f.counterVec("luma_main_requests_over_100k_total",
			"Main-class requests with more than 100K input tokens.", "tier"),

		BudgetSpentUSD: f.gauge("luma_budget_spent_usd", "Dollars spent this budget month."),
		BudgetCapUSD:   f.gauge("luma_budget_cap_usd", "The monthly budget cap in dollars."),
		UsageReconcileDriftRatio: f.gaugeVec("luma_usage_reconcile_drift_ratio",
			"Relative drift between the Usage and Cost API and the ledger, by workspace.", "workspace"),
		UsageReconcileDriftUSD: f.gaugeVec("luma_usage_reconcile_drift_usd",
			"Absolute drift in dollars between the Usage and Cost API and the ledger, by workspace.", "workspace"),
		UsageReconcileLastSuccessTimestampSeconds: f.gauge("luma_usage_reconcile_last_success_timestamp_seconds",
			"Unix time of the last successful usage reconcile."),

		UsageAnomaliesTotal: f.counterVec("luma_usage_anomalies_total", "Usage anomalies recorded.", "reason"),
		UnboundModelRequestsTotal: f.counterVec("luma_unbound_model_requests_total",
			"Requests to a model outside the session's bound models.", "model"),
		CostBasisUnexpectedTotal: f.counter("luma_cost_basis_unexpected_total",
			"Turn results whose cost basis was not the expected one."),

		TurnsTotal: f.counterVec("luma_turns_total", "Turns finished.", "agent", "tier", "source", "outcome"),
		TurnDurationSeconds: f.histogramVec("luma_turn_duration_seconds", "Turn duration.",
			[]float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}, "agent", "tier"),

		ConductorConnected: f.gauge("luma_conductor_connected", "1 while an authenticated conductor link is up."),
		OpenHosts:          f.gauge("luma_open_hosts", "Session hosts currently open on the conductor."),
		LinkRejectedTotal:  f.counterVec("luma_link_rejected_total", "Conductor link attempts rejected.", "reason"),

		ThinkingBindingErrorsTotal: f.counter("luma_thinking_binding_errors_total",
			"Thinking-block binding errors reported by the conductor."),
		MemoryWritesTotal: f.counterVec("luma_memory_writes_total", "Memory writes by op and status.", "op", "status"),
		AgentsReloadTotal: f.counterVec("luma_agents_reload_total", "Agent catalog reloads by result (ok or error).", "result"),

		EvalLastRunTimestampSeconds: f.gaugeVec("luma_eval_last_run_timestamp_seconds",
			"Unix time of the last eval run, by suite.", "suite"),
		EvalLastRunSuccess: f.gaugeVec("luma_eval_last_run_success",
			"1 if the last eval run of the suite passed.", "suite"),
		EvalCostUSDTotal: f.counterVec("luma_eval_cost_usd_total", "Dollars spent on eval runs, by suite.", "suite"),

		MaintenanceEnabled: f.gauge("luma_maintenance_enabled", "1 while maintenance mode is on."),
	}
}

// Label returns v if it is one of allowed, else "other": the guard that keeps
// a label's cardinality bounded.
func Label(v string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return "other"
}

type factory struct{ reg prometheus.Registerer }

func (f factory) counter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	f.reg.MustRegister(c)
	return c
}

func (f factory) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	f.reg.MustRegister(c)
	return c
}

func (f factory) gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	f.reg.MustRegister(g)
	return g
}

func (f factory) gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	f.reg.MustRegister(g)
	return g
}

func (f factory) histogramVec(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	f.reg.MustRegister(h)
	return h
}
