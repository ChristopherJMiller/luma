package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
)

const (
	retries      = 5
	firstBackoff = time.Second
	postPath     = "/internal/v1/usage-reconciliations"
)

// sleep waits d or until ctx ends; tests replace it with a fake clock.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run reads the day's cost of each named workspace from the Admin API and
// posts it to Core (C10). Core's 503 (a TokenReview that could not complete,
// S43) is retried with backoff 1 s, 2 s, 4 s, 8 s, 16 s, or its Retry-After
// when longer, re-reading tokenFile each time; any other non-2xx, or a sixth
// 503, is an error. The Admin key is used only by c and goes nowhere else.
func Run(ctx context.Context, c *AdminClient, core *http.Client, coreURL, tokenFile string, day time.Time, names []string) error {
	ids, err := c.WorkspaceIDs(ctx, names)
	if err != nil {
		return err
	}
	cost, err := c.DailyCost(ctx, day, ids)
	if err != nil {
		return err
	}
	body := protocol.UsageReconciliation{Day: day.UTC().Format(dayLayout), Workspaces: []protocol.ReconcileWorkspace{}}
	for _, n := range names {
		body.Workspaces = append(body.Workspaces, protocol.ReconcileWorkspace{Name: n, APIUSD: cost[n]})
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if core == nil {
		core = http.DefaultClient
	}
	backoff := firstBackoff
	for attempt := 0; ; attempt++ {
		status, retryAfter, res, err := post(ctx, core, coreURL, tokenFile, b)
		if err != nil {
			return err
		}
		switch {
		case status >= 200 && status < 300:
			for _, w := range res.Workspaces {
				slog.Info("reconcile: posted", "day", body.Day, "workspace", w.Name, "api_usd", w.APIUSD,
					"ledger_usd", w.LedgerUSD, "drift_ratio", w.DriftRatio)
			}
			return nil
		case status == http.StatusServiceUnavailable && attempt < retries:
			wait := max(backoff, retryAfter)
			slog.Warn("reconcile: Core said 503, retrying", "attempt", attempt+1, "wait", wait.String())
			if err := sleep(ctx, wait); err != nil {
				return err
			}
			backoff *= 2
		default:
			return fmt.Errorf("core refused the reconciliation: HTTP %d", status)
		}
	}
}

// post sends one attempt, with the token read afresh.
func post(ctx context.Context, core *http.Client, coreURL, tokenFile string, body []byte) (int, time.Duration, protocol.UsageReconciliationResult, error) {
	var res protocol.UsageReconciliationResult
	tok, err := os.ReadFile(tokenFile)
	if err != nil || strings.TrimSpace(string(tok)) == "" {
		return 0, 0, res, errors.New("core token file unreadable or empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(coreURL, "/")+postPath, bytes.NewReader(body))
	if err != nil {
		return 0, 0, res, errors.New("bad core URL")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := core.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, 0, res, ctx.Err()
		}
		return 0, 0, res, errors.New("posting to core failed: transport error")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var ra time.Duration
	if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
		ra = time.Duration(s) * time.Second
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_ = json.Unmarshal(data, &res) // the response is informational; a bad one does not fail a stored run
	}
	return resp.StatusCode, ra, res, nil
}
