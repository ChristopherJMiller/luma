package store

import (
	"context"
	"encoding/json"
	"time"
)

// LedgerRow is a C4 `cost_ledger` row: one model's usage in one turn.
// SessionID is nil once its conversation is deleted; the row stays.
type LedgerRow struct {
	ID                  int64
	SessionID           *string
	PersonID            string
	AgentID             string
	Tier                string
	Source              string
	APIWorkspace        string // interactive | ci
	Model               string
	TurnID              string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	WebSearchRequests   int64
	CostUSD             float64
	CreatedAt           time.Time
}

// RecordLedger appends a ledger row and returns its id.
func (s *Store) RecordLedger(ctx context.Context, r LedgerRow) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `insert into cost_ledger (session_id, person_id, agent_id, tier, source,
		api_workspace, model, turn_id, input_tokens, output_tokens, cache_read_tokens,
		cache_creation_tokens, web_search_requests, cost_usd, created_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14, coalesce($15, now())) returning id`,
		r.SessionID, r.PersonID, r.AgentID, r.Tier, r.Source, r.APIWorkspace, r.Model, r.TurnID,
		r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.WebSearchRequests,
		r.CostUSD, nilTime(r.CreatedAt)).Scan(&id)
	return id, err
}

func nilTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// SpendBetween sums ledger dollars in [from, to), optionally for one
// api_workspace ("" for all).
func (s *Store) SpendBetween(ctx context.Context, from, to time.Time, apiWorkspace string) (float64, error) {
	var usd float64
	err := s.pool.QueryRow(ctx, `select coalesce(sum(cost_usd), 0)::float8 from cost_ledger
		where created_at >= $1 and created_at < $2 and ($3 = '' or api_workspace = $3)`,
		from, to, apiWorkspace).Scan(&usd)
	return usd, err
}

// Anomaly is a C4 `usage_anomalies` row.
type Anomaly struct {
	SessionID *string
	TurnID    string
	Reason    string // negative | oversized
	Payload   any
}

// RecordAnomaly stores a usage anomaly. Its payload must carry metadata only.
func (s *Store) RecordAnomaly(ctx context.Context, a Anomaly) error {
	b, err := json.Marshal(a.Payload)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `insert into usage_anomalies (session_id, turn_id, reason, payload) values ($1,$2,$3,$4)`,
		a.SessionID, a.TurnID, a.Reason, b)
	return err
}
