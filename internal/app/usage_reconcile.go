package app

import (
	"context"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

// Reconciling the usage history against the accounting state.
//
// Every history row carries a MAC, so a row that was changed fails its read.
// A row that was deleted fails nothing: it is simply not there, and a report
// adds up to less. The accounting state holds the authoritative totals, so
// each accounting day's total row in the history is compared against the
// balances of that day summed across projects — the known cost, the provider
// tokens and the unknown-cost attempts, which both sides count from the same
// settlements.
//
// Only a day that can no longer change is compared: one older than
// reconcileSettledAfter, with no lease still open against it. A younger day
// may still be gaining settlements on one side before the other.
const reconcileSettledAfter = 48 * time.Hour

// reconcilePeriods compares the history's daily totals for the days before
// cutoff (all days when cutoff is empty) with the accounting totals, skipping
// the days a lease is still open against. A day with no history row counts as
// zero, as does a day with no balance.
func reconcilePeriods(totals map[ledger.PeriodKey]ledger.Balance, open map[ledger.PeriodKey]bool, history *usage.HistoryStore, cutoff string) (checked int, mismatched []ledger.PeriodKey, err error) {
	before := func(periodID string) bool { return cutoff == "" || periodID < cutoff }
	rows := make(map[ledger.PeriodKey]domain.DailyRollup)
	if _, err := history.RollupRange("", "", func(key domain.RollupKey, row domain.DailyRollup) error {
		if key.Dimension == domain.RollupDimensionTotal && before(key.PeriodID) {
			rows[ledger.PeriodKey{PeriodID: key.PeriodID, TimezoneVersion: key.TimezoneVersion}] = row
		}
		return nil
	}); err != nil {
		return 0, nil, err
	}
	days := make(map[ledger.PeriodKey]struct{}, len(rows))
	for period := range rows {
		days[period] = struct{}{}
	}
	for period := range totals {
		if before(period.PeriodID) {
			days[period] = struct{}{}
		}
	}
	for period := range days {
		if open[period] {
			continue
		}
		checked++
		balance, row := totals[period], rows[period]
		if balance.CommittedMicrosUSD != row.CostMicrosUSD || balance.InputTokens != row.InputTokens ||
			balance.OutputTokens != row.OutputTokens || balance.UnknownAttempts != row.UnknownAttempts {
			mismatched = append(mismatched, period)
		}
	}
	return checked, mismatched, nil
}

// reconcileUsageHistory runs one reconciliation. The history is brought up to
// the Ledger head first, so it covers everything the accounting totals do.
func (r *Runtime) reconcileUsageHistory(now time.Time) {
	totals, open, watermark, err := r.state.PeriodTotals()
	if err != nil {
		r.logger.Warn("usage history reconciliation skipped", "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		r.logger.Warn("usage history reconciliation skipped; the history did not catch up", "error", err)
		return
	}
	if committed := r.usage.HistoryStats().Committed; committed.Sequence < watermark.Sequence {
		r.logger.Warn("usage history reconciliation skipped; the history is behind the accounting state",
			"history_sequence", committed.Sequence, "accounting_sequence", watermark.Sequence)
		return
	}
	cutoff := now.UTC().Add(-reconcileSettledAfter).Format(time.DateOnly)
	checked, mismatched, err := reconcilePeriods(totals, open, r.usage.History(), cutoff)
	if err != nil {
		r.logger.Warn("usage history reconciliation failed", "error", err)
		return
	}
	r.reconciledPeriods.Store(int64(checked))
	if len(mismatched) == 0 {
		return
	}
	r.reconcileMismatches.Add(uint64(len(mismatched)))
	for _, period := range mismatched {
		r.logger.Error("usage history disagrees with the accounting totals; rebuild it with usage rebuild-summary",
			"period_id", period.PeriodID, "timezone_version", period.TimezoneVersion)
	}
}
