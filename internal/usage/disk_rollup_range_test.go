package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

var diskRollupTestLimits = DiskUsagePageLimits{ScanRows: 16, InputBytes: 8 << 20, OutputBytes: 8 << 20}

func drainDiskRollups(t *testing.T, p *DiskProjection, q DiskRollupQuery, cursor DiskRollupCursor, limits DiskUsagePageLimits) ([]DiskRollupItem, ledger.Watermark, int) {
	t.Helper()
	items := make([]DiskRollupItem, 0)
	var head ledger.Watermark
	for calls := 1; calls <= 5000; calls++ {
		page, err := p.RollupPage(context.Background(), q, cursor, limits)
		if err != nil {
			t.Fatal("rollup page", calls, err)
		}
		if calls == 1 {
			head = page.Head
		} else if head != page.Head {
			t.Fatal("mixed rollup heads")
		}
		if page.Scanned > limits.ScanRows || page.InputBytes > limits.InputBytes || page.OutputBytes > limits.OutputBytes || len(p.readers) != 0 || p.db.Stats().OpenTxN != 0 {
			t.Fatal("page cost/reader leak", page)
		}
		items = append(items, page.Items...)
		if page.Done {
			return items, head, calls
		}
		if page.Next == cursor || page.Next == (DiskRollupCursor{}) {
			t.Fatal("no-progress cursor", page)
		}
		cursor = page.Next
	}
	t.Fatal("rollup scan did not terminate")
	return nil, head, 0
}
func diskRollupItemMap(t *testing.T, items []DiskRollupItem) map[domain.RollupKey]domain.DailyRollup {
	t.Helper()
	out := make(map[domain.RollupKey]domain.DailyRollup)
	for _, item := range items {
		if _, dup := out[item.Key]; dup {
			t.Fatal("duplicate association")
		}
		out[item.Key] = item.Row
	}
	return out
}
func TestDiskRollupRangeNativeDimensionsAndApproximateRows(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 12; i++ {
		rows := diskFailureRequest(fmt.Sprint("range_", i), "provider_error", 1, i%3 == 0, false)
		for j := range rows {
			rows[j].PeriodID = fmt.Sprintf("2026-10-%02d", i%3+1)
			rows[j].PeriodTimezoneVersion = uint64([]int{0, 2, 10}[i%3])
			rows[j].RequestedModel = fmt.Sprint("model_", i%2)
			if rows[j].Kind == ledger.EventAttemptSettled {
				rows[j].LatencyMillis = int64(i+1) * 1000
				rows[j].ProviderInputTokens = 9
				rows[j].TokenEstimated = i%3 == 0
			}
		}
		events = append(events, rows...)
	}
	f := newDiskFailureFixture(t, events)
	for _, dimension := range diskRollupDimensions {
		q := DiskRollupQuery{Start: "2026-10-01", End: "2026-10-03", Dimensions: []string{dimension}, Limit: 2}
		got, head, _ := drainDiskRollups(t, f.p, q, DiskRollupCursor{}, diskRollupTestLimits)
		want := make(map[domain.RollupKey]domain.DailyRollup)
		for key, row := range f.a.rollupDelta {
			if key.Dimension == dimension {
				want[key] = *row
			}
		}
		if head != f.a.Watermark() || !reflect.DeepEqual(diskRollupItemMap(t, got), want) {
			t.Fatal("native rollup differs", dimension)
		}
		for _, item := range got {
			if item.Sequence > head.Sequence {
				t.Fatal("future value")
			}
			if _, found, err := f.p.ReadRollup(context.Background(), item.Key); err != nil || !found {
				t.Fatal(err)
			}
		}
	}
	q := DiskRollupQuery{Start: "2026-10-01", End: "2026-10-03", Dimensions: []string{domain.RollupDimensionRequestedModel}, FilterValue: "model_1", Limit: 1}
	got, _, calls := drainDiskRollups(t, f.p, q, DiskRollupCursor{}, DiskUsagePageLimits{ScanRows: 3, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	if len(got) != 3 || calls < 3 {
		t.Fatal("sparse filter/continuation lost", len(got), calls)
	}
	for _, item := range got {
		if item.Key.DimensionKey != "model_1" {
			t.Fatal("filter mismatch")
		}
	}
}
func TestDiskRollupRangeCursorPinsMutableValuesAndExcludesNewRows(t *testing.T) {
	rows := diskFailureRequest("frozen_rollup", "success", 1, false, true)
	f := newDiskFailureFixture(t, rows[:4])
	q := DiskRollupQuery{Start: "2026-10-03", End: "2026-10-03", Dimensions: []string{domain.RollupDimensionTotal, domain.RollupDimensionProject}, Limit: 1}
	first, err := f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, diskRollupTestLimits)
	if err != nil || first.Done || len(first.Items) != 1 || first.Items[0].Key.Dimension != domain.RollupDimensionTotal {
		t.Fatal(first, err)
	}
	want := make(map[domain.RollupKey]domain.DailyRollup)
	for key, row := range f.a.rollupDelta {
		if key.Dimension == q.Dimensions[0] || key.Dimension == q.Dimensions[1] {
			want[key] = *row
		}
	}
	appendDiskDetailEvent(t, f, rows[4])
	more := diskFailureRequest("new_rollup", "success", 0, false, false)
	for _, e := range more {
		e.ProjectID = "new_project"
		appendDiskDetailEvent(t, f, e)
	}
	q.Limit = 3
	rest, head, _ := drainDiskRollups(t, f.p, q, first.Next, diskRollupTestLimits)
	all := append(first.Items, rest...)
	if head != first.Head || !reflect.DeepEqual(diskRollupItemMap(t, all), want) {
		t.Fatal("mutable/new rows crossed H")
	}
	fresh, freshHead, _ := drainDiskRollups(t, f.p, q, DiskRollupCursor{}, diskRollupTestLimits)
	if freshHead.Sequence != 7 || len(fresh) != 3 {
		t.Fatal("fresh query did not see updates", freshHead, len(fresh))
	}
	for _, item := range fresh {
		if item.Key.Dimension == domain.RollupDimensionTotal && item.Row.Requests != 2 {
			t.Fatal("missing finalized contribution")
		}
	}
}
func TestDiskRollupRangeMiddleVersionDeletionCannotFallBack(t *testing.T) {
	first := diskFailureRequest("version_one", "success", 1, false, true)
	second := diskFailureRequest("version_two", "success", 1, false, true)
	f := newDiskFailureFixture(t, append(first, second[:4]...))
	q := DiskRollupQuery{Start: "2026-10-03", End: "2026-10-03", Dimensions: []string{domain.RollupDimensionTotal, domain.RollupDimensionProject}, Limit: 1}
	page, err := f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, diskRollupTestLimits)
	if err != nil || page.Head.Sequence != 9 {
		t.Fatal(page, err)
	}
	appendDiskDetailEvent(t, f, second[4])
	key := domain.RollupKey{PeriodID: q.Start, TimezoneVersion: 1, Dimension: domain.RollupDimensionProject, DimensionKey: "project"}
	vk := diskRollupVersionKey(diskUsageID(key.Encode()), 9)
	var saved []byte
	if err := f.p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("rollup_versions"))
		saved = bytes.Clone(b.Get(vk))
		if saved == nil {
			t.Fatal("selected version absent in fixture")
		}
		return b.Delete(vk)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.p.RollupPage(context.Background(), q, page.Next, diskRollupTestLimits)
	if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(got, DiskRollupPage{}) {
		t.Fatal("Seek fell back over a gap", got, err)
	}
	if err := f.p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("rollup_versions")).Put(vk, saved) }); err != nil {
		t.Fatal(err)
	}
	items, head, _ := drainDiskRollups(t, f.p, q, page.Next, diskRollupTestLimits)
	if head.Sequence != 9 || len(items) != 1 || items[0].Row.Requests != 1 || items[0].Row.Attempts != 2 {
		t.Fatal("repaired old cursor changed", head, items)
	}
}
func TestDiskRollupRangeFaultsAndNoPartialPage(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("range_fault", "provider_error", 1, false, false))
	q := DiskRollupQuery{Start: "2026-10-03", End: "2026-10-03", Dimensions: []string{domain.RollupDimensionTotal, domain.RollupDimensionProject}, Limit: 100}
	key := domain.RollupKey{PeriodID: q.Start, TimezoneVersion: 1, Dimension: domain.RollupDimensionProject, DimensionKey: "project"}
	rk := diskRollupRangeKey(key)
	vk := diskRollupVersionKey(rk[19:], 5)
	saved := diskDashboardContents(t, f.p)
	for _, fault := range []string{"missing_version", "scope", "negative_counter", "histogram_count", "bounds_future", "bad_bounds", "successor"} {
		t.Run(fault, func(t *testing.T) {
			if err := f.p.db.Update(func(tx *bbolt.Tx) error {
				if fault == "missing_version" {
					return tx.Bucket([]byte("rollup_versions")).Delete(vk)
				}
				if fault == "bad_bounds" {
					return tx.Bucket([]byte("rollup_range")).Put(rk, []byte{1})
				}
				if fault == "bounds_future" {
					b := make([]byte, 16)
					copy(b, diskUsageSequence(4))
					copy(b[8:], diskUsageSequence(999))
					return tx.Bucket([]byte("rollup_range")).Put(rk, b)
				}
				v, _, err := diskUsageGet[diskRollupVersion](tx, "rollup_versions", vk)
				if err != nil {
					return err
				}
				switch fault {
				case "scope":
					v.Key.DimensionKey = "foreign"
				case "negative_counter":
					v.Row.CostMicrosUSD = -1
				case "histogram_count":
					v.Row.AttemptLatencyBuckets[0]++
				case "successor":
					v.NextSequence = 6
				}
				return diskUsagePut(tx, "rollup_versions", vk, v)
			}); err != nil {
				t.Fatal(err)
			}
			got, err := f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, diskRollupTestLimits)
			if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(got, DiskRollupPage{}) || len(f.p.readers) != 0 || f.p.db.Stats().OpenTxN != 0 {
				t.Fatal("partial successful page/leak", got, err)
			}
			if err := f.p.db.Update(func(tx *bbolt.Tx) error {
				for _, name := range diskUsageBuckets {
					for k, v := range saved[name] {
						if err := tx.Bucket([]byte(name)).Put([]byte(k), v); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	// One budget row admits an index but cannot hydrate its matching version.
	got, err := f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, DiskUsagePageLimits{ScanRows: 1, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskRollupPage{}) {
		t.Fatal("no-progress cursor", got, err)
	}
	got, err = f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, DiskUsagePageLimits{ScanRows: 16, InputBytes: 20, OutputBytes: 8 << 20})
	if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskRollupPage{}) {
		t.Fatal(got, err)
	}
	got, err = f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, DiskUsagePageLimits{ScanRows: 16, InputBytes: 8 << 20, OutputBytes: 1})
	if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskRollupPage{}) {
		t.Fatal(got, err)
	}
	// Output exhaustion after progress preserves the unconsumed association.
	full, err := f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, diskRollupTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(full.Items[0])
	small := diskRollupTestLimits
	small.OutputBytes = len(raw)
	partial, err := f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, small)
	if err != nil || len(partial.Items) != 1 || partial.Done {
		t.Fatal(partial, err)
	}
	rest, _, _ := drainDiskRollups(t, f.p, q, partial.Next, diskRollupTestLimits)
	if !reflect.DeepEqual(append(partial.Items, rest...), full.Items) {
		t.Fatal("output stop lost a row")
	}
}
func TestDiskRollupHistoryAndScopeMissingRejectWholeApply(t *testing.T) {
	for _, fault := range []string{"range", "head_version", "current_row", "count", "scope"} {
		t.Run(fault, func(t *testing.T) {
			first := diskFailureRequest("scope_first", "success", 1, false, true)
			second := diskFailureRequest("scope_second", "success", 1, false, true)
			if fault == "count" || fault == "scope" {
				for i := range second {
					second[i].RequestedModel = "new_model"
				}
			}
			f := newDiskFailureFixture(t, append(first, second[:3]...))
			key := domain.RollupKey{PeriodID: first[0].PeriodID, TimezoneVersion: 1, Dimension: domain.RollupDimensionRequestedModel, DimensionKey: "chat"}
			rk := diskRollupRangeKey(key)
			vk := diskRollupVersionKey(rk[19:], 5)
			scope := diskUsageID(domain.RollupDimensionPrefix(key.PeriodID, 1, key.Dimension))
			if err := f.p.db.Update(func(tx *bbolt.Tx) error {
				switch fault {
				case "range":
					return tx.Bucket([]byte("rollup_range")).Delete(rk)
				case "head_version":
					return tx.Bucket([]byte("rollup_versions")).Delete(vk)
				case "current_row":
					return tx.Bucket([]byte("rollup")).Delete(diskUsageID(key.Encode()))
				case "count":
					return tx.Bucket([]byte("rollup_counts")).Delete(scope)
				default:
					return tx.Bucket([]byte("rollup_scope")).Delete(scope)
				}
			}); err != nil {
				t.Fatal(err)
			}
			before := diskDashboardContents(t, f.p)
			record, _, err := f.native.AppendRecordWithReceipt(context.Background(), second[3])
			if err != nil {
				t.Fatal(err)
			}
			if err := f.d.Apply(record); !errors.Is(err, ErrDiskUsageCorrupt) {
				t.Fatal("silently reinitialized missing history", err)
			}
			if !reflect.DeepEqual(before, diskDashboardContents(t, f.p)) {
				t.Fatal("partial publication after missing history")
			}
		})
	}
}
func TestDiskRollupRangeWideIdentityAndFullLabelBounds(t *testing.T) {
	rows := diskFailureRequest("wide_rollup", "success", 1, false, true)
	period := "2026-10-03_" + strings.Repeat("p", 40<<10)
	model := strings.Repeat("m", 40<<10)
	for i := range rows {
		rows[i].PeriodID = period
		rows[i].RequestedModel = model
	}
	f := newDiskFailureFixture(t, rows)
	q := DiskRollupQuery{Start: "2026-10-01", End: "2026-10-31", Dimensions: []string{domain.RollupDimensionRequestedModel}, FilterValue: model, Limit: 100}
	items, _, _ := drainDiskRollups(t, f.p, q, DiskRollupCursor{}, diskRollupTestLimits)
	if len(items) != 1 || items[0].Key.PeriodID != period || items[0].Key.DimensionKey != model {
		t.Fatal("wide fixed index identity changed")
	}
	limits := diskRollupTestLimits
	limits.InputBytes = 4096
	page, err := f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, limits)
	// Earlier non-selected associations may consume a bounded empty page.
	if err == nil && len(page.Items) == 0 && !page.Done {
		page, err = f.p.RollupPage(context.Background(), q, page.Next, limits)
	}
	if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(page, DiskRollupPage{}) {
		t.Fatal(page, err)
	}
	q.End = "2026-10-03"
	items, _, _ = drainDiskRollups(t, f.p, q, DiskRollupCursor{}, diskRollupTestLimits)
	if len(items) != 0 {
		t.Fatal("prefix treated as complete inclusive label")
	}
	// A corrupt out-of-range old row is never scanned/decoded.
	q.Start = "2026-10-04"
	q.End = "2026-10-31"
	if err := f.p.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("rollup_range")).Put(diskRollupRangeKey(domain.RollupKey{PeriodID: period, TimezoneVersion: 1, Dimension: q.Dimensions[0], DimensionKey: model}), []byte("bad"))
	}); err != nil {
		t.Fatal(err)
	}
	items, _, _ = drainDiskRollups(t, f.p, q, DiskRollupCursor{}, diskRollupTestLimits)
	if len(items) != 0 {
		t.Fatal(items)
	}
}
func TestDiskRollupRangeFirstTwoHundredAndOtherRemainExact(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 203; i++ {
		rows := diskFailureRequest(fmt.Sprint("fold_", i), "success", 1, false, true)
		for j := range rows {
			rows[j].RequestedModel = fmt.Sprintf("model_%03d", i)
		}
		events = append(events, rows...)
	}
	f := newDiskFailureFixture(t, events)
	q := DiskRollupQuery{Start: "2026-10-03", End: "2026-10-03", Dimensions: []string{domain.RollupDimensionRequestedModel}, Limit: 7}
	items, _, _ := drainDiskRollups(t, f.p, q, DiskRollupCursor{}, DiskUsagePageLimits{ScanRows: 20, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	got := diskRollupItemMap(t, items)
	want := make(map[domain.RollupKey]domain.DailyRollup)
	var other domain.DailyRollup
	for key, row := range f.a.rollupDelta {
		if key.Dimension != q.Dimensions[0] {
			continue
		}
		if key.DimensionKey < "model_200" {
			want[key] = *row
		} else {
			if err := other.Add(*row); err != nil {
				t.Fatal(err)
			}
		}
	}
	want[domain.RollupKey{PeriodID: q.Start, TimezoneVersion: 1, Dimension: q.Dimensions[0], DimensionKey: domain.RollupOtherKey}] = other
	if len(got) != 201 || !reflect.DeepEqual(got, want) {
		t.Fatal("deterministic admission fold changed", len(got))
	}
}
func TestDiskRollupRangeCursorWidthBusyCancelAndRetirement(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("range_life", "success", 1, false, true))
	q := DiskRollupQuery{Start: "2026-10-03", End: "2026-10-03", Dimensions: []string{domain.RollupDimensionTotal, domain.RollupDimensionProject}, Limit: 1}
	first, err := f.p.RollupPage(context.Background(), q, DiskRollupCursor{}, diskRollupTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	changed := q
	changed.End = "2026-10-04"
	page, err := f.p.RollupPage(context.Background(), changed, first.Next, diskRollupTestLimits)
	if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(page, DiskRollupPage{}) {
		t.Fatal(page, err)
	}
	foreign := newDiskFailureFixture(t, diskFailureRequest("foreign_range", "success", 1, false, true))
	page, err = foreign.p.RollupPage(context.Background(), q, first.Next, diskRollupTestLimits)
	if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(page, DiskRollupPage{}) {
		t.Fatal(page, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err = f.p.RollupPage(ctx, q, DiskRollupCursor{}, diskRollupTestLimits)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.p.read(context.Background(), func(*bbolt.DB) error { entered <- struct{}{}; <-release; return nil })
		}()
	}
	<-entered
	<-entered
	wide := q
	wide.Dimensions = []string{domain.RollupDimensionProject}
	wide.FilterValue = strings.Repeat("<", 200000)
	page, err = f.p.RollupPage(context.Background(), wide, DiskRollupCursor{}, diskRollupTestLimits)
	if !errors.Is(err, ErrDiskUsageBusy) || !reflect.DeepEqual(page, DiskRollupPage{}) {
		t.Fatal(page, err)
	}
	close(release)
	wg.Wait()
	page, err = f.p.RollupPage(context.Background(), wide, DiskRollupCursor{}, diskRollupTestLimits)
	if !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("encoded query gate", err)
	}
	invalid := q
	invalid.Dimensions = []string{domain.RollupDimensionTotal}
	invalid.FilterValue = "cross_term"
	page, err = f.p.RollupPage(context.Background(), invalid, DiskRollupCursor{}, diskRollupTestLimits)
	if !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
	invalid = q
	invalid.Start = "2000-01-01"
	page, err = f.p.RollupPage(context.Background(), invalid, DiskRollupCursor{}, diskRollupTestLimits)
	if !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	page, err = f.p.RollupPage(context.Background(), q, first.Next, diskRollupTestLimits)
	if !errors.Is(err, ErrDiskUsageClosed) || !reflect.DeepEqual(page, DiskRollupPage{}) {
		t.Fatal(page, err)
	}
	if len(f.p.readers) != 0 {
		t.Fatal("retired reader leaked")
	}
}
