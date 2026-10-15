package usage

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

var testHistoryKey = bytes.Repeat([]byte{0x5a}, 32)

// newTestAggregate is an empty aggregate over a fresh history index in the
// test's temporary directory.
func newTestAggregate(t testing.TB) *Aggregate {
	t.Helper()
	return NewAggregate(newTestHistory(t))
}

func newTestHistory(t testing.TB) *HistoryStore {
	t.Helper()
	store, _, err := OpenHistory(filepath.Join(t.TempDir(), "history.db"), testHistoryKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// testSnapshot is everything an aggregate's index holds, read back whole.
type testSnapshot struct {
	Watermark ledger.Watermark
	Totals    Bucket
	Attempts  []AttemptEvent
	Requests  []RequestSummary
}

// snapshotOf flushes the aggregate and reads its index back.
func snapshotOf(t testing.TB, aggregate *Aggregate) testSnapshot {
	t.Helper()
	if err := aggregate.Flush(); err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot{Watermark: aggregate.Watermark(), Totals: aggregate.Totals()}
	if err := aggregate.History().AttemptsAfter(0, func(attempt AttemptEvent) error {
		snapshot.Attempts = append(snapshot.Attempts, attempt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := aggregate.History().RequestsAfter(0, func(summary RequestSummary) error {
		snapshot.Requests = append(snapshot.Requests, summary)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// attemptSlice is an attempt source over a fixed list, for export tests that
// do not need an aggregate behind them.
type attemptSlice []AttemptEvent

func (s attemptSlice) AttemptsAfter(after uint64, visit func(AttemptEvent) error) error {
	for _, attempt := range s {
		if attempt.Sequence <= after {
			continue
		}
		if err := visit(attempt); err != nil {
			return err
		}
	}
	return nil
}

// restoreOneRound writes the aggregate's pending batch and resumes a new
// aggregate from what its index then holds — what a restart does.
func restoreOneRound(aggregate *Aggregate) (*Aggregate, error) {
	if err := aggregate.Flush(); err != nil {
		return nil, err
	}
	return RestoreAggregate(aggregate.History())
}

func dashboardOf(t testing.TB, aggregate *Aggregate, now time.Time, period Period) Dashboard {
	t.Helper()
	dashboard, err := aggregate.Dashboard(now, period)
	if err != nil {
		t.Fatal(err)
	}
	return dashboard
}

func detailOf(t testing.TB, aggregate *Aggregate, requestID string) (RequestDetail, bool) {
	t.Helper()
	detail, found, err := aggregate.RequestDetail(requestID)
	if err != nil {
		t.Fatal(err)
	}
	return detail, found
}

// rollupRowsOf flushes the aggregate and reads every stored daily row, keyed
// by its encoded rollup key.
func rollupRowsOf(t testing.TB, aggregate *Aggregate) map[string]domain.DailyRollup {
	t.Helper()
	if err := aggregate.Flush(); err != nil {
		t.Fatal(err)
	}
	rows := map[string]domain.DailyRollup{}
	if err := aggregate.History().RollupRange("", "", func(key domain.RollupKey, row domain.DailyRollup) error {
		rows[key.Encode()] = row
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// exportFixture is a hand-built list of attempts for the exporter tests, which
// exercise partitioning and verification rather than an aggregate.
type exportFixture struct {
	Attempts []AttemptEvent
}

func (f exportFixture) AttemptsAfter(after uint64, visit func(AttemptEvent) error) error {
	return attemptSlice(f.Attempts).AttemptsAfter(after, visit)
}
