package protocol

import "time"

// EvalSpec is the result of GET /internal/v1/agents/{id}/spec?tier= (C10),
// from the live catalog.
type EvalSpec struct {
	Agent               AgentSpec `json:"agent"`
	HelperModel         string    `json:"helper_model"`
	Timezone            string    `json:"timezone"`
	WebfetchDenyDomains []string  `json:"webfetch_deny_domains"`
}

// EvalResult is one golden task's outcome in an EvalRun.
type EvalResult struct {
	ID       string   `json:"id"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures"`
	CostUSD  float64  `json:"cost_usd"`
}

// EvalRun is the body of POST /internal/v1/eval-runs.
type EvalRun struct {
	Suite      string       `json:"suite"`
	SDKVersion string       `json:"sdk_version"`
	CLIVersion string       `json:"cli_version"`
	Image      string       `json:"image"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	Passed     int          `json:"passed"`
	Failed     int          `json:"failed"`
	CostUSD    float64      `json:"cost_usd"`
	Results    []EvalResult `json:"results"`
}

// ReconcileWorkspace is one Anthropic workspace's daily cost in the request.
type ReconcileWorkspace struct {
	Name   string  `json:"name"` // luma-interactive | luma-ci
	APIUSD float64 `json:"api_usd"`
}

// UsageReconciliation is the body of POST /internal/v1/usage-reconciliations.
type UsageReconciliation struct {
	Day        string               `json:"day"` // YYYY-MM-DD
	Workspaces []ReconcileWorkspace `json:"workspaces"`
}

// ReconcileDrift is one workspace's comparison in the response.
type ReconcileDrift struct {
	Name       string  `json:"name"`
	APIUSD     float64 `json:"api_usd"`
	LedgerUSD  float64 `json:"ledger_usd"`
	DriftRatio float64 `json:"drift_ratio"`
}

// UsageReconciliationResult is the response to a UsageReconciliation.
type UsageReconciliationResult struct {
	Workspaces []ReconcileDrift `json:"workspaces"`
}
