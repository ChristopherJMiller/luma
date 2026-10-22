package store

import (
	"context"
	"time"
)

// Reconciliation is a C4 `usage_reconciliations` row: one day of one
// Anthropic workspace (e.g. luma-ci) against the ledger.
type Reconciliation struct {
	Day       time.Time // a date; the time of day is ignored
	Workspace string
	APIUSD    float64
	LedgerUSD float64
	CreatedAt time.Time
}

// UpsertReconciliation stores (or, on a re-run, replaces) a day's comparison.
func (s *Store) UpsertReconciliation(ctx context.Context, r Reconciliation) error {
	_, err := s.pool.Exec(ctx, `insert into usage_reconciliations (day, workspace, api_usd, ledger_usd)
		values ($1::date, $2, $3, $4)
		on conflict (day, workspace) do update set api_usd = excluded.api_usd,
			ledger_usd = excluded.ledger_usd, created_at = now()`,
		r.Day.Format("2006-01-02"), r.Workspace, r.APIUSD, r.LedgerUSD)
	return err
}

// Reconciliations returns the rows of one day, by workspace.
func (s *Store) Reconciliations(ctx context.Context, day time.Time) ([]Reconciliation, error) {
	rows, err := s.pool.Query(ctx, `select day, workspace, api_usd::float8, ledger_usd::float8, created_at
		from usage_reconciliations where day = $1::date order by workspace`, day.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reconciliation
	for rows.Next() {
		var r Reconciliation
		if err := rows.Scan(&r.Day, &r.Workspace, &r.APIUSD, &r.LedgerUSD, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LedgerUSDForDay sums one UTC day's ledger dollars for an api_workspace
// ("interactive" | "ci"; reconcile maps luma-<x> to <x>, S5).
func (s *Store) LedgerUSDForDay(ctx context.Context, day time.Time, apiWorkspace string) (float64, error) {
	d := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	return s.SpendBetween(ctx, d, d.AddDate(0, 0, 1), apiWorkspace)
}

// LatestReconciliations returns, for each workspace, its row of the newest
// day (S7: Core restores the reconcile gauges from these at start).
func (s *Store) LatestReconciliations(ctx context.Context) ([]Reconciliation, error) {
	rows, err := s.pool.Query(ctx, `select distinct on (workspace) day, workspace, api_usd::float8,
		ledger_usd::float8, created_at from usage_reconciliations order by workspace, day desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reconciliation
	for rows.Next() {
		var r Reconciliation
		if err := rows.Scan(&r.Day, &r.Workspace, &r.APIUSD, &r.LedgerUSD, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
