package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/internalauth"
	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// APIWorkspaces is C10's fixed mapping (S5): Anthropic workspace name ->
// the ledger's api_workspace.
var APIWorkspaces = map[string]string{
	"luma-interactive": "interactive",
	"luma-ci":          "ci",
}

const (
	maxBody     = 64 << 10
	driftFloor  = 0.01 // drift_ratio = drift_usd / max(api_usd, driftFloor)
	dayLayout   = "2006-01-02"
	maxWorkspcs = 16
)

// Handler serves POST /internal/v1/usage-reconciliations (C10), mounted
// behind internalauth for the reconcile principal; it checks the principal
// itself too. Every name is validated before anything is stored: an unknown
// workspace name (or a negative or non-finite api_usd) is 422 and stores
// nothing. A re-post of the same (day, workspace) replaces its row.
func Handler(st *store.Store, m *metrics.Core) http.Handler {
	return handler(st, m, time.Now)
}

func handler(st *store.Store, m *metrics.Core, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, _, ok := internalauth.PrincipalFrom(r.Context()); !ok || p != internalauth.PrincipalReconcile {
			slog.Warn("reconcile: refused a caller that is not reconcile", "principal", string(p))
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, protocol.CodeRequestTooLarge)
			return
		case err != nil:
			writeError(w, http.StatusBadRequest, protocol.CodeInvalidRequest)
			return
		}
		req, err := decode(b)
		if err != nil {
			slog.Info("reconcile: refused a malformed request", "err", err)
			writeError(w, http.StatusBadRequest, protocol.CodeInvalidRequest)
			return
		}
		day, err := time.Parse(dayLayout, req.Day)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, protocol.CodeInvalidRequest)
			return
		}
		for _, ws := range req.Workspaces {
			if _, ok := APIWorkspaces[ws.Name]; !ok || !(ws.APIUSD >= 0) || math.IsInf(ws.APIUSD, 0) {
				slog.Info("reconcile: refused a workspace", "workspace_known", ok)
				writeError(w, http.StatusUnprocessableEntity, protocol.CodeInvalidRequest)
				return
			}
		}
		out := protocol.UsageReconciliationResult{Workspaces: []protocol.ReconcileDrift{}}
		for _, ws := range req.Workspaces {
			ledger, err := st.LedgerUSDForDay(r.Context(), day, APIWorkspaces[ws.Name])
			if err == nil {
				err = st.UpsertReconciliation(r.Context(), store.Reconciliation{Day: day, Workspace: ws.Name, APIUSD: ws.APIUSD, LedgerUSD: ledger})
			}
			if err != nil {
				slog.Error("reconcile: storing a comparison failed", "workspace", ws.Name, "err", err)
				writeError(w, http.StatusInternalServerError, protocol.CodeUpstreamError)
				return
			}
			out.Workspaces = append(out.Workspaces, protocol.ReconcileDrift{
				Name: ws.Name, APIUSD: ws.APIUSD, LedgerUSD: ledger, DriftRatio: driftRatio(ledger, ws.APIUSD),
			})
			slog.Info("reconcile: compared", "day", req.Day, "workspace", ws.Name, "api_usd", ws.APIUSD,
				"ledger_usd", ledger, "drift_usd", ledger-ws.APIUSD)
		}
		// The gauges follow the newest day stored, so a back-fill of an older
		// day does not move them.
		if err := setGauges(r.Context(), st, m); err != nil {
			slog.Error("reconcile: refreshing gauges failed", "err", err)
		}
		m.UsageReconcileLastSuccessTimestampSeconds.Set(float64(now().Unix()))
		b, _ = json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
}

func driftRatio(ledger, api float64) float64 { return (ledger - api) / math.Max(api, driftFloor) }

// InitGauges sets the luma_usage_reconcile_* gauges from the newest stored row
// of each workspace, so a restarted Core does not report "never reconciled"
// until the next run (S7). A workspace with no row has no series.
func InitGauges(ctx context.Context, st *store.Store, m *metrics.Core) error {
	if err := setGauges(ctx, st, m); err != nil {
		return err
	}
	rows, err := st.LatestReconciliations(ctx)
	if err != nil {
		return err
	}
	var last time.Time
	for _, r := range rows {
		if r.CreatedAt.After(last) {
			last = r.CreatedAt
		}
	}
	if !last.IsZero() {
		m.UsageReconcileLastSuccessTimestampSeconds.Set(float64(last.Unix()))
	}
	return nil
}

func setGauges(ctx context.Context, st *store.Store, m *metrics.Core) error {
	rows, err := st.LatestReconciliations(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, ok := APIWorkspaces[r.Workspace]; !ok {
			continue
		}
		m.UsageReconcileDriftUSD.WithLabelValues(r.Workspace).Set(r.LedgerUSD - r.APIUSD)
		m.UsageReconcileDriftRatio.WithLabelValues(r.Workspace).Set(driftRatio(r.LedgerUSD, r.APIUSD))
	}
	return nil
}

// decode requires day and workspaces (and name and api_usd in each), refusing
// null; unknown keys are ignored (S37).
func decode(b []byte) (protocol.UsageReconciliation, error) {
	var req protocol.UsageReconciliation
	if err := requireKeys(b, "day", "workspaces"); err != nil {
		return req, err
	}
	var raw struct {
		Workspaces []json.RawMessage `json:"workspaces"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return req, err
	}
	if len(raw.Workspaces) > maxWorkspcs {
		return req, errors.New("too many workspaces")
	}
	for _, w := range raw.Workspaces {
		if err := requireKeys(w, "name", "api_usd"); err != nil {
			return req, err
		}
	}
	return req, json.Unmarshal(b, &req)
}

func requireKeys(b []byte, keys ...string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for _, k := range keys {
		if v, ok := m[k]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return errors.New("missing " + k)
		}
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, code protocol.ErrorCode) {
	b, _ := json.Marshal(protocol.ErrorInfo{Code: code, Message: protocol.ErrorCopy[code]})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
