package ledger

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/metrics"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// Budget is the global cap of a UTC calendar month (P12, D22): every ledger
// row counts, interactive and ci alike, eval runs included. It warns at
// spent ≥ warn ratio × cap and refuses new turns and sessions at spent ≥ cap
// (budget_exhausted).
type Budget struct {
	st        *store.Store
	m         *metrics.Core
	capUSD    float64
	warnRatio float64
	loc       *time.Location
	clock     func() time.Time

	// mu guards the month's last figure read from the store: the fallback
	// while the store cannot answer, so an outage never reopens an
	// exhausted budget within its month.
	mu    sync.Mutex
	month time.Time // the first instant of the month spent is for; zero before the first read
	spent float64
}

// monthOf returns the first instant of the UTC calendar month holding t and
// of the month after it.
func monthOf(t time.Time) (start, next time.Time) {
	u := t.UTC()
	start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// Status is the month-to-date spend at now against the cap. resets_at is the
// next month's 1st, 00:00 UTC. It also sets luma_budget_spent_usd. When the
// store fails, the month's last figure stands (or nothing spent, if this
// process has none for the month).
func (b *Budget) Status(ctx context.Context, now time.Time) protocol.BudgetStatus {
	s, err := b.status(ctx, now)
	if err != nil {
		slog.Warn("ledger: reading the month's spend failed; using its last figure", "err", err)
	}
	return s
}

func (b *Budget) status(ctx context.Context, now time.Time) (protocol.BudgetStatus, error) {
	start, next := monthOf(now)
	spent, err := b.st.SpendBetween(ctx, start, next, "")
	b.mu.Lock()
	switch {
	case err == nil:
		b.month, b.spent = start, spent
		b.m.BudgetSpentUSD.Set(spent)
	case b.month.Equal(start):
		spent = b.spent
	default:
		spent = 0
	}
	b.mu.Unlock()
	ratio := spent / b.capUSD
	return protocol.BudgetStatus{
		SpentUSD: spent, CapUSD: b.capUSD, Ratio: ratio,
		Warn: ratio >= b.warnRatio, Exhausted: ratio >= 1, ResetsAt: next,
	}, err
}

// CheckNewTurn refuses Create, Fork and Send once the month's budget is
// spent (S1, S53), with the C3 copy naming the reset's date in
// LUMA_TIMEZONE.
func (b *Budget) CheckNewTurn(ctx context.Context) *protocol.ErrorInfo {
	s := b.Status(ctx, b.clock())
	if !s.Exhausted {
		return nil
	}
	msg := strings.ReplaceAll(protocol.ErrorCopy[protocol.CodeBudgetExhausted], "{resets_at, local date}",
		s.ResetsAt.In(b.loc).Format("January 2, 2006"))
	return &protocol.ErrorInfo{Code: protocol.CodeBudgetExhausted, Message: msg}
}
