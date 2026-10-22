package ledger_test

import (
	"testing"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/ledger"
	"github.com/ChristopherJMiller/luma/core/internal/protocol"
	"github.com/ChristopherJMiller/luma/core/internal/store/storetest"
)

func TestMonthToDateUTCBoundary(t *testing.T) {
	t.Parallel()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// LUMA_TIMEZONE only words the copy: the month is UTC's (P12).
	h := newHarness(t, ledger.Config{MonthlyUSD: 200, Location: la})
	lastOct := time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)
	firstNov := time.Date(2026, 11, 1, 0, 1, 0, 0, time.UTC)
	h.spend(time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC), 7, "interactive")
	h.spend(lastOct, 3, "interactive")
	h.spend(firstNov, 5, "ci")

	oct := h.l.Budget().Status(bg, lastOct.Add(30*time.Second))
	if !near(oct.SpentUSD, 3) {
		t.Errorf("October spent = %v, want 3", oct.SpentUSD)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !oct.ResetsAt.Equal(want) {
		t.Errorf("October resets_at = %v, want %v", oct.ResetsAt, want)
	}
	nov := h.l.Budget().Status(bg, firstNov.Add(30*time.Second)) // Oct 31 in Los Angeles
	if !near(nov.SpentUSD, 5) {
		t.Errorf("November spent = %v, want 5 (both workspaces count, the month is UTC)", nov.SpentUSD)
	}
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !nov.ResetsAt.Equal(want) {
		t.Errorf("November resets_at = %v, want %v", nov.ResetsAt, want)
	}
	if nov.ResetsAt.Location() != time.UTC {
		t.Errorf("resets_at is in %v, want UTC", nov.ResetsAt.Location())
	}
}

func TestWarnAt80Percent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{MonthlyUSD: 200, WarnRatio: 0.8})
	h.spend(testNow, 159.99, "interactive")
	s := h.l.Budget().Status(bg, testNow)
	if s.Warn || s.Exhausted {
		t.Errorf("at 159.99 of 200: warn %v, exhausted %v; want neither", s.Warn, s.Exhausted)
	}
	h.spend(testNow, 0.01, "interactive")
	s = h.l.Budget().Status(bg, testNow)
	want := protocol.BudgetStatus{SpentUSD: 160, CapUSD: 200, Ratio: 0.8, Warn: true, Exhausted: false,
		ResetsAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	if s != want {
		t.Errorf("at 160 of 200: %+v\nwant %+v", s, want)
	}
	if e := h.l.Budget().CheckNewTurn(bg); e != nil {
		t.Errorf("a warned budget refused a turn: %+v", e)
	}
	if v := h.value("luma_budget_spent_usd", nil); v != 160 {
		t.Errorf("luma_budget_spent_usd = %v, want 160", v)
	}
}

func TestExhaustedRefusesNewTurn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{MonthlyUSD: 200})
	h.spend(testNow, 199.99, "interactive")
	if e := h.l.Budget().CheckNewTurn(bg); e != nil {
		t.Fatalf("at 199.99 of 200: refused %+v", e)
	}
	h.spend(testNow, 0.01, "ci")
	s := h.l.Budget().Status(bg, testNow)
	want := protocol.BudgetStatus{SpentUSD: 200, CapUSD: 200, Ratio: 1, Warn: true, Exhausted: true,
		ResetsAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	if s != want {
		t.Errorf("at 200 of 200: %+v\nwant %+v", s, want)
	}
	e := h.l.Budget().CheckNewTurn(bg)
	wantErr := protocol.ErrorInfo{Code: protocol.CodeBudgetExhausted,
		Message: "Luma has used this month's budget. It will be back on November 1, 2026."}
	if e == nil || *e != wantErr {
		t.Errorf("CheckNewTurn = %+v, want %+v", e, wantErr)
	}
	if v := h.value("luma_budget_cap_usd", nil); v != 200 {
		t.Errorf("luma_budget_cap_usd = %v, want 200", v)
	}

	// The 1st, 00:00 UTC, opens a fresh month.
	h.setNow(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if e := h.l.Budget().CheckNewTurn(bg); e != nil {
		t.Errorf("the new month still refuses: %+v", e)
	}
}

func TestBudgetDefaultsAreTheProductDefaults(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	h.spend(testNow, 160, "interactive")
	s := h.l.Budget().Status(bg, testNow)
	if s.CapUSD != 200 || !s.Warn || s.Exhausted {
		t.Errorf("zero Config: %+v, want the C6 defaults ($200, warn at 0.8)", s)
	}
}

func TestBudgetCopyUsesTheLocalDate(t *testing.T) {
	t.Parallel()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, ledger.Config{MonthlyUSD: 10, Location: la})
	h.spend(testNow, 10, "interactive")
	e := h.l.Budget().CheckNewTurn(bg)
	// November 1, 00:00 UTC is still October 31 in Los Angeles.
	want := "Luma has used this month's budget. It will be back on October 31, 2026."
	if e == nil || e.Message != want {
		t.Errorf("CheckNewTurn = %+v, want message %q", e, want)
	}
}

func TestBudgetKeepsItsLastFigureWhenTheStoreFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{MonthlyUSD: 10})
	h.spend(testNow, 10, "interactive")
	if e := h.l.Budget().CheckNewTurn(bg); e == nil {
		t.Fatal("an exhausted budget let a turn through")
	}
	h.st.Close()
	// The month's last figure still stands: a store outage does not reopen
	// an exhausted budget.
	if e := h.l.Budget().CheckNewTurn(bg); e == nil || e.Code != protocol.CodeBudgetExhausted {
		t.Errorf("with the store down: %+v, want budget_exhausted from the last figure", e)
	}
	// A figure from last month is not this month's.
	h.setNow(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	if s := h.l.Budget().Status(bg, h.clock()); s.SpentUSD != 0 || s.Exhausted {
		t.Errorf("a new month with the store down: %+v, want nothing spent", s)
	}
}

// Task 14 review: within a month the figure only grows, so a slower, older
// read can never move the gauge (or the gate) back.
func TestBudgetFigureNeverFallsWithinAMonth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{MonthlyUSD: 10})
	h.spend(testNow, 10, "interactive")
	if s := h.l.Budget().Status(bg, testNow); !s.Exhausted {
		t.Fatalf("at $10 of $10: %+v", s)
	}
	// A read that sees less (here the rows are gone; in life, a read that
	// started before the last write and finished after it) keeps the figure.
	if _, err := storetest.ConnAs(t, h.st, storetest.Superuser).Exec(bg, `delete from cost_ledger`); err != nil {
		t.Fatal(err)
	}
	if s := h.l.Budget().Status(bg, testNow); !near(s.SpentUSD, 10) || !s.Exhausted {
		t.Errorf("after a lower read: %+v, want the month's $10 to stand", s)
	}
	if v := h.value("luma_budget_spent_usd", nil); !near(v, 10) {
		t.Errorf("luma_budget_spent_usd = %v, want 10", v)
	}
	// The next month starts from what the store says.
	h.setNow(time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC))
	if v := h.value("luma_budget_spent_usd", nil); v != 0 {
		t.Errorf("luma_budget_spent_usd = %v just after the 1st, want 0", v)
	}
}

// The gauge is read at each scrape, so at 00:00 UTC on the 1st it is the new
// month's spend without anything having been written or asked.
func TestBudgetGaugeIsFreshAtTheMonthsStart(t *testing.T) {
	t.Parallel()
	h := newHarness(t, ledger.Config{})
	h.setNow(time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC))
	h.spend(time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), 42, "interactive")
	if v := h.value("luma_budget_spent_usd", nil); !near(v, 42) {
		t.Fatalf("luma_budget_spent_usd = %v on October 31, want 42", v)
	}
	h.spend(time.Date(2026, 11, 1, 0, 0, 30, 0, time.UTC), 1.5, "ci")
	h.setNow(time.Date(2026, 11, 1, 0, 1, 0, 0, time.UTC))
	if v := h.value("luma_budget_spent_usd", nil); !near(v, 1.5) {
		t.Errorf("luma_budget_spent_usd = %v at 00:01 UTC on the 1st, want 1.5", v)
	}
}
