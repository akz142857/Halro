package usage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func TestDiskRollupSnapshotCompleteAcrossPagesAndLabels(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 35; i++ {
		rows := diskFailureRequest(fmt.Sprint("snapshot_", i), "provider_error", 1, i%3 == 0, false)
		for j := range rows {
			rows[j].RequestedModel = fmt.Sprint("model_", i)
			rows[j].PeriodID = []string{"2001-01-01", "2026-10-03", "historical-legacy-label"}[i%3]
			rows[j].PeriodTimezoneVersion = uint64(i%2 + 1)
		}
		events = append(events, rows...)
	}
	f := newDiskFailureFixture(t, events)
	want := map[domain.RollupKey]domain.DailyRollup{}
	for key, row := range f.a.rollupDelta {
		want[key] = *row
	}
	got := map[domain.RollupKey]domain.DailyRollup{}
	var cursor DiskRollupSnapshotCursor
	calls := 0
	for {
		page, err := f.p.RollupSnapshotPage(context.Background(), cursor, DiskUsagePageLimits{ScanRows: 3, InputBytes: 8 << 20, OutputBytes: 8 << 20})
		if err != nil {
			t.Fatal(err)
		}
		calls++
		if len(page.Rows) > 3 || page.Head != f.a.Watermark() || len(f.p.readers) != 0 || f.p.db.Stats().OpenTxN != 0 {
			t.Fatal("unbounded page or retained reader", page)
		}
		for _, row := range page.Rows {
			if _, dup := got[row.Key]; dup {
				t.Fatal("duplicate key")
			}
			got[row.Key] = row.Row
		}
		if page.Done {
			break
		}
		if page.Next == cursor {
			t.Fatal("no progress")
		}
		cursor = page.Next
	}
	if calls < 2 || !reflect.DeepEqual(got, want) {
		t.Fatal("incomplete all-history rollups", calls, len(got), len(want))
	}
}

func TestDiskRollupSnapshotRejectsMissingRowsInsteadOfEmptySuccess(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("snapshot_missing", "provider_error", 1, false, false))
	limits := DiskUsagePageLimits{ScanRows: 1, InputBytes: 8 << 20, OutputBytes: 8 << 20}
	page, err := f.p.RollupSnapshotPage(context.Background(), DiskRollupSnapshotCursor{}, limits)
	if err != nil || page.Done {
		t.Fatal(page, err)
	}
	// Remove the remaining physical rows while retaining the transactionally
	// recorded expected cardinality. Native head alone cannot detect this fault.
	if err := f.p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("rollup"))
		c := b.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if err := c.Delete(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.p.RollupSnapshotPage(context.Background(), page.Next, limits); !errors.Is(err, ErrDiskUsageCorrupt) || len(got.Rows) != 0 {
		t.Fatal("missing remaining rows became Done", got, err)
	}
	if got, err := f.p.RollupSnapshotPage(context.Background(), DiskRollupSnapshotCursor{}, limits); !errors.Is(err, ErrDiskUsageCorrupt) || len(got.Rows) != 0 {
		t.Fatal("deleted bucket contents became empty source", got, err)
	}
}

func TestDiskRollupSnapshotRefusesChangedOwnerAndLimits(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("snapshot_cursor", "provider_error", 1, false, false))
	limits := DiskUsagePageLimits{ScanRows: 1, InputBytes: 8 << 20, OutputBytes: 8 << 20}
	first, err := f.p.RollupSnapshotPage(context.Background(), DiskRollupSnapshotCursor{}, limits)
	if err != nil || first.Done {
		t.Fatal(first, err)
	}
	other := newDiskFailureFixture(t, diskFailureRequest("snapshot_other", "provider_error", 1, false, false))
	if page, err := other.p.RollupSnapshotPage(context.Background(), first.Next, limits); !errors.Is(err, ErrDiskUsageCursor) || len(page.Rows) != 0 {
		t.Fatal(page, err)
	}
	for _, e := range diskFailureRequest("snapshot_new", "provider_error", 1, false, false) {
		appendDiskDetailEvent(t, f, e)
	}
	if page, err := f.p.RollupSnapshotPage(context.Background(), first.Next, limits); !errors.Is(err, ErrDiskUsageCursor) || len(page.Rows) != 0 {
		t.Fatal(page, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if page, err := f.p.RollupSnapshotPage(ctx, DiskRollupSnapshotCursor{}, limits); !errors.Is(err, context.Canceled) || len(page.Rows) != 0 {
		t.Fatal(page, err)
	}
	limits.OutputBytes = 1
	if page, err := f.p.RollupSnapshotPage(context.Background(), DiskRollupSnapshotCursor{}, limits); !errors.Is(err, ErrDiskUsageLimit) || len(page.Rows) != 0 {
		t.Fatal(page, err)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.RollupSnapshotPage(context.Background(), DiskRollupSnapshotCursor{}, diskTestLimits); err == nil {
		t.Fatal("closed owner readable")
	}
}
