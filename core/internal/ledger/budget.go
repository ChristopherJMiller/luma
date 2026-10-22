package ledger

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// Budget is the global cap of a UTC calendar month (P12, D22): every ledger
// row counts, interactive and ci alike, eval runs included. It warns at
// spent ≥ warn ratio × cap and refuses new turns and sessions at spent ≥ cap
// (budget_exhausted).
type Budget struct {
	st        *store.Store
	capUSD    float64
	warnRatio float64
	loc       *time.Location
	clock     func() time.Time

	// mu guards the month's figure: the most read from the store for the
	// month (a month's spend only grows, so a slower, older read never moves
	// it back), luma_budget_spent_usd's value, and the fallback while the
	// store cannot answer, so an outage never reopens an exhausted budget
	// within its month.
	mu    sync.Mutex
	month time.Time // the first instant of the month spent is for; zero before the first read
	spent float64
}

// gaugeReadTimeout bounds the store read a scrape makes when the process has
// no figure for the current month yet.
const gaugeReadTimeout = 2 * time.Second

// spentNow is luma_budget_spent_usd at a scrape: the current UTC month's
// figure, read from the store when this process has none for the month yet
// (the first scrape after 00:00 UTC on the 1st), so the gauge never shows
// last month's spend as this month's.
func (b *Budget) spentNow() float64 {
	now := b.clock()
	start, _ := monthOf(now)
	b.mu.Lock()
	if b.month.Equal(start) {
		defer b.mu.Unlock()
		return b.spent
	}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), gaugeReadTimeout)
	defer cancel()
	return b.Status(ctx, now).SpentUSD
}

// monthOf returns the first instant of the UTC calendar month holding t and
// of the month after it.
func monthOf(t time.Time) (start, next time.Time) {
	u := t.UTC()
	start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// Status is the month-to-date spend at now against the cap. resets_at is the
// next month's 1st, 00:00 UTC. The figure is the most this process has read
// for the month. When the store fails, the month's figure stands (or nothing
// spent, if this process has none for the month: S62 fails open).
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
	same := b.month.Equal(start)
	switch {
	case err == nil && same:
		spent = max(spent, b.spent)
		b.spent = spent
	case err == nil:
		b.month, b.spent = start, spent
	case same:
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
