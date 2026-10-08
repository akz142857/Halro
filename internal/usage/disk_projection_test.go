package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

var diskTestLimits = DiskUsagePageLimits{ScanRows: 16, InputBytes: 8 << 20, OutputBytes: 8 << 20}

func TestDiskUsageProjectionPreservesReducerSemantics(t *testing.T) {
	for _, late := range []bool{false, true} {
		for _, cost := range []string{"known", "free", "unknown"} {
			t.Run(fmt.Sprint(late, "-", cost), func(t *testing.T) {
				ctx := context.Background()
				rows := finalizationFixture(late, true)
				for i := range rows {
					if rows[i].Event.Kind == ledger.EventAttemptSettled {
						if cost == "free" {
							rows[i].Event.PriceSnapshot = usagePriceSnapshot(t, domain.BillingModeFree, rows[i].Event.OccurredAt)
							rows[i].Event.LeaseMode = ledger.LeaseModeFree
							rows[i].Event.CommittedMicrosUSD = ledger.MicrosUSD(0)
						}
						if cost == "unknown" {
							s := domain.NewUnknownPriceSnapshot(rows[i].Event.OccurredAt)
							rows[i].Event.PriceSnapshot = &s
							rows[i].Event.LeaseMode = ledger.LeaseModeUnknownAllowed
							rows[i].Event.CommittedMicrosUSD = nil
						}
					}
				}
				reader := fixtureFinalReader(rows)
				p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), reader)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Close() })
				a, err := NewAggregateWithFinalizationReader(reader)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					if err := p.ApplyContext(ctx, r); err != nil {
						t.Fatal(err)
					}
					if err := a.Apply(r); err != nil {
						t.Fatal(err)
					}
					s, err := p.ReadStats(ctx)
					if err != nil || s.Watermark != a.Watermark() || s.Totals != a.Snapshot().Totals || s.Metrics != a.Metrics() {
						t.Fatal("disk/reference divergence", s, a.Snapshot(), err)
					}
				}
				attempts, err := p.AttemptPage(ctx, AttemptQuery{Limit: 100}, DiskUsageCursor{}, diskTestLimits)
				if err != nil || !attempts.Done || !reflect.DeepEqual(attempts.Items, a.Snapshot().Attempts) {
					t.Fatal(attempts, err)
				}
				summaries, err := p.SummaryPage(ctx, DiskSummaryQuery{Limit: 100}, DiskUsageCursor{}, diskTestLimits)
				if err != nil || !summaries.Done || !reflect.DeepEqual(summaries.Items, a.Snapshot().Requests) {
					t.Fatal(summaries, err)
				}
				detail, err := p.RequestDetailPage(ctx, DiskRequestDetailQuery{RequestID: "req", Limit: 100}, DiskRequestDetailCursor{}, diskTestLimits)
				wantDetail, exists := a.RequestDetail("req")
				if err != nil || !exists || !detail.Found || !detail.Done || detail.Summary != wantDetail.Summary || !reflect.DeepEqual(detail.Attempts, wantDetail.Attempts) {
					t.Fatal("detail cost/late semantics diverged", detail, wantDetail, err)
				}
				for key, want := range a.rollupDelta {
					got, found, err := p.ReadRollup(ctx, key)
					if err != nil || !found || got != *want {
						t.Fatal(key, got, *want, err)
					}
				}
				request, found, err := p.ReadRequest(ctx, "req")
				if err != nil || !found || request != a.Snapshot().Requests[0] {
					t.Fatal(request, found, err)
				}
				before, _ := p.ReadStats(ctx)
				if err := p.Apply(rows[len(rows)-1]); err != nil {
					t.Fatal("same native record retry", err)
				}
				after, _ := p.ReadStats(ctx)
				if before != after {
					t.Fatal("same record counted twice")
				}
				var settlement ledger.Record
				for _, r := range rows {
					if r.Event.Kind == ledger.EventAttemptSettled {
						settlement = r
					}
				}
				settlement.Sequence = 6
				settlement.Offset = 600
				if err := p.Apply(settlement); err != nil {
					t.Fatal("duplicate historical ID", err)
				}
				after, err = p.ReadStats(ctx)
				before.Watermark = after.Watermark
				if err != nil || before != after || after.Watermark.Sequence != 6 {
					t.Fatal("duplicate changed statistics", before, after, err)
				}
			})
		}
	}
}

func TestDiskUsageDerivativeUsesRealNativeAndSelectedDiskOwner(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x61}, 32)
	native, err := ledger.OpenWithOptions(filepath.Join(root, "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	rows := finalizationFixture(true, false)
	for _, r := range rows {
		if _, err := native.Append(ctx, r.Event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := native.Append(ctx, rows[4].Event); err != nil {
		t.Fatal(err)
	}
	owner, _, err := native.BuildProjectionFromCursor(ctx, 0, 0, filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	d, p, err := NewDiskLedgerDerivative(owner, filepath.Join(root, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); _ = p.Close() })
	if d.Aggregate() != nil {
		t.Fatal("disk bridge retained historical Aggregate")
	}
	if anchor, err := p.ReadReplayAnchor(ctx); err != nil || anchor != (ledger.ReplayAnchor{}) {
		t.Fatal("fresh Usage manufactured replay authority", anchor, err)
	}
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
		if err := d.Apply(r); err != nil {
			return err
		}
		anchor, err := p.ReadReplayAnchor(ctx)
		want := ledger.ReplayAnchor{Head: ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash, Epoch: r.Epoch}
		if err != nil || anchor != want {
			return fmt.Errorf("committed Usage/native cut differs: got %+v want %+v: %w", anchor, want, err)
		}
		if d.currentID != "" {
			return errors.New("borrowed ID retained")
		}
		return owner.Apply(ctx, r)
	}); err != nil {
		t.Fatal(err)
	}
	s, err := p.ReadStats(ctx)
	if err != nil || s.Watermark.Sequence != 6 || s.Totals.Requests != 1 || s.Totals.Attempts != 1 || s.Totals.CostMicrosUSD != 7 || s.Metrics.ActiveRequests != 0 {
		t.Fatal(s, err)
	}
	r, found, err := p.ReadRequest(ctx, "req")
	if err != nil || !found || r.Attempts != 0 {
		t.Fatal("late settlement changed summary", r, err)
	}
	anchor, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.ReplayAuthenticated(ctx, anchor, func(ledger.Record) error {
		return errors.New("committed cut replayed an already applied record")
	}); err != nil {
		t.Fatal("Usage cut cannot resume actual authenticated native source", err)
	}
	d.Close()
	if d.disk != nil || d.owner != nil {
		t.Fatal("retired bridge retained owners")
	}
	if err := d.Apply(rows[0]); !errors.Is(err, ErrLedgerDerivativeRetired) {
		t.Fatal(err)
	}
	if err := p.Apply(rows[0]); !errors.Is(err, ErrLedgerDerivativeRetired) {
		t.Fatal("direct alias bypassed retired reader", err)
	}
}

func diskUsageDigest(t *testing.T, p *DiskProjection) [32]byte {
	t.Helper()
	h := sha256.New()
	err := p.db.View(func(tx *bbolt.Tx) error {
		for _, name := range diskUsageBuckets {
			h.Write([]byte(name))
			if err := tx.Bucket([]byte(name)).ForEach(func(k, v []byte) error { h.Write(k); h.Write(v); return nil }); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func TestDiskUsageProjectionFailuresRollBackWholeTransaction(t *testing.T) {
	ctx := context.Background()
	rows := finalizationFixture(false, false)
	for _, reason := range []string{"rollup-overflow", "metrics-overflow", "reader-error", "cancel", "physical-retry"} {
		t.Run(reason, func(t *testing.T) {
			p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), fixtureFinalReader(rows))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			for _, r := range rows[:3] {
				if err := p.Apply(r); err != nil {
					t.Fatal(err)
				}
			}
			if reason == "rollup-overflow" {
				err = p.db.Update(func(tx *bbolt.Tx) error {
					row := domain.DailyRollup{CostMicrosUSD: math.MaxInt64}
					e := rows[3].Event
					row.Identity(e.PeriodID, e.PeriodTimezoneVersion, e.PeriodTimezone, e.PeriodStartMicros, e.PeriodEndMicros)
					key := domain.RollupKey{PeriodID: e.PeriodID, TimezoneVersion: e.PeriodTimezoneVersion, Dimension: domain.RollupDimensionTotal, DimensionKey: domain.RollupTotalKey}
					return diskUsagePut(tx, "rollup", diskUsageID(key.Encode()), diskRollupRow{key, row})
				})
			}
			if reason == "metrics-overflow" {
				err = p.db.Update(func(tx *bbolt.Tx) error {
					s, _, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
					if err != nil {
						return err
					}
					s.Metrics.AttemptsError = math.MaxUint64
					return diskUsagePut(tx, "meta", []byte("state"), s)
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			callCtx := ctx
			record := rows[3]
			if reason == "reader-error" {
				p.reader = eventPositionReader{p.reader, func(context.Context, string) (ledger.Watermark, bool, error) {
					return ledger.Watermark{}, false, errors.New("IO")
				}}
			}
			if reason == "cancel" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				callCtx = c
			}
			if reason == "physical-retry" {
				record = rows[2]
				record.Offset++
			}
			before := diskUsageDigest(t, p)
			if err := p.ApplyContext(callCtx, record); err == nil {
				t.Fatal("failure accepted", reason)
			}
			if got := diskUsageDigest(t, p); got != before {
				t.Fatal("failed transaction changed rows/index/head", reason)
			}
		})
	}
}

// A computed reader keeps no historical slice/map; it is only a unit fixture.
// Actual MAC WAL/selected owner authorization is checked separately above.
func diskGrowthReader() FinalizationReader {
	return eventPositionReader{testFinalizationReader{finalizationReaderFunc: finalizationReaderFunc(func(context.Context, string) (ledger.Record, bool, error) { return ledger.Record{}, false, nil })}, func(ctx context.Context, id string) (ledger.Watermark, bool, error) {
		if err := ctx.Err(); err != nil {
			return ledger.Watermark{}, false, err
		}
		seq, err := strconv.ParseUint(strings.TrimPrefix(id, "disk_e_"), 10, 64)
		if err != nil {
			return ledger.Watermark{}, false, err
		}
		return ledger.Watermark{Generation: 1, Sequence: seq, Offset: int64(seq * 100)}, true, nil
	}}
}
func diskGrowthRequest(t *testing.T, p *DiskProjection, i int) {
	t.Helper()
	rows := finalizationFixture(false, false)
	for j, r := range rows {
		r.Sequence = uint64(i*5 + j + 1)
		r.Offset = int64(r.Sequence * 100)
		r.Event.EventID = fmt.Sprint("disk_e_", r.Sequence)
		r.Event.RequestID = fmt.Sprint("disk_req_", i)
		r.Event.AttemptID = ""
		r.Event.ProjectID = fmt.Sprint("project_", i)
		if r.Event.Kind >= ledger.EventReservationCreated && r.Event.Kind <= ledger.EventAttemptSettled {
			r.Event.AttemptID = fmt.Sprint("disk_att_", i)
		}
		r.Event.ProviderID = "provider"
		r.Event.DeploymentID = "deployment"
		if err := p.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiskUsageProjectionDataFileCeilingRollsBack(t *testing.T) {
	tooSmall := filepath.Join(t.TempDir(), "too-small.db")
	if _, err := CreateDiskProjectionWithLimit(tooSmall, diskGrowthReader(), 1); !errors.Is(err, ErrDiskUsageDiskLimit) {
		t.Fatalf("small initialization ceiling: %v", err)
	}
	if _, err := os.Stat(tooSmall); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed initialization left candidate file: %v", err)
	}

	measurePath := filepath.Join(t.TempDir(), "measure.db")
	measure, err := CreateDiskProjection(measurePath, diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(measurePath)
	if err != nil {
		t.Fatal(err)
	}
	baseBytes := info.Size()
	if err := measure.Discard(); err != nil {
		t.Fatal(err)
	}
	ceiling := baseBytes + 256<<10
	path := filepath.Join(t.TempDir(), "limited.db")
	p, err := CreateDiskProjectionWithLimit(path, diskGrowthReader(), ceiling)
	if err != nil {
		t.Fatal(err)
	}
	refused := false
	var through uint64
	for request := 0; request < 200 && !refused; request++ {
		rows := finalizationFixture(false, false)
		for j, record := range rows {
			sequence := uint64(request*len(rows) + j + 1)
			record.Event.EventID = fmt.Sprintf("disk_e_%d", sequence)
			record.Event.RequestID = fmt.Sprintf("disk_req_%d", request)
			record.Event.ProjectID = fmt.Sprintf("project_%d", request)
			record.Event.ProviderID = "provider"
			record.Event.DeploymentID = "deployment"
			if record.Event.Kind >= ledger.EventReservationCreated && record.Event.Kind <= ledger.EventAttemptSettled {
				record.Event.AttemptID = fmt.Sprintf("disk_att_%d", request)
			}
			record.Sequence = sequence
			record.Offset = int64(sequence * 100)
			if err := p.Apply(record); err != nil {
				if !errors.Is(err, ErrDiskUsageDiskLimit) {
					t.Fatal(err)
				}
				refused = true
				break
			}
			through = sequence
			info, err := os.Stat(path)
			if err != nil || info.Size() > ceiling {
				t.Fatalf("committed transaction crossed ceiling: size=%v ceiling=%d err=%v", info, ceiling, err)
			}
		}
	}
	if !refused || through == 0 {
		t.Fatalf("expected bounded non-empty prefix then disk refusal, through=%d refused=%t", through, refused)
	}
	stats, err := p.ReadStats(context.Background())
	if err != nil || stats.Watermark.Sequence != through {
		t.Fatalf("refused transaction changed projection position: %+v %v", stats, err)
	}
	info, err = os.Stat(path)
	if err != nil || info.Size() > ceiling {
		t.Fatalf("file exceeded configured ceiling after refusal: size=%v ceiling=%d err=%v", info, ceiling, err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
}

func TestDiskUsagePagesFreezePrefixAndBoundSparseScans(t *testing.T) {
	ctx := context.Background()
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	for i := 0; i < 230; i++ {
		diskGrowthRequest(t, p, i)
	}
	stats, err := p.ReadStats(ctx)
	if err != nil || stats.Totals.Requests != 230 || stats.Totals.Attempts != 230 || stats.Metrics.ActiveRequests != 0 {
		t.Fatal(stats, err)
	}
	key := domain.RollupKey{PeriodID: "2026-10-02", TimezoneVersion: 1, Dimension: domain.RollupDimensionProject, DimensionKey: domain.RollupOtherKey}
	other, found, err := p.ReadRollup(ctx, key)
	if err != nil || !found || other.Requests != 30 || other.Attempts != 30 || other.CostMicrosUSD != 210 {
		t.Fatal("rollup dimension admission drift", other, found, err)
	}
	q := AttemptQuery{Limit: 3, ProviderID: "provider"}
	limits := DiskUsagePageLimits{ScanRows: 4, InputBytes: 8 << 20, OutputBytes: 8 << 20}
	page, err := p.AttemptPage(ctx, q, DiskUsageCursor{}, limits)
	if err != nil || len(page.Items) != 3 || page.Done {
		t.Fatal(page, err)
	}
	frozen := page.Head
	seen := len(page.Items)
	last := page.Items[len(page.Items)-1].Sequence
	cursor := page.Next
	diskGrowthRequest(t, p, 230)
	for !page.Done {
		page, err = p.AttemptPage(ctx, q, cursor, limits)
		if err != nil {
			t.Fatal(err)
		}
		if page.Head != frozen || page.Scanned > 4 || len(page.Items) > 3 || page.InputBytes > limits.InputBytes || page.OutputBytes > limits.OutputBytes {
			t.Fatal("unbounded/floating page", page)
		}
		for _, a := range page.Items {
			if a.Sequence >= last || a.Sequence > frozen.Sequence {
				t.Fatal("page repeated or crossed frozen prefix", a)
			}
			last = a.Sequence
		}
		seen += len(page.Items)
		cursor = page.Next
	}
	if seen != 230 {
		t.Fatal("frozen pagination lost rows", seen)
	}
	q.Status = "success" // Every settled fixture is an error, so the first page is empty but resumable.
	page, err = p.AttemptPage(ctx, q, DiskUsageCursor{}, limits)
	if err != nil || len(page.Items) != 0 || page.Done || page.Scanned != 4 || page.Next == (DiskUsageCursor{}) {
		t.Fatal("sparse scan hid continuation", page, err)
	}
	changed := q
	changed.DeploymentID = "other"
	if _, err := p.AttemptPage(ctx, changed, page.Next, limits); err == nil {
		t.Fatal("query-mismatched cursor accepted")
	}
	otherDB, err := CreateDiskProjection(filepath.Join(t.TempDir(), "other.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	if _, err := otherDB.AttemptPage(ctx, q, page.Next, limits); err == nil {
		t.Fatal("other generation accepted cursor")
	}
	summaries, err := p.SummaryPage(ctx, DiskSummaryQuery{Limit: 1, ProjectID: "project_17", FailedOnly: true}, DiskUsageCursor{}, diskTestLimits)
	if err != nil || !summaries.Done || summaries.Scanned != 1 || len(summaries.Items) != 1 || summaries.Items[0].RequestID != "disk_req_17" {
		t.Fatal("summary index mismatch", summaries, err)
	}
	if stats := p.db.Stats(); stats.OpenTxN != 0 {
		t.Fatal("page retained DB reader", stats)
	}
	if _, err := p.AttemptPage(ctx, AttemptQuery{Limit: 1}, DiskUsageCursor{}, DiskUsagePageLimits{ScanRows: 1, InputBytes: 1, OutputBytes: 1}); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("width silently truncated", err)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("attempts")).Delete(diskUsageSequence(230*5 + 4)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AttemptPage(ctx, AttemptQuery{Limit: 1, ProjectID: "project_230"}, DiskUsageCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("orphan index became empty", err)
	}
}

func TestDiskUsageCloseJoinsSelectedReaderAndWithdrawsAdmission(t *testing.T) {
	rows := finalizationFixture(false, false)
	entered, release := make(chan struct{}), make(chan struct{})
	base := fixtureFinalReader(rows)
	reader := eventPositionReader{base, func(ctx context.Context, id string) (ledger.Watermark, bool, error) {
		close(entered)
		<-release
		return base.ReadEventPosition(ctx, id)
	}}
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), reader)
	if err != nil {
		t.Fatal(err)
	}
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }); _ = p.Close() })
	applied := make(chan error, 1)
	go func() { applied <- p.Apply(rows[0]) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	deadline := time.After(time.Second)
	for !p.retired.Load() {
		select {
		case <-deadline:
			t.Fatal("Close did not withdraw")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := p.ReadStats(context.Background()); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatal("closed before reader exit", err)
	default:
	}
	unblock.Do(func() { close(release) })
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if p.reader != nil || p.db != nil {
		t.Fatal("retired projection retained dependencies")
	}
	if _, _, err := p.ReadRequest(context.Background(), "req"); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
}

func TestDiskUsageNativeWideIdentitiesUseFixedKeys(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	native, err := ledger.OpenWithOptions(filepath.Join(root, "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: bytes.Repeat([]byte{0x64}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	rows := finalizationFixture(true, false)
	for _, row := range rows {
		e := row.Event
		e.ProjectID = strings.Repeat("p", 40000)
		e.PeriodID = strings.Repeat("d", 40000)
		e.RequestedModel = strings.Repeat("m", 40000)
		e.ProviderModel = strings.Repeat("u", 40000)
		if _, err := native.Append(ctx, e); err != nil {
			t.Fatal("legal wide native event", err)
		}
	}
	owner, _, err := native.BuildProjectionFromCursor(ctx, 1, 5, filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	d, p, err := NewDiskLedgerDerivative(owner, filepath.Join(root, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close(); _ = p.Close() }()
	a, err := NewAggregateWithFinalizationReader(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
		if err := d.Apply(r); err != nil {
			return err
		}
		return a.Apply(r)
	}); err != nil {
		t.Fatal(err)
	}
	s, err := p.ReadStats(ctx)
	if err != nil || s.Totals != a.Snapshot().Totals || s.Metrics != a.Metrics() {
		t.Fatal(s, err)
	}
	for key, want := range a.rollupDelta {
		got, found, err := p.ReadRollup(ctx, key)
		if err != nil || !found || got != *want {
			t.Fatal("wide rollup differs", key.Dimension, found, err)
		}
	}
	page, err := p.AttemptPage(ctx, AttemptQuery{Limit: 1, ProjectID: strings.Repeat("p", 40000)}, DiskUsageCursor{}, diskTestLimits)
	if err != nil || !page.Done || len(page.Items) != 1 {
		t.Fatal("wide filtered page", page.Scanned, err)
	}
}

func TestDiskUsageActiveCountCoversMoreThanScratchRequest(t *testing.T) {
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i := 0; i < 6; i++ {
		e := finalizationFixture(false, false)[0].Event
		e.EventID = fmt.Sprint("disk_e_", i+1)
		e.RequestID = fmt.Sprint("req_", i%3)
		if i >= 3 {
			e.Kind = ledger.EventRequestFinalized
			e.Outcome = "interrupted"
		}
		if err := p.Apply(ledger.Record{Generation: 1, Sequence: uint64(i + 1), Offset: int64((i + 1) * 100), Event: e}); err != nil {
			t.Fatal(err)
		}
		s, err := p.ReadStats(context.Background())
		want := uint64(i + 1)
		if i >= 3 {
			want = uint64(5 - i)
		}
		if err != nil || s.Metrics.ActiveRequests != want {
			t.Fatal("active count came from scratch", s.Metrics.ActiveRequests, want, err)
		}
	}
}

func TestDiskUsageQueryWidthRejectsBeforeAllocatingCallerSizedCopy(t *testing.T) {
	for _, value := range []string{strings.Repeat("x", 2<<20), strings.Repeat("<", 2<<20)} {
		// The source string belongs to the caller before measurement. No DB is
		// needed: rejection must happen before both Marshal and reader admission.
		p := &DiskProjection{}
		q := AttemptQuery{Limit: 1, ProjectID: value}
		result := testing.Benchmark(func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := p.AttemptPage(context.Background(), q, DiskUsageCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageLimit) {
					b.Fatal(err)
				}
			}
		})
		if result.AllocedBytesPerOp() > 4096 {
			t.Fatal("oversized filter copied before rejection", result.AllocedBytesPerOp())
		}
	}
	p := &DiskProjection{}
	if _, err := p.SummaryPage(context.Background(), DiskSummaryQuery{Limit: 1, ProjectID: strings.Repeat("<", 2<<20)}, DiskUsageCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
	// Several individually small fields must still be bounded together.
	q := AttemptQuery{Limit: 1, ProjectID: strings.Repeat("p", 600000), RequestedModel: strings.Repeat("m", 600000)}
	if _, err := p.AttemptPage(context.Background(), q, DiskUsageCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
	// Raw input may fit while escaping exceeds the independent encoded gate.
	disk, err := CreateDiskProjection(filepath.Join(t.TempDir(), "width.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	q = AttemptQuery{Limit: 1, ProjectID: strings.Repeat("<", 200000)}
	if _, err := disk.AttemptPage(context.Background(), q, DiskUsageCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
}

func TestDiskUsageReaderSlotsAreFiniteAndReleasedOnFailure(t *testing.T) {
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			done <- p.view(context.Background(), func(*bbolt.Tx) error { entered <- struct{}{}; <-release; return errors.New("borrowed reader failed") })
		}()
	}
	<-entered
	<-entered
	if _, err := p.ReadStats(context.Background()); !errors.Is(err, ErrDiskUsageBusy) {
		t.Fatal("unbounded reader admission", err)
	}
	q := AttemptQuery{Limit: 1, ProjectID: strings.Repeat("<", 100000)}
	result := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := p.AttemptPage(context.Background(), q, DiskUsageCursor{}, diskTestLimits); !errors.Is(err, ErrDiskUsageBusy) {
				b.Fatal(err)
			}
		}
	})
	if result.AllocedBytesPerOp() > 4096 {
		t.Fatal("Busy request encoded before admission", result.AllocedBytesPerOp())
	}
	once.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		if err := <-done; err == nil {
			t.Fatal("reader failure hidden")
		}
	}
	if len(p.readers) != 0 || p.db.Stats().OpenTxN != 0 {
		t.Fatal("failed reader retained slot/transaction")
	}
	if _, err := p.ReadStats(context.Background()); err != nil {
		t.Fatal("slot not reusable", err)
	}
}
