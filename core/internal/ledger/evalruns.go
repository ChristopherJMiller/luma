package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/ChristopherJMiller/luma/core/internal/internalauth"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// Suites are the eval runner's suites (Task 27): the suite label's fixed set,
// and the suites whose gauges InitGauges restores. A run of another suite is
// stored and counted under the label "other", which a restart does not
// restore.
var Suites = []string{"gate", "guard", "nightly", "weekly"}

func suiteLabel(s string) string { return metrics.Label(s, Suites...) }

// maxEvalRunBody bounds a POST /internal/v1/eval-runs body.
const maxEvalRunBody = 1 << 20

// The C10 keys an eval run and each of its results must carry (S37: missing
// required keys are refused, unknown keys ignored).
var (
	evalRunKeys    = []string{"suite", "sdk_version", "cli_version", "image", "started_at", "finished_at", "passed", "failed", "cost_usd", "results"}
	evalResultKeys = []string{"id", "passed", "failures", "cost_usd"}
)

// The ledger row of an eval run's cost (S5): the ci person, source and
// api_workspace, so the global budget counts it (P12) and the reconcile
// compares it with luma-ci.
const (
	evalPerson = "ci"
	evalAgent  = "evals"
	evalTier   = "ci"
	evalModel  = "eval-run"
)

// EvalRunsHandler serves POST /internal/v1/eval-runs (C10), mounted behind
// internalauth for the evals principal; it checks the principal itself too,
// so a missing or wider middleware lets nobody else in. A body that is not
// an eval run is 400; one with cost_usd outside [0, maxCost], an empty
// suite, a negative count or cost, or a finish before its start is 422. A
// run posted again is answered 201 and adds nothing. A zero maxCost is
// LUMA_EVAL_MAX_COST_USD's default. st must be the ledger's own store: the
// run and its cost row are written in one of its transactions, so New's
// store is the one used, and another one is a wiring bug (it panics).
func EvalRunsHandler(l *Ledger, st *store.Store, maxCost float64) http.Handler {
	if st != l.st {
		panic("ledger: EvalRunsHandler's store is not the ledger's")
	}
	if maxCost <= 0 {
		maxCost = DefaultMaxEvalCostUSD
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, _, ok := internalauth.PrincipalFrom(r.Context()); !ok || p != internalauth.PrincipalEvals {
			slog.Warn("ledger: refused an eval-runs caller that is not evals", "principal", string(p))
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEvalRunBody))
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeErrorInfo(w, http.StatusRequestEntityTooLarge, protocol.CodeRequestTooLarge)
			return
		case err != nil:
			writeErrorInfo(w, http.StatusBadRequest, protocol.CodeInvalidRequest)
			return
		}
		run, err := decodeEvalRun(b)
		if err != nil {
			slog.Info("ledger: refused a malformed eval run", "err", err)
			writeErrorInfo(w, http.StatusBadRequest, protocol.CodeInvalidRequest)
			return
		}
		if why := invalidEvalRun(run, maxCost); why != "" {
			slog.Info("ledger: refused an eval run", "suite", run.Suite, "reason", why)
			writeErrorInfo(w, http.StatusUnprocessableEntity, protocol.CodeInvalidRequest)
			return
		}
		recorded, err := l.recordEvalRun(r.Context(), run)
		if err != nil {
			slog.Error("ledger: storing an eval run failed", "suite", run.Suite, "err", err)
			writeErrorInfo(w, http.StatusInternalServerError, protocol.CodeUpstreamError)
			return
		}
		if recorded {
			slog.Info("ledger: eval run recorded", "suite", run.Suite, "passed", run.Passed, "failed", run.Failed,
				"cost_usd", run.CostUSD)
		} else {
			slog.Info("ledger: eval run already recorded; nothing added", "suite", run.Suite)
		}
		w.WriteHeader(http.StatusCreated)
	})
}

// decodeEvalRun decodes an eval run whose required keys are all present and
// not null.
func decodeEvalRun(b []byte) (protocol.EvalRun, error) {
	var run protocol.EvalRun
	if err := requireKeys(b, evalRunKeys); err != nil {
		return run, err
	}
	var raw struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return run, err
	}
	for i, res := range raw.Results {
		if err := requireKeys(res, evalResultKeys); err != nil {
			return run, fmt.Errorf("results[%d]: %w", i, err)
		}
	}
	return run, json.Unmarshal(b, &run)
}

func requireKeys(b []byte, keys []string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for _, k := range keys {
		if v, ok := m[k]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("missing %s", k)
		}
	}
	return nil
}

// invalidEvalRun is why a decoded run is refused (422), or "".
func invalidEvalRun(run protocol.EvalRun, maxCost float64) string {
	switch {
	case run.Suite == "":
		return "empty suite"
	case !(run.CostUSD >= 0) || run.CostUSD > maxCost:
		return "cost_usd out of bounds"
	case run.Passed < 0 || run.Failed < 0:
		return "negative count"
	case run.FinishedAt.Before(run.StartedAt):
		return "finished before it started"
	}
	for _, res := range run.Results {
		if !(res.CostUSD >= 0) {
			return "negative result cost"
		}
	}
	return ""
}

// recordEvalRun stores the run and the ledger row of its cost in one
// transaction, then moves the run's metrics. The row is written even at $0:
// its turn id, eval_<suite>_<finished_at>, identifies the run, so a run
// posted again is stored and counted once (Task 14 review). recorded is
// false for such a repeat.
func (l *Ledger) recordEvalRun(ctx context.Context, run protocol.EvalRun) (recorded bool, err error) {
	now := l.clock()
	recorded, err = l.st.InsertEvalRunWithCost(ctx, run, store.LedgerRow{
		PersonID: evalPerson, AgentID: evalAgent, Tier: evalTier, Source: "ci", APIWorkspace: "ci",
		Model: evalModel, TurnID: "eval_" + run.Suite + "_" + run.FinishedAt.UTC().Format("20060102T150405Z"),
		CostUSD: run.CostUSD, CreatedAt: now,
	})
	if err != nil || !recorded {
		return false, err
	}
	l.m.EvalCostUSDTotal.WithLabelValues(suiteLabel(run.Suite)).Add(run.CostUSD)
	l.budget.Status(ctx, now) // the month's figure behind luma_budget_spent_usd
	l.setEvalGauges(run)
	return true, nil
}

func writeErrorInfo(w http.ResponseWriter, status int, code protocol.ErrorCode) {
	b, _ := json.Marshal(protocol.ErrorInfo{Code: code, Message: protocol.ErrorCopy[code]})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
