package usage

import (
	"fmt"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

// The daily rollup's merge rules, exercised directly against the history
// index the way a batch commit applies them.

func putRollupDelta(store *HistoryStore, delta map[string]domain.DailyRollup) error {
	return store.db.Update(func(tx *bbolt.Tx) error {
		return store.applyRollupDelta(tx.Bucket(historyRollups), delta)
	})
}

func rollupIncrement(attempts, cost int64) domain.DailyRollup {
	row := domain.DailyRollup{Attempts: attempts, CostMicrosUSD: cost}
	row.Identity("2026-08-30", 2, "Asia/Shanghai", 1, 2)
	return row
}

func totalRollupKey() domain.RollupKey {
	return domain.RollupKey{
		PeriodID: "2026-08-30", TimezoneVersion: 2,
		Dimension: domain.RollupDimensionTotal, DimensionKey: domain.RollupTotalKey,
	}
}

// The key names the accounting day and so does the row. If they can disagree,
// one day's traffic can be filed under another's heading and the total that
// reads by key prefix would silently include it.
func TestUsageRollupRejectsRowsThatContradictTheirKey(t *testing.T) {
	store := newTestHistory(t)
	mismatched := rollupIncrement(1, 10)
	mismatched.PeriodID = "2026-08-31"
	err := putRollupDelta(store, map[string]domain.DailyRollup{totalRollupKey().Encode(): mismatched})
	if err == nil {
		t.Fatal("a row filed under another day's key was accepted")
	}
}

// A dimension key can legitimately contain "/" — Gemini public models are
// "models/...", Bedrock inference profiles are ARNs — so the separator has to
// be one the key cannot contain, and the scan has to stay inside its prefix.
func TestUsageRollupKeysSurviveSlashesInDimensionValues(t *testing.T) {
	store := newTestHistory(t)
	slashed := domain.RollupKey{
		PeriodID: "2026-08-30", TimezoneVersion: 2,
		Dimension: domain.RollupDimensionProviderModel, DimensionKey: "models/gemini-2.5/pro",
	}
	other := domain.RollupKey{
		PeriodID: "2026-08-31", TimezoneVersion: 2,
		Dimension: domain.RollupDimensionProviderModel, DimensionKey: "models/other",
	}
	otherRow := domain.DailyRollup{Attempts: 5}
	otherRow.Identity("2026-08-31", 2, "Asia/Shanghai", 1, 2)
	if err := putRollupDelta(store, map[string]domain.DailyRollup{
		slashed.Encode(): rollupIncrement(1, 90),
		other.Encode():   otherRow,
	},
	); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int64{}
	if err := store.RollupRange("2026-08-30", "2026-08-30",
		func(key domain.RollupKey, row domain.DailyRollup) error {
			if key.Dimension != domain.RollupDimensionProviderModel {
				return nil
			}
			seen[key.DimensionKey] = row.Attempts
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen["models/gemini-2.5/pro"] != 1 {
		t.Fatalf("the range crossed its end or lost the key: %#v", seen)
	}
}

func cappedIncrement(sequence uint64, cost int64) domain.DailyRollup {
	row := domain.DailyRollup{Attempts: 1, CostMicrosUSD: cost, FirstSequence: sequence}
	row.Identity("2026-08-30", 2, "Asia/Shanghai", 1, 2)
	return row
}

func providerModelKey(value string) string {
	return domain.RollupKey{
		PeriodID: "2026-08-30", TimezoneVersion: 2,
		Dimension: domain.RollupDimensionProviderModel, DimensionKey: value,
	}.Encode()
}

func dimensionRows(t *testing.T, store *HistoryStore) map[string]domain.DailyRollup {
	t.Helper()
	rows := map[string]domain.DailyRollup{}
	if err := store.RollupRange("", "", func(key domain.RollupKey, row domain.DailyRollup) error {
		if key.Dimension == domain.RollupDimensionProviderModel {
			rows[key.DimensionKey] = row
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// An unbounded day would grow the rollup — and every backup carrying it — with
// how many distinct model names a caller happens to use. The tail is folded,
// and the folded row still carries its attempts so the dimension keeps adding
// up to the day's total.
func TestUsageRollupBoundsKeysPerDimension(t *testing.T) {
	store := newTestHistory(t)
	delta := map[string]domain.DailyRollup{}
	overflow := domain.MaxRollupKeysPerDimension + 5
	for index := 0; index < overflow; index++ {
		delta[providerModelKey(fmt.Sprintf("vendor/model-%03d", index))] = cappedIncrement(uint64(index+1), 10)
	}
	if err := putRollupDelta(store, delta); err != nil {
		t.Fatal(err)
	}
	rows := dimensionRows(t, store)
	if len(rows) != domain.MaxRollupKeysPerDimension+1 {
		t.Fatalf("stored %d keys, want the cap plus the folded row", len(rows))
	}
	folded := rows[domain.RollupOtherKey]
	if folded.Attempts != 5 || folded.CostMicrosUSD != 50 {
		t.Fatalf("folded row=%#v", folded)
	}
	var attempts int64
	for _, row := range rows {
		attempts += row.Attempts
	}
	if attempts != int64(overflow) {
		t.Fatalf("dimension totals %d attempts, want %d", attempts, overflow)
	}
	// Ledger order decides, so the earliest values survive and the latest fold.
	if _, kept := rows["vendor/model-000"]; !kept {
		t.Fatal("the first value to appear was not kept")
	}
	if _, kept := rows[fmt.Sprintf("vendor/model-%03d", overflow-1)]; kept {
		t.Fatal("a value past the cap was admitted")
	}
}

// The cap has to reach the same decision however the day was written: one
// rebuild pass, or a hundred checkpoint increments. Sorting admissions by the
// sequence that first produced each row is what makes those the same.
func TestUsageRollupCapIsIndependentOfIncrementBatching(t *testing.T) {
	oneShot := func(t *testing.T, batches int) map[string]domain.DailyRollup {
		store := newTestHistory(t)
		overflow := domain.MaxRollupKeysPerDimension + 7
		batch := map[string]domain.DailyRollup{}
		flush := func(sequence uint64) {
			if len(batch) == 0 {
				return
			}
			if err := putRollupDelta(store, batch); err != nil {
				t.Fatal(err)
			}
			batch = map[string]domain.DailyRollup{}
		}
		size := overflow/batches + 1
		for index := 0; index < overflow; index++ {
			// Names are deliberately out of lexicographic order relative to the
			// sequence, so a cap that sorted by key would disagree with one that
			// sorts by ledger order.
			batch[providerModelKey(fmt.Sprintf("vendor/model-%03d", overflow-index))] = cappedIncrement(uint64(index+1), 10)
			if (index+1)%size == 0 {
				flush(uint64(index + 1))
			}
		}
		flush(uint64(overflow))
		return dimensionRows(t, store)
	}
	single := oneShot(t, 1)
	many := oneShot(t, 13)
	if len(single) != len(many) {
		t.Fatalf("one pass stored %d keys, batched stored %d", len(single), len(many))
	}
	for key, row := range single {
		if other, exists := many[key]; !exists || other.Attempts != row.Attempts || other.CostMicrosUSD != row.CostMicrosUSD {
			t.Fatalf("%s: one pass=%#v batched=%#v", key, row, many[key])
		}
	}
}
