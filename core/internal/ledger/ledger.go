// Package ledger records spend: one cost_ledger row per model per turn from
// the SDK's per-turn deltas (P11), the usage anomalies that never reach the
// ledger, the global monthly budget (P12), the eval runs' cost, and the SLO
// counters of every main-class request (P10). Its *Ledger is the
// sessions.Ledger (S3), called from inside a session's actor: it touches only
// the store and the metrics, never the session service.
package ledger

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/agents"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// The C6 product defaults; the site sets its own cap (core.budget.monthlyUsd).
const (
	DefaultMonthlyUSD     = 200 // LUMA_BUDGET_MONTHLY_USD
	DefaultWarnRatio      = 0.8 // LUMA_BUDGET_WARN_RATIO
	DefaultMaxTurnCostUSD = 5   // LUMA_MAX_TURN_COST_USD
	DefaultMaxEvalCostUSD = 25  // LUMA_EVAL_MAX_COST_USD
)

// over100K is the prompt size Core counts main requests above: the price
// step the fast tier's compaction window keeps chat under.
const over100K = 100_000

// Anomaly reasons, the C4 usage_anomalies.reason values.
const (
	reasonNegative  = "negative"
	reasonOversized = "oversized"
)

// Config is the site configuration of the ledger and the budget. A zero
// field takes its C6 default.
type Config struct {
	MonthlyUSD     float64          // LUMA_BUDGET_MONTHLY_USD: the global cap of a UTC month
	WarnRatio      float64          // LUMA_BUDGET_WARN_RATIO: warn at spent ≥ WarnRatio × cap
	MaxTurnCostUSD float64          // LUMA_MAX_TURN_COST_USD: a larger turn delta is an anomaly
	Location       *time.Location   // LUMA_TIMEZONE: words the date in the budget_exhausted copy only; nil is UTC
	Clock          func() time.Time // nil is time.Now
}

// Ledger records spend and implements sessions.Ledger.
type Ledger struct {
	st         *store.Store
	m          *metrics.Core
	live       *agents.Live
	budget     *Budget
	maxTurnUSD float64
	clock      func() time.Time

	mu sync.Mutex
	// evalLast is the newest finished_at behind each suite label's eval
	// gauges, so a late post of an older run never moves them back.
	evalLast map[string]time.Time
}

// New builds the ledger and its budget.
func New(st *store.Store, m *metrics.Core, live *agents.Live, cfg Config) *Ledger {
	if cfg.MonthlyUSD <= 0 {
		cfg.MonthlyUSD = DefaultMonthlyUSD
	}
	if cfg.WarnRatio <= 0 {
		cfg.WarnRatio = DefaultWarnRatio
	}
	if cfg.MaxTurnCostUSD <= 0 {
		cfg.MaxTurnCostUSD = DefaultMaxTurnCostUSD
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	m.BudgetCapUSD.Set(cfg.MonthlyUSD)
	return &Ledger{
		st: st, m: m, live: live, maxTurnUSD: cfg.MaxTurnCostUSD, clock: cfg.Clock,
		budget: &Budget{st: st, m: m, capUSD: cfg.MonthlyUSD, warnRatio: cfg.WarnRatio,
			loc: cfg.Location, clock: cfg.Clock},
		evalLast: map[string]time.Time{},
	}
}

// Budget is the global monthly budget, the Gate's budget half (S1).
func (l *Ledger) Budget() *Budget { return l.budget }

// RecordTurn ledgers a turn's usage, a per-turn delta of the SDK's running
// totals (P11): one cost_ledger row per model that has any usage, with the
// session's api_workspace (S5). A delta with a negative figure, or dollars
// over LUMA_MAX_TURN_COST_USD, is a usage_anomalies row instead, and nothing
// is ledgered. A cost basis other than "list" and a model outside the
// session's bound models (P29) are counted; the rows are still recorded.
func (l *Ledger) RecordTurn(ctx context.Context, sess store.Session, turnID string, u protocol.TurnUsage, costBasis *string) error {
	if costBasis != nil && *costBasis != "list" {
		l.m.CostBasisUnexpectedTotal.Inc()
	}
	if reason := l.anomaly(u); reason != "" {
		return l.recordAnomaly(ctx, sess, turnID, u, costBasis, reason)
	}
	names := make([]string, 0, len(u.Models))
	for name, mu := range u.Models {
		if mu != (protocol.ModelUsage{}) { // a model of the totals this turn did not use
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return nil
	}
	now := l.clock()
	known := l.knownModels()
	agent, tier, source := l.agentLabel(sess.AgentID), tierLabel(sess.Tier), sourceLabel(sess.Source)
	for _, name := range names {
		mu := u.Models[name]
		model := modelLabel(name, known)
		if !slices.Contains(sess.BoundModels, name) {
			l.m.UnboundModelRequestsTotal.WithLabelValues(model).Inc()
			slog.Warn("ledger: a model outside the session's bound models", "session", sess.ID, "turn", turnID, "model", model)
		}
		sid := sess.ID
		if _, err := l.st.RecordLedger(ctx, store.LedgerRow{
			SessionID: &sid, PersonID: sess.PersonID, AgentID: sess.AgentID, Tier: sess.Tier, Source: sess.Source,
			APIWorkspace: sess.APIWorkspace, Model: name, TurnID: turnID,
			InputTokens: mu.Input, OutputTokens: mu.Output, CacheReadTokens: mu.CacheRead,
			CacheCreationTokens: mu.CacheCreation, WebSearchRequests: mu.WebSearchRequests,
			CostUSD: mu.CostUSD, CreatedAt: now,
		}); err != nil {
			return err
		}
		person := sess.PersonID
		l.m.CostUSDTotal.WithLabelValues(person, agent, tier, source, model).Add(mu.CostUSD)
		for typ, n := range map[string]int64{"input": mu.Input, "output": mu.Output,
			"cache_read": mu.CacheRead, "cache_creation": mu.CacheCreation} {
			l.m.TokensTotal.WithLabelValues(person, agent, tier, source, model, typ).Add(float64(n))
		}
		if mu.WebSearchRequests > 0 {
			l.m.WebSearchRequestsTotal.WithLabelValues(person, agent, tier, source).Add(float64(mu.WebSearchRequests))
		}
	}
	l.budget.Status(ctx, now) // refreshes luma_budget_spent_usd
	return nil
}

// anomaly is the reason a turn delta cannot be ledgered, or "". A delta
// below zero (a wrong baseline, S31) is negative; dollars over the cap,
// as the turn's total or as the sum of its models' (what the rows would
// add up to), are oversized.
func (l *Ledger) anomaly(u protocol.TurnUsage) string {
	sum := 0.0
	negative := !(u.CostUSD >= 0) // also NaN
	for _, mu := range u.Models {
		if mu.Input < 0 || mu.Output < 0 || mu.CacheRead < 0 || mu.CacheCreation < 0 ||
			mu.WebSearchRequests < 0 || !(mu.CostUSD >= 0) {
			negative = true
		}
		sum += mu.CostUSD
	}
	switch {
	case negative:
		return reasonNegative
	case u.CostUSD > l.maxTurnUSD || sum > l.maxTurnUSD:
		return reasonOversized
	}
	return ""
}

// anomalyPayload is a usage_anomalies payload: numbers and model names only.
type anomalyPayload struct {
	Usage          protocol.TurnUsage `json:"usage"`
	CostBasis      *string            `json:"cost_basis"`
	MaxTurnCostUSD float64            `json:"max_turn_cost_usd"`
}

func (l *Ledger) recordAnomaly(ctx context.Context, sess store.Session, turnID string, u protocol.TurnUsage,
	costBasis *string, reason string) error {
	l.m.UsageAnomaliesTotal.WithLabelValues(reason).Inc()
	slog.Warn("ledger: usage anomaly, not ledgered", "session", sess.ID, "turn", turnID, "reason", reason,
		"cost_usd", u.CostUSD)
	sid := sess.ID
	return l.st.RecordAnomaly(ctx, store.Anomaly{SessionID: &sid, TurnID: turnID, Reason: reason,
		Payload: anomalyPayload{Usage: u, CostBasis: costBasis, MaxTurnCostUSD: l.maxTurnUSD}})
}

// RecordMainRequest counts one main-class API request toward the cache SLO
// (P10). Input is the API's uncached input_tokens, so input, cache_read and
// cache_creation sum to the whole prompt and the share is cache_read over
// their sum (S28). A request the conductor marks first, cold_resume or
// post_compaction is excluded from the SLO and counted by reason. A prompt
// over 100K tokens is counted whether or not it is excluded.
func (l *Ledger) RecordMainRequest(sess store.Session, u protocol.MainRequestUsage) {
	if u.Input < 0 || u.CacheRead < 0 || u.CacheCreate < 0 {
		slog.Warn("ledger: dropped a main request with negative usage", "session", sess.ID, "api_message", u.APIMessageID)
		return
	}
	agent, tier, source := l.agentLabel(sess.AgentID), tierLabel(sess.Tier), sourceLabel(sess.Source)
	slo := "included"
	if u.SLOExclusion != "none" {
		slo = "excluded"
		l.m.MainRequestExclusionsTotal.WithLabelValues(agent, tier,
			metrics.Label(u.SLOExclusion, "first", "cold_resume", "post_compaction")).Inc()
	}
	l.m.MainRequestTokensTotal.WithLabelValues(agent, tier, source, "input", slo).Add(float64(u.Input))
	l.m.MainRequestTokensTotal.WithLabelValues(agent, tier, source, "cache_read", slo).Add(float64(u.CacheRead))
	l.m.MainRequestTokensTotal.WithLabelValues(agent, tier, source, "cache_creation", slo).Add(float64(u.CacheCreate))
	if u.Input+u.CacheRead+u.CacheCreate > over100K {
		l.m.MainRequestsOver100kTotal.WithLabelValues(tier).Inc()
	}
}

// InitGauges sets the budget gauges and each suite's eval gauges from the
// database, so a restarted Core never exports them empty (S7). A suite that
// has never run gets no series.
func (l *Ledger) InitGauges(ctx context.Context) error {
	l.m.BudgetCapUSD.Set(l.budget.capUSD)
	if _, err := l.budget.status(ctx, l.clock()); err != nil {
		return err
	}
	for _, suite := range Suites {
		r, err := l.st.LastEvalRun(ctx, suite)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		l.setEvalGauges(r)
	}
	return nil
}

// setEvalGauges points the last-run gauges of r's suite label at r unless a
// newer run is already behind them. A run succeeded when it passed at least
// one check and failed none.
func (l *Ledger) setEvalGauges(r protocol.EvalRun) {
	suite := suiteLabel(r.Suite)
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.evalLast[suite]; ok && r.FinishedAt.Before(last) {
		return
	}
	l.evalLast[suite] = r.FinishedAt
	l.m.EvalLastRunTimestampSeconds.WithLabelValues(suite).Set(float64(r.FinishedAt.Unix()))
	success := 0.0
	if r.Failed == 0 && r.Passed > 0 {
		success = 1
	}
	l.m.EvalLastRunSuccess.WithLabelValues(suite).Set(success)
}

// ---- labels (C7: a value outside a fixed set is "other") ----

// knownModels is every model the live models.yaml names: the helper and each
// tier's (the subagents' models are tiers' models too).
func (l *Ledger) knownModels() []string {
	m := l.live.Models()
	out := []string{m.HelperModel}
	for _, t := range m.Tiers {
		out = append(out, t.Model)
	}
	return out
}

func modelLabel(model string, known []string) string { return metrics.Label(model, known...) }

// agentLabel is the agent id while the live catalog has it, else "other".
func (l *Ledger) agentLabel(id string) string {
	if _, ok := l.live.Agent(id); ok {
		return id
	}
	return "other"
}

func tierLabel(t string) string   { return metrics.Label(t, "fast", "smart", "deep") }
func sourceLabel(s string) string { return metrics.Label(s, "web", "ci") }
