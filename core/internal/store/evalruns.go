package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

// InsertEvalRun stores an eval run and returns its id.
func (s *Store) InsertEvalRun(ctx context.Context, r protocol.EvalRun) (int64, error) {
	results, err := json.Marshal(r.Results)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `insert into eval_runs (suite, sdk_version, cli_version, image, started_at,
		finished_at, passed, failed, cost_usd, results) values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) returning id`,
		r.Suite, r.SDKVersion, r.CLIVersion, r.Image, r.StartedAt, r.FinishedAt, r.Passed, r.Failed,
		r.CostUSD, results).Scan(&id)
	return id, err
}

// LastEvalRun returns a suite's most recently finished run.
func (s *Store) LastEvalRun(ctx context.Context, suite string) (protocol.EvalRun, error) {
	var r protocol.EvalRun
	var results []byte
	err := s.pool.QueryRow(ctx, `select suite, sdk_version, cli_version, image, started_at, finished_at,
		passed, failed, cost_usd::float8, results from eval_runs where suite = $1
		order by finished_at desc, id desc limit 1`, suite).
		Scan(&r.Suite, &r.SDKVersion, &r.CLIVersion, &r.Image, &r.StartedAt, &r.FinishedAt,
			&r.Passed, &r.Failed, &r.CostUSD, &results)
	if errors.Is(err, pgx.ErrNoRows) {
		return protocol.EvalRun{}, ErrNotFound
	}
	if err != nil {
		return protocol.EvalRun{}, err
	}
	return r, json.Unmarshal(results, &r.Results)
}
