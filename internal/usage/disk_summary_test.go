package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func diskSummaryTestLimits() DiskSummaryLimits {
	return DiskSummaryLimits{Pages: 4096, SourceRows: 65536, SourceBytes: 64 << 20, OutputBytes: 8 << 20,
		ScratchReads: 1 << 20, ScratchReadBytes: 256 << 20, ScratchWriteBytes: 256 << 20, ScratchFileBytes: 64 << 20,
		Periods: 4096, PeriodBytes: 8 << 20, MaxDuration: time.Minute, Page: diskRollupTestLimits}
}
func diskSummaryTestQuery() DiskDailySummaryQuery {
	return DiskDailySummaryQuery{Start: "2026-10-01", End: "2026-10-03", Granularity: "day", GroupBy: domain.RollupDimensionRequestedModel, Sort: "cost", Order: "desc", Limit: 2}
}
func diskSummaryEvents() []ledger.Event {
	var events []ledger.Event
	for i, model := range []string{"a", "b", domain.RollupOtherKey, "a", "c", "d", domain.RollupOtherKey} {
		rows := diskFailureRequest(fmt.Sprint("summary_", i), "success", 1, false, true)
		for j := range rows {
			rows[j].PeriodID = fmt.Sprintf("2026-10-%02d", i%3+1)
			rows[j].PeriodTimezoneVersion = uint64([]int{0, 2, 10}[i%3])
			rows[j].RequestedModel = model
			start := time.Date(2026, 10, i%3+1, 0, 0, 0, 0, time.UTC)
			rows[j].PeriodStartMicros = start.UnixMicro()
			rows[j].PeriodEndMicros = start.Add(24 * time.Hour).UnixMicro()
			if rows[j].Kind == ledger.EventAttemptSettled {
				rows[j].ProviderInputTokens = int64(i + 1)
				rows[j].ProviderOutputTokens = 3
				rows[j].LatencyMillis = int64(i+1) * 100
			}
		}
		events = append(events, rows...)
	}
	return events
}
func readDiskSummary(t *testing.T, p *DiskProjection, q DiskDailySummaryQuery, l DiskSummaryLimits) DiskSummary {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scratch")
	got, err := p.ReadSummary(context.Background(), q, l, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("scratch survived", err)
	}
	if len(p.summaries) != 0 || len(p.readers) != 0 || p.db.Stats().OpenTxN != 0 {
		t.Fatal("query ownership leaked")
	}
	return got
}
func TestDiskDailySummaryNativeFoldAndSpans(t *testing.T) {
	f := newDiskFailureFixture(t, diskSummaryEvents())
	q, limits := diskSummaryTestQuery(), diskSummaryTestLimits()
	for _, granularity := range []string{"day", "month", "year"} {
		q.Granularity = granularity
		got := readDiskSummary(t, f.p, q, limits)
		p := got.Payload
		if got.Head != f.a.Watermark() || p.WatermarkSequence != got.Head.Sequence || p.Totals.Attempts != 7 || *p.Totals.Requests != 7 || p.Totals.InputTokens != 28 || p.Totals.CostMicrosUSD != 49 {
			t.Fatal("native totals", got)
		}
		wantBuckets := 1
		if granularity == "day" {
			wantBuckets = 3
		}
		if len(p.Buckets) != wantBuckets || len(p.TimezoneChanges) != 2 || p.TimezoneChanges[0].FromVersion != 0 || p.TimezoneChanges[1].ToVersion != 10 {
			t.Fatal("bucket/timezone identity", p)
		}
		if p.Buckets[0].Start != time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) {
			t.Fatal("span start", p.Buckets)
		}
		if *p.GroupsTruncated != true || *p.GroupsOtherCount != 3 || len(*p.Groups) != 2 {
			t.Fatal("other was duplicated", p)
		}
		var calls, cost int64
		for _, group := range *p.Groups {
			calls += *group.Requests
			cost += group.CostMicrosUSD
		}
		if calls != 7 || cost != 49 || (*p.Groups)[0].Key != domain.RollupOtherKey || (*p.Groups)[0].CostMicrosUSD != 35 {
			t.Fatal("kept other tail conservation/rank", *p.Groups)
		}
	}
	q = diskSummaryTestQuery()
	q.Order = "asc"
	q.Limit = 1
	got := readDiskSummary(t, f.p, q, limits)
	if len(*got.Payload.Groups) != 2 || (*got.Payload.Groups)[0].Key != "b" || (*got.Payload.Groups)[1].Key != domain.RollupOtherKey {
		t.Fatal("unkept other fold", got.Payload)
	}
	q.GroupBy = domain.RollupDimensionProvider
	got = readDiskSummary(t, f.p, q, limits)
	if got.Payload.Totals.Requests == nil || (*got.Payload.Groups)[0].Requests != nil {
		t.Fatal("provider request metrics fabricated")
	}
	q.GroupBy = ""
	q.FilterDimension = domain.RollupDimensionProvider
	q.FilterValue = "provider_a"
	got = readDiskSummary(t, f.p, q, limits)
	if got.Payload.Totals.Requests != nil || got.Payload.Totals.Attempts != 7 || got.Payload.Filter.Value != "provider_a" {
		t.Fatal("filtered provider", got.Payload)
	}
}
func TestDiskDailySummaryWholeJobLimitsAndZeroFailure(t *testing.T) {
	f := newDiskFailureFixture(t, diskSummaryEvents())
	q, limits := diskSummaryTestQuery(), diskSummaryTestLimits()
	good := readDiskSummary(t, f.p, q, limits)
	mutations := map[string]func(*DiskSummaryLimits){
		"pages":               func(l *DiskSummaryLimits) { l.Pages = good.Pages - 1 },
		"source rows":         func(l *DiskSummaryLimits) { l.SourceRows = good.SourceRows - 1 },
		"source bytes":        func(l *DiskSummaryLimits) { l.SourceBytes = good.SourceBytes - 1 },
		"output":              func(l *DiskSummaryLimits) { l.OutputBytes = good.OutputBytes - 1 },
		"scratch reads":       func(l *DiskSummaryLimits) { l.ScratchReads = good.ScratchReads - 1 },
		"scratch read bytes":  func(l *DiskSummaryLimits) { l.ScratchReadBytes = good.ScratchReadBytes - 1 },
		"scratch write bytes": func(l *DiskSummaryLimits) { l.ScratchWriteBytes = good.ScratchWriteBytes - 1 },
		"period count":        func(l *DiskSummaryLimits) { l.Periods = 2 },
		"period bytes":        func(l *DiskSummaryLimits) { l.PeriodBytes = 3*(len(q.Start)+16) - 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			l := limits
			mutate(&l)
			path := filepath.Join(t.TempDir(), "scratch")
			got, err := f.p.ReadSummary(context.Background(), q, l, path)
			if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskSummary{}) {
				t.Fatal("partial result escaped", got, err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("scratch leaked", err)
			}
		})
	}
	// A later selected row fails after earlier source pages were reduced.
	if err := f.p.db.Update(func(tx *bbolt.Tx) error {
		c := tx.Bucket([]byte("rollup_versions")).Cursor()
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			var version diskRollupVersion
			if json.Unmarshal(raw, &version) != nil {
				return ErrDiskUsageCorrupt
			}
			if version.Key.Dimension == domain.RollupDimensionTotal && version.Key.PeriodID == "2026-10-03" && version.NextSequence == 0 {
				return c.Bucket().Put(k, []byte("invalid"))
			}
		}
		return errors.New("fault target missing")
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "corrupt")
	got, err := f.p.ReadSummary(context.Background(), q, limits, path)
	if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(got, DiskSummary{}) {
		t.Fatal("corruption produced a partial response", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("failed job scratch leaked", err)
	}
}
func TestDiskDailySummaryWideKeysAndPhysicalScratchCap(t *testing.T) {
	rows := diskFailureRequest("wide_summary", "success", 1, false, true)
	wide := strings.Repeat("w", 40<<10)
	for i := range rows {
		rows[i].RequestedModel = wide
	}
	f := newDiskFailureFixture(t, rows)
	q, limits := diskSummaryTestQuery(), diskSummaryTestLimits()
	got := readDiskSummary(t, f.p, q, limits)
	if (*got.Payload.Groups)[0].Key != wide {
		t.Fatal("group key truncated")
	}
	limits.ScratchFileBytes = 32768
	path := filepath.Join(t.TempDir(), "capped")
	got, err := f.p.ReadSummary(context.Background(), q, limits, path)
	if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskSummary{}) {
		t.Fatal("physical limit", got, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("physical failure leaked", err)
	}
}
func TestDiskDailySummaryAdmissionCancellationAndCloseJoin(t *testing.T) {
	for _, retiring := range []bool{false, true} {
		t.Run(fmt.Sprint(retiring), func(t *testing.T) {
			f := newDiskFailureFixture(t, diskFailureRequest("summary_join", "success", 1, false, true))
			q, l := diskSummaryTestQuery(), diskSummaryTestLimits()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "owned")
			f.p.mu.Lock()
			locked := true
			defer func() {
				if locked {
					f.p.mu.Unlock()
				}
			}()
			done := make(chan error, 1)
			go func() {
				result, err := f.p.ReadSummary(ctx, q, l, path)
				if !reflect.DeepEqual(result, DiskSummary{}) {
					done <- errors.New("partial query escaped")
					return
				}
				done <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for len(f.p.readers) != 1 {
				if time.Now().After(deadline) {
					t.Fatal("job never reached source")
				}
				time.Sleep(time.Millisecond)
			}
			unused := filepath.Join(t.TempDir(), "busy")
			if _, err := f.p.ReadSummary(context.Background(), q, l, unused); !errors.Is(err, ErrDiskUsageBusy) {
				t.Fatal("job queued", err)
			}
			if _, err := os.Lstat(unused); !os.IsNotExist(err) {
				t.Fatal("busy admission allocated disk")
			}
			closed := make(chan error, 1)
			want := context.Canceled
			if retiring {
				want = ErrDiskUsageClosed
				go func() { closed <- f.p.Close() }()
				for !f.p.retired.Load() {
					if time.Now().After(deadline) {
						t.Fatal("retirement not visible")
					}
					time.Sleep(time.Millisecond)
				}
				select {
				case err := <-closed:
					t.Fatal("Close escaped active job", err)
				default:
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				t.Fatal("ownership released before actual source exit", err)
			default:
			}
			if len(f.p.summaries) != 1 {
				t.Fatal("job permit released early")
			}
			f.p.mu.Unlock()
			locked = false
			if err := <-done; !errors.Is(err, want) {
				t.Fatal("join result", err)
			}
			if retiring {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("join preceded cleanup", err)
			}
			if len(f.p.summaries) != 0 || len(f.p.readers) != 0 {
				t.Fatal("join leaked ownership")
			}
		})
	}
}
func TestDiskDailySummaryPreservesExistingAndReplacedScratch(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("existing_summary", "success", 1, false, true))
	path := t.TempDir()
	marker := filepath.Join(path, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.ReadSummary(context.Background(), diskSummaryTestQuery(), diskSummaryTestLimits(), path); !os.IsExist(err) {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "keep" {
		t.Fatal("existing path changed", err)
	}
	// Direct adversarial ownership fault, not a normal query fixture.
	dir := t.TempDir()
	identity, _ := os.Lstat(dir)
	file := filepath.Join(dir, "summary.db")
	if err := os.WriteFile(file, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	fileIdentity, _ := os.Lstat(file)
	if err := os.Rename(file, file+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := summaryCleanup(dir, identity, fileIdentity, nil); err == nil {
		t.Fatal("replacement identity accepted")
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != "foreign" {
		t.Fatal("foreign file deleted", err)
	}
}
func TestDiskDailySummaryDerivedIntegerExtremes(t *testing.T) {
	// Arithmetic fixtures prove counter boundaries, not this history scale.
	row := domain.DailyRollup{Requests: math.MaxInt64, RequestErrors: 1, InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64}
	if summaryRank(row, "tokens", true) != uint64(math.MaxInt64)*2 || summaryRank(row, "success_rate", true) != 99999 {
		t.Fatal("rank overflow")
	}
	var histogram [domain.RollupLatencyBuckets]uint64
	histogram[0] = math.MaxInt64
	if p95, over := summaryApproxP95(histogram, 0); p95 != int64(LatencyBucketsMillis[0]) || over {
		t.Fatal("percentile overflow", p95, over)
	}
	if err := summarySubtract(&row, domain.DailyRollup{Requests: math.MaxInt64, InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64}); err != nil || row.Requests != 0 || row.InputTokens != 0 {
		t.Fatal("checked subtraction", row, err)
	}
	if err := summarySubtract(&row, domain.DailyRollup{Attempts: 1}); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("subtraction underflow", err)
	}
}

func TestDiskDailySummaryDerivedCrossPeriodOverflowFailsWholeJob(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 2; i++ {
		rows := diskFailureRequest(fmt.Sprint("overflow_", i), "success", 1, false, true)
		for j := range rows {
			rows[j].PeriodID = fmt.Sprintf("2026-10-%02d", i+1)
			rows[j].OccurredAt = rows[j].OccurredAt.Add(time.Duration(i) * 24 * time.Hour)
		}
		events = append(events, rows...)
	}
	f := newDiskFailureFixture(t, events)
	// Global native metrics reject this history before a query can see it.
	// Inject individually valid derived row counters to exercise the reducer's
	// independent cross-period overflow guard, not to claim a native scale.
	if err := f.p.db.Update(func(tx *bbolt.Tx) error {
		c := tx.Bucket([]byte("rollup_versions")).Cursor()
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			var version diskRollupVersion
			if err := json.Unmarshal(raw, &version); err != nil {
				return err
			}
			if version.Key.Dimension == domain.RollupDimensionTotal && version.NextSequence == 0 {
				version.Row.CostMicrosUSD = math.MaxInt64/2 + 1
				raw, err := json.Marshal(version)
				if err != nil {
					return err
				}
				if err := c.Bucket().Put(k, raw); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "overflow")
	got, err := f.p.ReadSummary(context.Background(), diskSummaryTestQuery(), diskSummaryTestLimits(), path)
	if err == nil || !strings.Contains(err.Error(), "overflow") || !reflect.DeepEqual(got, DiskSummary{}) {
		t.Fatal("cross period overflow wrapped", got, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("overflow scratch leaked", err)
	}
}

func TestDiskDailySummaryCancellationDuringCleanupFailsPublication(t *testing.T) {
	for _, retiring := range []bool{false, true} {
		t.Run(fmt.Sprint(retiring), func(t *testing.T) {
			f := newDiskFailureFixture(t, diskFailureRequest("publication", "success", 1, false, true))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, finish := make(chan struct{}), make(chan struct{})
			path := filepath.Join(t.TempDir(), "scratch")
			done := make(chan error, 1)
			go func() {
				got, err := f.p.readSummary(ctx, diskSummaryTestQuery(), diskSummaryTestLimits(), path, func(path string, dir, file os.FileInfo, db *bbolt.DB) error {
					close(entered)
					<-finish
					return summaryCleanup(path, dir, file, db)
				})
				if !reflect.DeepEqual(got, DiskSummary{}) {
					done <- errors.New("cancelled final payload escaped")
					return
				}
				done <- err
			}()
			<-entered
			closed := make(chan error, 1)
			want := context.Canceled
			if retiring {
				want = ErrDiskUsageClosed
				go func() { closed <- f.p.Close() }()
				deadline := time.Now().Add(3 * time.Second)
				for !f.p.retired.Load() {
					if time.Now().After(deadline) {
						t.Fatal("Close did not withdraw")
					}
					time.Sleep(time.Millisecond)
				}
			} else {
				cancel()
			}
			if len(f.p.summaries) != 1 {
				t.Fatal("cleanup lost job ticket")
			}
			select {
			case err := <-done:
				t.Fatal("cleanup did not join", err)
			default:
			}
			close(finish)
			if err := <-done; !errors.Is(err, want) {
				t.Fatal("publication cancellation", err)
			}
			if retiring {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("cleanup was skipped", err)
			}
		})
	}
}

func TestDiskDailySummaryEmptyValidationAndDeadline(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("empty_range", "success", 1, false, true))
	q, l := diskSummaryTestQuery(), diskSummaryTestLimits()
	q.Start, q.End = "2026-09-01", "2026-09-03"
	got := readDiskSummary(t, f.p, q, l)
	if len(got.Payload.Buckets) != 0 || got.Payload.Groups == nil || len(*got.Payload.Groups) != 0 || *got.Payload.GroupsTruncated || *got.Payload.GroupsOtherCount != 0 || *got.Payload.Totals.Requests != 0 {
		t.Fatal("empty shape", got.Payload)
	}
	q.GroupBy = ""
	got = readDiskSummary(t, f.p, q, l)
	if got.Payload.Groups != nil || got.Payload.GroupsTruncated != nil {
		t.Fatal("ungrouped shape")
	}
	q.GroupBy, q.FilterDimension, q.FilterValue = domain.RollupDimensionProject, domain.RollupDimensionProvider, "provider_a"
	path := filepath.Join(t.TempDir(), "invalid")
	if _, err := f.p.ReadSummary(context.Background(), q, l, path); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("cross dimension accepted", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("invalid query allocated scratch")
	}
	q = diskSummaryTestQuery()
	l.MaxDuration = time.Nanosecond
	got, err := f.p.ReadSummary(context.Background(), q, l, path)
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(got, DiskSummary{}) {
		t.Fatal("deadline", got, err)
	}
}

func TestDiskDailySummaryConcurrentNativeApplyKeepsOneHead(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 8; i++ {
		rows := diskFailureRequest(fmt.Sprint("initial_", i), "success", 1, false, true)
		for j := range rows {
			rows[j].RequestedModel = fmt.Sprint("model_", i)
		}
		events = append(events, rows...)
	}
	f := newDiskFailureFixture(t, events)
	writer := make(chan error, 1)
	go func() {
		for i := 0; i < 12; i++ {
			rows := diskFailureRequest(fmt.Sprint("growing_", i), "success", 1, false, true)
			for j := range rows {
				rows[j].RequestedModel = fmt.Sprint("new_model_", i)
			}
			for _, e := range rows {
				r, _, err := f.native.AppendRecordWithReceipt(context.Background(), e)
				if err == nil {
					err = f.d.Apply(r)
				}
				if err == nil {
					err = f.owner.Apply(context.Background(), r)
				}
				if err != nil {
					writer <- err
					return
				}
			}
		}
		writer <- nil
	}()
	q, l := diskSummaryTestQuery(), diskSummaryTestLimits()
	l.Page.ScanRows = 4
	for i := 0; i < 8; i++ {
		got := readDiskSummary(t, f.p, q, l)
		head := got.Head.Sequence
		if got.Payload.Totals.Attempts != int64((head+1)/5) || *got.Payload.Totals.Requests != int64(head/5) {
			t.Fatal("mixed source cuts", head, got.Payload.Totals)
		}
		var calls int64
		for _, g := range *got.Payload.Groups {
			calls += *g.Requests
		}
		if calls != *got.Payload.Totals.Requests {
			t.Fatal("mixed group cut")
		}
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
}
