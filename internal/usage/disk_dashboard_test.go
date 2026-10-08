package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

var diskDashboardTestLimits = DiskDashboardLimits{Hours: 256, ScanRows: 65536, InputBytes: 8 << 20, OutputBytes: 8 << 20}

func assertDiskDashboard(t *testing.T, f *diskFailureFixture, now time.Time, period Period) DiskDashboard {
	t.Helper()
	got, err := f.p.ReadDashboard(context.Background(), now, period, diskDashboardTestLimits)
	want := f.a.Dashboard(now, period)
	if err != nil || !reflect.DeepEqual(got.Dashboard, want) || got.Head != f.a.Watermark() {
		g, _ := json.Marshal(got.Dashboard)
		w, _ := json.Marshal(want)
		t.Fatalf("Dashboard differs: %v\ngot %s\nwant %s", err, g, w)
	}
	encoded, _ := json.Marshal(want)
	if got.OutputBytes != len(encoded) || got.Scanned > diskDashboardTestLimits.ScanRows || got.InputBytes > diskDashboardTestLimits.InputBytes || len(f.p.readers) != 0 || f.p.db.Stats().OpenTxN != 0 {
		t.Fatal("budget/reader leak", got.Scanned, got.InputBytes, got.OutputBytes)
	}
	return got
}
func TestDiskDashboardNativeExactGroupsBeyondRollupLimit(t *testing.T) {
	base := diskFailureBase("dashboard").OccurredAt
	var events []ledger.Event
	for i := 0; i < 216; i++ {
		rows := diskFailureRequest(fmt.Sprint("dash_", i), "success", 1, false, true)
		group := fmt.Sprintf("group_%03d", i)
		if i >= 207 {
			group = "z_promote"
		}
		for j := range rows {
			rows[j].RequestedModel = group
			rows[j].ProviderModel = group
			rows[j].ProviderID = group
			rows[j].OccurredAt = rows[j].OccurredAt.Add(time.Duration(i%12) * time.Hour)
			if rows[j].Kind == ledger.EventAttemptSettled {
				rows[j].ProviderInputTokens = 3
				rows[j].ProviderOutputTokens = 5
				rows[j].LatencyMillis = 17
				rows[j].TokenEstimated = i%4 == 0
				rows[j].CostEstimated = i%7 == 0
				if i%13 == 0 {
					s := domain.NewUnknownPriceSnapshot(rows[j].OccurredAt)
					rows[j].PriceSnapshot = &s
					rows[j].LeaseMode = ledger.LeaseModeUnknownAllowed
					rows[j].CommittedMicrosUSD = nil
				} else if i%19 == 0 {
					rows[j].PriceSnapshot = usagePriceSnapshot(t, domain.BillingModeFree, rows[j].OccurredAt)
					rows[j].LeaseMode = ledger.LeaseModeFree
					rows[j].CommittedMicrosUSD = ledger.MicrosUSD(0)
				}
				if i%17 == 0 {
					rows[j].Outcome = "provider_error"
					rows[j].RetryCount = 1
					rows[j].FailurePhase = "provider"
					rows[j].ErrorClass = "timeout"
				}
			}
		}
		events = append(events, rows...)
	}
	// The first 200 daily groups fold, but all Dashboard keys still compete.
	f := newDiskFailureFixture(t, events)
	period := Period{ID: events[0].PeriodID, TimezoneVersion: 1}
	got := assertDiskDashboard(t, f, base.Add(2*time.Hour), period)
	for _, dim := range []string{"requested_model", "provider_model", "provider"} {
		if got.Dashboard.Breakdowns[dim]["calls"][0].Key != "z_promote" || got.Dashboard.Breakdowns[dim]["calls"][0].Calls != 9 {
			t.Fatal("late group was folded or missed", dim, got.Dashboard.Breakdowns[dim])
		}
	}
	if len(got.Dashboard.Hourly) != 12 || !got.Dashboard.Hourly[11].Hour.After(base.Add(2*time.Hour)) {
		t.Fatal("legacy future hours truncated")
	}
	if got.Dashboard.Today.EstimatedInputTokens == 0 || got.Dashboard.Today.UnknownAttempts == 0 || len(got.Dashboard.RecentAnomalies) != 5 {
		t.Fatal("missing accounting subsets")
	}
	// Integer/lexicographic ties, independent rankings, and empty collections.
	assertDiskDashboard(t, f, base.Add(2*time.Hour), Period{ID: period.ID, TimezoneVersion: 2})
}
func TestDiskDashboardNativeLateDuplicateTimezoneAndSignedHours(t *testing.T) {
	rows := diskFailureRequest("dashboard_late", "interrupted", 1, true, false)
	f := newDiskFailureFixture(t, rows[:4])
	period := Period{ID: rows[0].PeriodID, TimezoneVersion: 1}
	now := rows[0].OccurredAt
	before := assertDiskDashboard(t, f, now, period)
	appendDiskDetailEvent(t, f, rows[4])
	after := assertDiskDashboard(t, f, now, period)
	if after.Dashboard.Today.Requests != before.Dashboard.Today.Requests || after.Dashboard.Today.Attempts != 1 || after.Dashboard.Today.EstimatedCostMicrosUSD != 7 {
		t.Fatal("late accounting semantics lost")
	}
	appendDiskDetailEvent(t, f, rows[4])
	dup := assertDiskDashboard(t, f, now, period)
	a, b := after.Dashboard, dup.Dashboard
	a.Watermark = b.Watermark
	if !reflect.DeepEqual(a, b) {
		t.Fatal("physical duplicate added dashboard contribution")
	}
	var events []ledger.Event
	for i, hour := range []time.Time{time.Date(1969, 12, 30, 0, 0, 0, 0, time.UTC), time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)} {
		e := diskFailureBase(fmt.Sprint("signed_", i))
		e.Kind = ledger.EventRequestAccepted
		e.EventID = e.RequestID + "_accepted"
		e.OccurredAt = hour
		e.PeriodTimezoneVersion = uint64(i%2 + 1)
		events = append(events, e)
		e.Kind = ledger.EventRequestFinalized
		e.EventID = e.RequestID + "_final"
		e.Outcome = "success"
		e.OccurredAt = hour.Add(time.Second)
		events = append(events, e)
	}
	signed := newDiskFailureFixture(t, events)
	assertDiskDashboard(t, signed, time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC), period)
	// Old hours are skipped by Seek, even if their derived companion is broken.
	if err := signed.p.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("hourly")).Put(diskUsageSequence(uint64(events[0].OccurredAt.Unix())), []byte("bad"))
	}); err != nil {
		t.Fatal(err)
	}
	assertDiskDashboard(t, signed, time.Date(1970, 1, 8, 0, 0, 0, 0, time.UTC), period)
}
func diskDashboardContents(t *testing.T, p *DiskProjection) map[string]map[string][]byte {
	t.Helper()
	out := make(map[string]map[string][]byte)
	if err := p.db.View(func(tx *bbolt.Tx) error {
		for _, name := range diskUsageBuckets {
			out[name] = make(map[string][]byte)
			if err := tx.Bucket([]byte(name)).ForEach(func(k, v []byte) error { out[name][string(k)] = bytes.Clone(v); return nil }); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestDiskDashboardFaultsBudgetsAndWholeResultRefusal(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("dashboard_fault", "provider_error", 1, false, false))
	period := Period{ID: "2026-10-03", TimezoneVersion: 1}
	now := diskFailureBase("x").OccurredAt
	good := assertDiskDashboard(t, f, now, period)
	saved := diskDashboardContents(t, f.p)
	scope := diskDashboardGroupScope{period, "project"}
	top, _ := diskDashboardTopKey(scope, "calls")
	group, _ := diskDashboardGroupKey(scope, "project")
	pk, _ := diskDashboardKey(period)
	qk, _ := diskQualityKey(diskQualityScope{Kind: "period", PeriodID: period.ID, TimezoneVersion: 1})
	faults := []struct {
		name   string
		mutate func(*bbolt.Tx) error
	}{
		{"missing_top", func(tx *bbolt.Tx) error { return tx.Bucket([]byte("dashboard_top")).Delete(top) }},
		{"missing_group", func(tx *bbolt.Tx) error { return tx.Bucket([]byte("dashboard_group")).Delete(group) }},
		{"missing_period", func(tx *bbolt.Tx) error { return tx.Bucket([]byte("dashboard_period")).Delete(pk) }},
		{"missing_hour_companion", func(tx *bbolt.Tx) error {
			return tx.Bucket([]byte("hourly")).Delete(diskUsageSequence(uint64(now.Unix())))
		}},
		{"bad_hour_index", func(tx *bbolt.Tx) error {
			return tx.Bucket([]byte("hourly_order")).Put(diskDashboardHourKey(now.Unix()), []byte{1})
		}},
		{"missing_rank_root", func(tx *bbolt.Tx) error {
			return tx.Bucket([]byte("request_latency_rank")).Delete(diskRankKey(qk, 0, 0))
		}},
		{"missing_anomaly", func(tx *bbolt.Tx) error {
			for key := range saved["attempts"] {
				return tx.Bucket([]byte("attempts")).Delete([]byte(key))
			}
			return nil
		}},
		{"bad_period_scope", func(tx *bbolt.Tx) error {
			p, _, _ := diskUsageGet[diskDashboardPeriod](tx, "dashboard_period", pk)
			p.Period.ID = "wrong"
			return diskUsagePut(tx, "dashboard_period", pk, p)
		}},
		{"duplicate_top_ref", func(tx *bbolt.Tx) error {
			p, _, _ := diskUsageGet[diskDashboardTop](tx, "dashboard_top", top)
			p.Refs = append(p.Refs, p.Refs[0])
			return diskUsagePut(tx, "dashboard_top", top, p)
		}},
	}
	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			if err := f.p.db.Update(fault.mutate); err != nil {
				t.Fatal(err)
			}
			got, err := f.p.ReadDashboard(context.Background(), now, period, diskDashboardTestLimits)
			if !errors.Is(err, ErrDiskUsageCorrupt) || !reflect.DeepEqual(got, DiskDashboard{}) || len(f.p.readers) != 0 || f.p.db.Stats().OpenTxN != 0 {
				t.Fatal("partial result/retained reader", got, err)
			}
			if err := f.p.db.Update(func(tx *bbolt.Tx) error {
				for _, name := range diskUsageBuckets {
					b := tx.Bucket([]byte(name))
					for key, value := range saved[name] {
						if err := b.Put([]byte(key), value); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			assertDiskDashboard(t, f, now, period)
		})
	}
	for _, limits := range []DiskDashboardLimits{{256, good.Scanned - 1, 8 << 20, 8 << 20}, {256, 65536, good.InputBytes - 1, 8 << 20}, {256, 65536, 8 << 20, good.OutputBytes - 1}} {
		got, err := f.p.ReadDashboard(context.Background(), now, period, limits)
		if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskDashboard{}) {
			t.Fatal("whole-query gate failed", got, err, limits)
		}
	}
	// Per-hour limits are result bounds, never a request to silently truncate.
	more := diskFailureRequest("next_hour", "success", 0, false, false)
	for _, e := range more {
		e.OccurredAt = e.OccurredAt.Add(time.Hour)
		appendDiskDetailEvent(t, f, e)
	}
	limits := diskDashboardTestLimits
	limits.Hours = 1
	got, err := f.p.ReadDashboard(context.Background(), now, period, limits)
	if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskDashboard{}) {
		t.Fatal(got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = f.p.ReadDashboard(ctx, now, period, diskDashboardTestLimits)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DiskDashboard{}) {
		t.Fatal(got, err)
	}
}
func TestDiskDashboardAttemptFailureRollsBackAllDerivedRows(t *testing.T) {
	rows := diskFailureRequest("dashboard_rollback", "provider_error", 1, false, false)
	f := newDiskFailureFixture(t, rows[:3])
	period := Period{ID: rows[0].PeriodID, TimezoneVersion: 1}
	// Overflow in the last dimension happens after period totals and earlier
	// dimension caches would have changed. Every bucket must still roll back.
	scope := diskDashboardGroupScope{period, "provider_model"}
	key, _ := diskDashboardGroupKey(scope, rows[3].ProviderModel)
	g := diskDashboardGroup{Scope: scope, Item: Breakdown{Key: rows[3].ProviderModel, Calls: math.MaxInt64}}
	if err := f.p.db.Update(func(tx *bbolt.Tx) error { return diskUsagePut(tx, "dashboard_group", key, g) }); err != nil {
		t.Fatal(err)
	}
	before := diskDashboardContents(t, f.p)
	r, _, err := f.native.AppendRecordWithReceipt(context.Background(), rows[3])
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.Apply(r); err == nil {
		t.Fatal("overflow accepted")
	}
	if !reflect.DeepEqual(before, diskDashboardContents(t, f.p)) {
		t.Fatal("failed Apply published partial counters/indexes/watermark")
	}
	if err := f.p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("dashboard_group")).Delete(key) }); err != nil {
		t.Fatal(err)
	}
	if err := f.d.Apply(r); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Apply(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := f.a.Apply(r); err != nil {
		t.Fatal(err)
	}
	assertDiskDashboard(t, f, rows[0].OccurredAt, period)
}
func TestDiskDashboardWideIdentityAdmissionAndLifecycle(t *testing.T) {
	rows := diskFailureRequest("dashboard_wide", "provider_error", 1, false, false)
	wide := strings.Repeat("m", 40<<10)
	for i := range rows {
		rows[i].RequestedModel = wide
		rows[i].ProviderModel = wide
	}
	f := newDiskFailureFixture(t, rows)
	period := Period{ID: rows[0].PeriodID, TimezoneVersion: 1}
	now := rows[0].OccurredAt
	good := assertDiskDashboard(t, f, now, period)
	if good.InputBytes < 6*len(wide) || good.OutputBytes < 6*len(wide) {
		t.Fatal("wide identities uncharged", good.InputBytes, good.OutputBytes)
	}
	limits := diskDashboardTestLimits
	limits.InputBytes = 4096
	got, err := f.p.ReadDashboard(context.Background(), now, period, limits)
	if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskDashboard{}) {
		t.Fatal(got, err)
	}
	// Returned strings/counters are independent of later caller mutation.
	good.Dashboard.Breakdowns["requested_model"]["calls"][0].Key = "mutated"
	assertDiskDashboard(t, f, now, period)
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
	got, err = f.p.ReadDashboard(context.Background(), now, Period{ID: strings.Repeat("\x00", 100<<10)}, diskDashboardTestLimits)
	if !errors.Is(err, ErrDiskUsageBusy) || !reflect.DeepEqual(got, DiskDashboard{}) {
		t.Fatal(got, err)
	}
	close(release)
	wg.Wait()
	for _, id := range []string{strings.Repeat("x", (1<<20)+1), strings.Repeat("\x00", 200<<10)} {
		got, err = f.p.ReadDashboard(context.Background(), now, Period{ID: id}, diskDashboardTestLimits)
		if !errors.Is(err, ErrDiskUsageLimit) || !reflect.DeepEqual(got, DiskDashboard{}) {
			t.Fatal(got, err)
		}
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = f.p.ReadDashboard(context.Background(), now, period, diskDashboardTestLimits)
	if !errors.Is(err, ErrDiskUsageClosed) || !reflect.DeepEqual(got, DiskDashboard{}) {
		t.Fatal(got, err)
	}
}

func TestDiskDashboardMissingHistoryCannotBeReinitializedByNewSettlement(t *testing.T) {
	for _, fault := range []string{"top_new_group", "period_old_group", "period_new_group"} {
		t.Run(fault, func(t *testing.T) {
			first := diskFailureRequest("history_first", "success", 1, false, true)
			next := diskFailureRequest("history_next", "success", 1, false, true)
			if fault != "period_old_group" {
				for i := range next {
					next[i].RequestedModel = "new_group"
				}
			}
			f := newDiskFailureFixture(t, append(first, next[:3]...))
			period := Period{ID: first[0].PeriodID, TimezoneVersion: 1}
			pk, _ := diskDashboardKey(period)
			tk, _ := diskDashboardTopKey(diskDashboardGroupScope{period, "requested_model"}, "calls")
			if err := f.p.db.Update(func(tx *bbolt.Tx) error {
				if fault == "top_new_group" {
					return tx.Bucket([]byte("dashboard_top")).Delete(tk)
				}
				return tx.Bucket([]byte("dashboard_period")).Delete(pk)
			}); err != nil {
				t.Fatal(err)
			}
			before := diskDashboardContents(t, f.p)
			r, _, err := f.native.AppendRecordWithReceipt(context.Background(), next[3])
			if err != nil {
				t.Fatal(err)
			}
			if err := f.d.Apply(r); !errors.Is(err, ErrDiskUsageCorrupt) {
				t.Fatal("missing history was silently initialized", err)
			}
			if !reflect.DeepEqual(before, diskDashboardContents(t, f.p)) {
				t.Fatal("corrupt history refusal changed projection/watermark")
			}
		})
	}
}

func TestDiskDashboardConcurrentViewsKeepOneWatermark(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 8; i++ {
		events = append(events, diskFailureRequest(fmt.Sprint("coherent_", i), "provider_error", 1, false, false)...)
	}
	f := newDiskFailureFixture(t, events)
	period := Period{ID: events[0].PeriodID, TimezoneVersion: 1}
	now := events[0].OccurredAt
	var records []ledger.Record
	if _, err := f.native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, func(r ledger.Record) error { records = append(records, r); return nil }); err != nil {
		t.Fatal(err)
	}
	ref, err := NewAggregateWithFinalizationReader(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[uint64]Dashboard{0: ref.Dashboard(now, period)}
	for _, r := range records {
		if err := ref.Apply(r); err != nil {
			t.Fatal(err)
		}
		expected[r.Sequence] = ref.Dashboard(now, period)
	}
	d, p, err := NewDiskLedgerDerivative(f.owner, t.TempDir()+"/coherent.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close(); _ = p.Close() }()
	start := make(chan struct{})
	failures := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for _, r := range records {
			if err := d.Apply(r); err != nil {
				failures <- err
				return
			}
		}
	}()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 80; j++ {
				got, err := p.ReadDashboard(context.Background(), now, period, diskDashboardTestLimits)
				if err != nil {
					failures <- err
					return
				}
				if !reflect.DeepEqual(got.Dashboard, expected[got.Head.Sequence]) {
					failures <- fmt.Errorf("mixed Dashboard cut %d", got.Head.Sequence)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if p.db.Stats().OpenTxN != 0 || len(p.readers) != 0 {
		t.Fatal("concurrent query retained reader")
	}
}
