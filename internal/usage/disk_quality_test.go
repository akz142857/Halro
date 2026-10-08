package usage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

var diskQualityLimits = DiskUsagePageLimits{ScanRows: 256, InputBytes: 8 << 20, OutputBytes: 8 << 20}

func requestQualityOnly(b Bucket) Bucket {
	return Bucket{Hour: b.Hour, Requests: b.Requests, RequestErrors: b.RequestErrors, RequestLatencySamples: b.RequestLatencySamples, RequestLatencyP50Millis: b.RequestLatencyP50Millis, RequestLatencyP95Millis: b.RequestLatencyP95Millis}
}

func TestDiskRequestQualityExactNativeDashboardDifferential(t *testing.T) {
	var events []ledger.Event
	base := diskFailureBase("quality").OccurredAt
	for i := 0; i < 111; i++ {
		e := diskFailureBase(fmt.Sprint("quality_", i))
		e.PeriodID = fmt.Sprint("period_", i%2)
		e.PeriodTimezoneVersion = uint64(i % 3)
		e.Kind = ledger.EventRequestAccepted
		e.EventID = e.RequestID + "_accepted"
		events = append(events, e)
		e.Kind = ledger.EventRequestFinalized
		e.EventID = e.RequestID + "_final"
		e.Outcome = "success"
		if i%4 == 0 {
			e.Outcome = "rejected"
		}
		e.OccurredAt = base.Add(time.Duration((i*i)%167+1) * time.Millisecond)
		switch i {
		case 0:
			e.OccurredAt = base.Add(100 * time.Nanosecond)
		case 1:
			e.OccurredAt = base
		case 2:
			e.OccurredAt = base.Add(-time.Millisecond)
		case 3:
			e.OccurredAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		events = append(events, e)
	}
	f := newDiskFailureFixture(t, events)
	for i := 0; i < 2; i++ {
		for v := uint64(0); v < 3; v++ {
			period := Period{ID: fmt.Sprint("period_", i), TimezoneVersion: v}
			want := f.a.Dashboard(base.Add(time.Hour), period).Today
			got, err := f.p.ReadPeriodRequestQuality(context.Background(), period, diskQualityLimits)
			if err != nil || got.Bucket != requestQualityOnly(want) || got.Watermark != f.a.Watermark() || got.Scanned != 179 {
				t.Fatal(period, got, requestQualityOnly(want), err)
			}
		}
	}
	for _, hour := range []time.Time{base.Add(-time.Hour), base, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)} {
		want := Bucket{Hour: hour.UTC().Truncate(time.Hour)}
		for _, b := range f.a.Dashboard(base.Add(time.Hour), Period{ID: "period_0"}).Hourly {
			if b.Hour.Equal(want.Hour) {
				want = requestQualityOnly(b)
			}
		}
		got, err := f.p.ReadHourRequestQuality(context.Background(), hour, diskQualityLimits)
		if err != nil || got.Bucket != want {
			t.Fatal(hour, got, want, err)
		}
	}
	// No approximation: the saturated Sub sample is the actual maximum duration
	// in milliseconds; the positive sub-millisecond sample remains a zero leaf.
	maxHour, err := f.p.ReadHourRequestQuality(context.Background(), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), diskQualityLimits)
	if err != nil || maxHour.Bucket.RequestLatencySamples != 1 || uint64(maxHour.Bucket.RequestLatencyP95Millis) != diskMaxRequestLatencyMillis {
		t.Fatal(maxHour, err)
	}
	zero := newDiskFailureFixture(t, events[:2])
	z, err := zero.p.ReadPeriodRequestQuality(context.Background(), Period{ID: "period_0"}, diskQualityLimits)
	if err != nil || z.Bucket.RequestLatencySamples != 1 || z.Bucket.RequestLatencyP50Millis != 0 || z.Bucket.RequestLatencyP95Millis != 0 {
		t.Fatal("submillisecond sample lost", z, err)
	}
}

func TestDiskRequestQualityLateDuplicateAndAtomicTreeRollback(t *testing.T) {
	events := diskFailureRequest("quality_late", "interrupted", 1, true, false)
	f := newDiskFailureFixture(t, events[:4])
	period := Period{ID: events[0].PeriodID, TimezoneVersion: 1}
	before, err := f.p.ReadPeriodRequestQuality(context.Background(), period, diskQualityLimits)
	if err != nil {
		t.Fatal(err)
	}
	appendDiskDetailEvent(t, f, events[4])
	appendDiskDetailEvent(t, f, events[3])
	appendDiskDetailEvent(t, f, events[4])
	after, err := f.p.ReadPeriodRequestQuality(context.Background(), period, diskQualityLimits)
	if err != nil || after.Bucket != before.Bucket || after.Bucket.Requests != 1 || after.Watermark.Sequence != 7 {
		t.Fatal("late/physical duplicate changed request quality", before, after, err)
	}
	// Poison the hour scope after the period scope would have been updated.
	rows := finalizationFixture(false, false)
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), fixtureFinalReader(rows))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, r := range rows[:4] {
		if err := p.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	hour := rows[4].Event.OccurredAt.UTC().Truncate(time.Hour)
	scope := diskQualityScope{Kind: "hour", Hour: hour.Unix()}
	key, _ := diskQualityKey(scope)
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return diskUsagePut(tx, "request_quality", key, diskRequestQuality{Scope: scope, Requests: math.MaxInt64})
	}); err != nil {
		t.Fatal(err)
	}
	beforeDigest := diskUsageDigest(t, p)
	if err := p.Apply(rows[4]); err == nil {
		t.Fatal("overflowing hour accepted")
	}
	if diskUsageDigest(t, p) != beforeDigest {
		t.Fatal("partial period/tree/summary/head commit on hour error")
	}
}

func TestDiskRequestQualityTreeCorruptionAndBudgetsFailClosed(t *testing.T) {
	for _, fault := range []string{"missing-root", "bad-child", "wide-counter", "wrong-scope", "missing-quality", "zero-counter", "sample-mismatch", "row-limit", "byte-limit", "output-limit", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := newDiskFailureFixture(t, diskFailureRequest("fault_quality", "rejected", 0, false, false))
			period := Period{ID: "2026-10-03", TimezoneVersion: 1}
			scope := diskQualityScope{Kind: "period", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion}
			key, _ := diskQualityKey(scope)
			if err := f.p.db.Update(func(tx *bbolt.Tx) error {
				b := tx.Bucket([]byte("request_latency_rank"))
				switch fault {
				case "missing-root":
					return b.Delete(diskRankKey(key, 0, 0))
				case "bad-child":
					return b.Put(diskRankKey(key, 1, 0), diskUsageSequence(2))
				case "wide-counter":
					return b.Put(diskRankKey(key, 0, 0), bytes.Repeat([]byte{1}, 9))
				case "zero-counter":
					return b.Put(diskRankKey(key, 0, 0), diskUsageSequence(0))
				case "missing-quality":
					return tx.Bucket([]byte("request_quality")).Delete(key)
				case "wrong-scope", "sample-mismatch":
					q, _, err := diskUsageGet[diskRequestQuality](tx, "request_quality", key)
					if err != nil {
						return err
					}
					if fault == "wrong-scope" {
						q.Scope.PeriodID = "foreign"
					} else {
						q.Samples = 0
					}
					return diskUsagePut(tx, "request_quality", key, q)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			limits := diskQualityLimits
			switch fault {
			case "row-limit":
				limits.ScanRows = 178
			case "byte-limit":
				limits.InputBytes = 50
			case "output-limit":
				limits.OutputBytes = 1
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := f.p.ReadPeriodRequestQuality(ctx, period, limits)
			if err == nil || !reflect.DeepEqual(got, DiskRequestQuality{}) {
				t.Fatal("failure leaked valid quality", fault, got, err)
			}
			if len(f.p.readers) != 0 || f.p.db.Stats().OpenTxN != 0 {
				t.Fatal("failed quality read retained slot/transaction")
			}
		})
	}
}

func TestDiskRequestQualityIntegerRankOverflowAndInvalidLeaf(t *testing.T) {
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	period := Period{ID: "large-count"}
	scope := diskQualityScope{Kind: "period", PeriodID: period.ID}
	key, _ := diskQualityKey(scope)
	// Encode a legitimate single-value distribution with MaxInt64 observations;
	// a physical fixture this large is unnecessary to test integer rank arithmetic.
	seed := func(latency uint64) error {
		return p.db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte("request_latency_rank"))
			prefix := uint64(0)
			for depth := 0; depth <= diskLatencyBits; depth++ {
				if err := b.Put(diskRankKey(key, depth, prefix), diskUsageSequence(math.MaxInt64)); err != nil {
					return err
				}
				if depth < diskLatencyBits {
					prefix |= latency & (uint64(1) << (diskLatencyBits - depth - 1))
				}
			}
			return diskUsagePut(tx, "request_quality", key, diskRequestQuality{Scope: scope, Requests: math.MaxInt64, Samples: math.MaxInt64})
		})
	}
	if err := seed(777); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReadPeriodRequestQuality(context.Background(), period, diskQualityLimits)
	if err != nil || got.Bucket.RequestLatencySamples != math.MaxInt64 || got.Bucket.RequestLatencyP50Millis != 777 || got.Bucket.RequestLatencyP95Millis != 777 {
		t.Fatal("nearest rank multiplied/rounded incorrectly", got, err)
	}
	// This is derived corruption, not a legal duration; local count conservation
	// alone must not credit a leaf beyond time.Duration's saturated value range.
	otherScope := diskQualityScope{Kind: "period", PeriodID: "invalid-leaf"}
	otherKey, _ := diskQualityKey(otherScope)
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("request_latency_rank"))
		for depth := 0; depth <= diskLatencyBits; depth++ {
			prefix := uint64(0)
			if depth > 0 {
				prefix = ((uint64(1) << depth) - 1) << (diskLatencyBits - depth)
			}
			if err := b.Put(diskRankKey(otherKey, depth, prefix), diskUsageSequence(1)); err != nil {
				return err
			}
		}
		return diskUsagePut(tx, "request_quality", otherKey, diskRequestQuality{Scope: otherScope, Requests: 1, Samples: 1})
	}); err != nil {
		t.Fatal(err)
	}
	got, err = p.ReadPeriodRequestQuality(context.Background(), Period{ID: "invalid-leaf"}, diskQualityLimits)
	if !errors.Is(err, ErrDiskUsageCorrupt) || got != (DiskRequestQuality{}) {
		t.Fatal("impossible leaf accepted", got, err)
	}
}

func TestDiskRequestQualityLegacyUnknownLatencyAndWideNativePeriod(t *testing.T) {
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	now := diskFailureBase("legacy").OccurredAt
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return diskUsageAddRequestQuality(tx, RequestSummary{RequestID: "legacy", PeriodID: "legacy", CompletedAt: now, Outcome: "rejected"})
	}); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReadPeriodRequestQuality(context.Background(), Period{ID: "legacy"}, diskQualityLimits)
	if err != nil || got.Bucket.Requests != 1 || got.Bucket.RequestErrors != 1 || got.Bucket.RequestLatencySamples != 0 {
		t.Fatal("unknown AcceptedAt fabricated sample", got, err)
	}
	events := diskFailureRequest("wide-quality", "rejected", 0, false, false)
	period := strings.Repeat("p", 40<<10)
	for i := range events {
		events[i].PeriodID = period
	}
	f := newDiskFailureFixture(t, events)
	got, err = f.p.ReadPeriodRequestQuality(context.Background(), Period{ID: period, TimezoneVersion: 1}, diskQualityLimits)
	if err != nil || got.Bucket.Requests != 1 || got.Bucket.RequestLatencyP95Millis != 60000 || got.InputBytes < 40<<10 {
		t.Fatal("legal long period identity lost/not charged", got, err)
	}
	if _, err := f.p.ReadPeriodRequestQuality(context.Background(), Period{ID: period, TimezoneVersion: 1}, DiskUsagePageLimits{ScanRows: 256, InputBytes: 100, OutputBytes: 8 << 20}); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("decoded wide period before byte admission", err)
	}
}

func TestDiskRequestQualitySlotsAndWidthAdmission(t *testing.T) {
	p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), diskGrowthReader())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, id := range []string{"", strings.Repeat("p", 2<<20), strings.Repeat("<", 200000)} {
		if _, err := p.ReadPeriodRequestQuality(context.Background(), Period{ID: id}, diskQualityLimits); !errors.Is(err, ErrDiskUsageLimit) {
			t.Fatal(err)
		}
	}
	entered, release, done := make(chan struct{}, 2), make(chan struct{}), make(chan error, 2)
	var once sync.Once
	defer once.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		go func() {
			done <- p.view(context.Background(), func(*bbolt.Tx) error { entered <- struct{}{}; <-release; return nil })
		}()
	}
	<-entered
	<-entered
	q := Period{ID: strings.Repeat("<", 100000)}
	result := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := p.ReadPeriodRequestQuality(context.Background(), q, diskQualityLimits); !errors.Is(err, ErrDiskUsageBusy) {
				b.Fatal(err)
			}
		}
	})
	if result.AllocedBytesPerOp() > 4096 {
		t.Fatal("Busy quality query encoded before slot", result.AllocedBytesPerOp())
	}
	once.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	got, err := p.ReadPeriodRequestQuality(context.Background(), Period{ID: "missing"}, diskQualityLimits)
	if err != nil || got.Bucket != (Bucket{}) || got.Scanned != 3 {
		t.Fatal(got, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadPeriodRequestQuality(context.Background(), Period{ID: "missing"}, diskQualityLimits); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
}
