package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

var windowTestCutoff = time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
var windowTestLimits = DiskWindowLimits{Rows: 2, InputBytes: 8 << 20}

func windowRequest(id string, attemptNew, summaryNew bool) []ledger.Event {
	rows := diskFailureRequest(id, "provider_error", 2, false, false)
	for i := range rows {
		if (attemptNew && rows[i].Kind == ledger.EventAttemptSettled) || (summaryNew && rows[i].Kind == ledger.EventRequestFinalized) {
			rows[i].OccurredAt = rows[i].OccurredAt.Add(48 * time.Hour)
		}
	}
	return rows
}

func windowPublish(t *testing.T, f *diskFailureFixture) uint64 {
	t.Helper()
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits())
	if err != nil || cut.Head != f.a.Watermark() {
		t.Fatal(cut, err)
	}
	if _, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); err != nil {
		t.Fatal(err)
	}
	return cut.ThroughSequence
}

func windowDrain(t *testing.T, p *DiskProjection, cutoff time.Time, exported uint64, limits DiskWindowLimits) (attempts, summaries, versions, calls int) {
	t.Helper()
	for calls = 1; calls <= 1000; calls++ {
		step, err := p.pruneWindowStep(context.Background(), cutoff, exported, limits)
		if err != nil {
			t.Fatal(step, err)
		}
		if step.Scanned > 2*limits.Rows || step.InputBytes > 2*limits.InputBytes {
			t.Fatal("unbounded cleanup", step)
		}
		attempts += step.Attempts
		summaries += step.Summaries
		versions += step.Versions
		if step.Done {
			return
		}
	}
	t.Fatal("window failed to finish")
	return
}

func windowDigest(t *testing.T, p *DiskProjection, names []string) [32]byte {
	t.Helper()
	h := sha256.New()
	err := p.db.View(func(tx *bbolt.Tx) error {
		for _, name := range names {
			h.Write([]byte(name))
			b := tx.Bucket([]byte(name))
			if b == nil {
				return ErrDiskUsageCorrupt
			}
			c := b.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var size [8]byte
				binary.BigEndian.PutUint64(size[:], uint64(len(k)))
				h.Write(size[:])
				h.Write(k)
				binary.BigEndian.PutUint64(size[:], uint64(len(v)))
				h.Write(size[:])
				h.Write(v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func TestDiskWindowMatchesIndependentNativePrefixesAndPreservesTotals(t *testing.T) {
	for _, c := range []struct {
		name                             string
		firstAttemptNew, firstSummaryNew bool
	}{{"ordered", false, false}, {"new_attempt_old_summary", true, false}, {"old_attempt_new_summary", false, true}} {
		t.Run(c.name, func(t *testing.T) {
			events := windowRequest("first", c.firstAttemptNew, c.firstSummaryNew)
			events = append(events, windowRequest("new", true, true)...)
			events = append(events, windowRequest("late_old", false, false)...)
			f := newDiskFailureFixture(t, events)
			exported := windowPublish(t, f)
			before, err := f.p.ReadStats(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			stable := []string{"active", "started", "hourly", "hourly_order", "request_quality", "request_latency_rank", "dashboard_period", "dashboard_group", "dashboard_top", "rollup", "rollup_counts", "rollup_range", "rollup_versions", "rollup_scope"}
			digest := windowDigest(t, f.p, stable)
			want := f.a.PruneBefore(windowTestCutoff, exported)
			a, s, _, _ := windowDrain(t, f.p, windowTestCutoff, exported, windowTestLimits)
			stats, err := f.p.ReadStats(context.Background())
			if err != nil || a != want.Attempts || s != want.Summaries || stats.Window.Floor != want.Floor || stats.Watermark != before.Watermark || stats.Totals != before.Totals || stats.Metrics != before.Metrics || windowDigest(t, f.p, stable) != digest {
				t.Fatal("window changed native totals or floor", stats, want, a, s, err)
			}
			snap := f.a.Snapshot()
			attempts, err := f.p.AttemptPage(context.Background(), AttemptQuery{Limit: 100}, DiskUsageCursor{}, DiskUsagePageLimits{ScanRows: 128, InputBytes: 8 << 20, OutputBytes: 8 << 20})
			if err != nil || !attempts.Done || len(attempts.Items) != len(snap.Attempts) {
				t.Fatal(attempts, err, snap)
			}
			for i := range attempts.Items {
				if !reflect.DeepEqual(attempts.Items[i], snap.Attempts[len(snap.Attempts)-1-i]) {
					t.Fatal("attempt prefix changed")
				}
			}
			if stats.Window.Attempts != uint64(len(snap.Attempts)) || stats.Window.Summaries != uint64(len(snap.Requests)) {
				t.Fatal("window counters differ", stats.Window, snap)
			}
			for _, id := range []string{"first", "new", "late_old"} {
				want, found := f.a.RequestDetail(id)
				page, got, _ := drainDiskDetail(t, f.p, DiskRequestDetailQuery{RequestID: id, Limit: 1}, DiskRequestDetailCursor{}, diskTestLimits)
				if page.Found != found || (found && (page.Summary != want.Summary || (len(got) != len(want.Attempts) || (len(got) > 0 && !reflect.DeepEqual(got, want.Attempts))))) {
					t.Fatal("detail revived an expired summary", id, page, want, found)
				}
			}
			if a2, s2, v2, _ := windowDrain(t, f.p, windowTestCutoff, exported, windowTestLimits); a2+s2+v2 != 0 {
				t.Fatal("duplicate prune", a2, s2, v2)
			}
			// The authority and its authenticated first positions survive pruning.
			count := 0
			_, err = f.native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, func(r ledger.Record) error {
				count++
				_, found, err := f.owner.ReadEventPosition(context.Background(), r.Event.EventID)
				if !found {
					return ErrDiskUsageCorrupt
				}
				return err
			})
			if err != nil || count != len(events) {
				t.Fatal("native facts altered", count, err)
			}
		})
	}
}

func TestDiskWindowZeroAndLaggingExportPreserveUnarchivedFacts(t *testing.T) {
	f := newDiskFailureFixture(t, windowRequest("lag", false, false))
	digest := windowDigest(t, f.p, diskUsageBuckets)
	step, err := f.p.pruneWindowStep(context.Background(), windowTestCutoff, 0, windowTestLimits)
	if err != nil || step.Attempts+step.Summaries+step.Versions != 0 || windowDigest(t, f.p, diskUsageBuckets) != digest {
		t.Fatal(step, err)
	}
	exported := f.a.Snapshot().Attempts[0].Sequence
	want := f.a.PruneBefore(windowTestCutoff, exported)
	a, s, _, _ := windowDrain(t, f.p, windowTestCutoff, exported, windowTestLimits)
	if a != want.Attempts || s != 0 || s != want.Summaries {
		t.Fatal(a, s, want)
	}
	stats, err := f.p.ReadStats(context.Background())
	if err != nil || stats.Window.Summaries != 1 || stats.Window.Attempts != 1 {
		t.Fatal(stats, err)
	}
}

func TestDiskWindowInvalidatesLocalAndSignedContinuationsAfterActualDeletion(t *testing.T) {
	events := diskFailureRequest("chain", "provider_error", 35, false, false)
	events = append(events, windowRequest("other", false, false)...)
	f := newDiskFailureFixture(t, events)
	exported := windowPublish(t, f)
	ctx := context.Background()
	aq := AttemptQuery{Limit: 1}
	sq := DiskSummaryQuery{Limit: 1}
	fq := FailureQuery{Limit: 1, ProviderID: "provider_a", RequestID: "chain"}
	dq := DiskRequestDetailQuery{RequestID: "chain", Limit: 1}
	a, err := f.p.AttemptPage(ctx, aq, DiskUsageCursor{}, diskTestLimits)
	if err != nil || a.Done {
		t.Fatal(a, err)
	}
	s, err := f.p.SummaryPage(ctx, sq, DiskUsageCursor{}, diskTestLimits)
	if err != nil || s.Done {
		t.Fatal(s, err)
	}
	d, err := f.p.RequestDetailPage(ctx, dq, DiskRequestDetailCursor{}, diskTestLimits)
	if err != nil || d.Done {
		t.Fatal(d, err)
	}
	fp, err := f.p.FailurePage(ctx, fq, DiskFailureCursor{}, DiskUsagePageLimits{ScanRows: 3, InputBytes: 8 << 20, OutputBytes: 8 << 20})
	if err != nil || fp.Done || fp.Next.pending == 0 {
		t.Fatal(fp, err)
	}
	ha, err := f.p.QueryAttemptsHTTP(ctx, aq, "", diskTestLimits)
	if err != nil || len(ha.NextCursor) != 196 {
		t.Fatal(ha, err)
	}
	hf := f.p.encodeHTTPCursor(2, fp.Next)
	step, err := f.p.pruneWindowStep(ctx, windowTestCutoff, exported, windowTestLimits)
	if err != nil || step.Window.Epoch == 0 {
		t.Fatal(step, err)
	}
	if page, err := f.p.AttemptPage(ctx, aq, a.Next, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	if page, err := f.p.SummaryPage(ctx, sq, s.Next, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	if page, err := f.p.RequestDetailPage(ctx, dq, d.Next, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) || page.Found {
		t.Fatal(page, err)
	}
	if page, err := f.p.FailurePage(ctx, fq, fp.Next, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	if page, err := f.p.QueryAttemptsHTTP(ctx, aq, ha.NextCursor, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) || len(page.Attempts) != 0 {
		t.Fatal(page, err)
	}
	if page, err := f.p.QueryFailedRequestsHTTP(ctx, fq, hf, diskTestLimits); !errors.Is(err, ErrDiskUsageCursor) || len(page.Failures) != 0 {
		t.Fatal(page, err)
	}
	// A new nonzero epoch round-trips through the MAC token and still pages.
	ha, err = f.p.QueryAttemptsHTTP(ctx, aq, "", diskTestLimits)
	if err != nil || ha.NextCursor == "" {
		t.Fatal(ha, err)
	}
	if _, err = f.p.QueryAttemptsHTTP(ctx, aq, ha.NextCursor, diskTestLimits); err != nil {
		t.Fatal(err)
	}
}

func TestDiskWindowBoundedVersionCleanupRemainsInvisibleAndResumes(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("wide", "provider_error", 20, false, false))
	exported := windowPublish(t, f)
	limits := DiskWindowLimits{Rows: 1, InputBytes: 8 << 20}
	step, err := f.p.pruneWindowStep(context.Background(), windowTestCutoff, exported, limits)
	if err != nil || step.Attempts != 1 || step.Summaries != 1 || step.Versions != 0 || step.Done {
		t.Fatal(step, err)
	}
	var pending diskWindowCleanup
	if err := f.p.db.View(func(tx *bbolt.Tx) error {
		s, _, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		pending = s.WindowCleanup
		return err
	}); err != nil || pending.Through == 0 {
		t.Fatal(pending, err)
	}
	page, err := f.p.RequestDetailPage(context.Background(), DiskRequestDetailQuery{RequestID: "wide", Limit: 1}, DiskRequestDetailCursor{}, diskTestLimits)
	if err != nil || page.Found || !page.Done {
		t.Fatal("unreachable versions revived detail", page, err)
	}
	_, _, versions, calls := windowDrain(t, f.p, windowTestCutoff, exported, limits)
	if calls < 20 || versions < 20 {
		t.Fatal("versions were not bounded/resumed", versions, calls)
	}
	if err := f.p.db.View(func(tx *bbolt.Tx) error {
		if k, _ := tx.Bucket([]byte("request_versions")).Cursor().First(); k != nil {
			return fmt.Errorf("version orphan survived")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := f.p.ReadStats(context.Background())
	if err != nil || stats.Window.Attempts != 0 || stats.Window.Summaries != 0 || stats.Window.TrimmedAttempts != 20 {
		t.Fatal(stats, err)
	}
}

func TestDiskWindowFailuresRollbackRowsIndexesAndPosition(t *testing.T) {
	for _, fault := range []string{"index", "version", "missing_terminal", "counter", "epoch"} {
		t.Run(fault, func(t *testing.T) {
			f := newDiskFailureFixture(t, windowRequest("bad", false, false))
			exported := windowPublish(t, f)
			err := f.p.db.Update(func(tx *bbolt.Tx) error {
				s, _, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
				if err != nil {
					return err
				}
				switch fault {
				case "index":
					b := tx.Bucket([]byte("attempt_index"))
					k, _ := b.Cursor().First()
					return b.Delete(bytes.Clone(k))
				case "version":
					b := tx.Bucket([]byte("request_versions"))
					k, _ := b.Cursor().First()
					return b.Put(k, []byte("broken"))
				case "missing_terminal":
					key := tx.Bucket([]byte("request_version_head")).Get(diskUsageID("bad"))
					return tx.Bucket([]byte("request_versions")).Delete(diskRequestVersionKey("bad", binary.BigEndian.Uint64(key)))
				case "counter":
					s.Window.Attempts = 0
				case "epoch":
					s.Window.Epoch = ^uint64(0)
				}
				return diskUsagePut(tx, "meta", []byte("state"), s)
			})
			if err != nil {
				t.Fatal(err)
			}
			digest := windowDigest(t, f.p, diskUsageBuckets)
			step, err := f.p.pruneWindowStep(context.Background(), windowTestCutoff, exported, DiskWindowLimits{Rows: 128, InputBytes: 8 << 20})
			if !errors.Is(err, ErrDiskUsageCorrupt) || step != (DiskWindowProgress{}) || windowDigest(t, f.p, diskUsageBuckets) != digest {
				t.Fatal("partial cleanup committed", step, err)
			}
		})
	}
	f := newDiskFailureFixture(t, windowRequest("bounds", false, false))
	exported := windowPublish(t, f)
	digest := windowDigest(t, f.p, diskUsageBuckets)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if step, err := f.p.pruneWindowStep(ctx, windowTestCutoff, exported, windowTestLimits); !errors.Is(err, context.Canceled) || step != (DiskWindowProgress{}) {
		t.Fatal(step, err)
	}
	if step, err := f.p.pruneWindowStep(context.Background(), windowTestCutoff, exported, DiskWindowLimits{Rows: 1, InputBytes: 1}); !errors.Is(err, ErrDiskUsageLimit) || step != (DiskWindowProgress{}) {
		t.Fatal(step, err)
	}
	if step, err := f.p.pruneWindowStep(context.Background(), windowTestCutoff, exported+1, windowTestLimits); !errors.Is(err, ErrDiskUsageCursor) || step != (DiskWindowProgress{}) {
		t.Fatal(step, err)
	}
	if windowDigest(t, f.p, diskUsageBuckets) != digest {
		t.Fatal("refused operation changed data")
	}
}

func TestDiskWindowKeepsAdmissionUntilCancelledHeavyCallbackActuallyExits(t *testing.T) {
	f := newDiskFailureFixture(t, windowRequest("busy", false, false))
	exported := windowPublish(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, err := f.p.readRequestDetailHTTP(ctx, "busy", diskHTTPDetailLimits(), func(context.Context, DiskRequestDetailQuery, DiskRequestDetailCursor, DiskUsagePageLimits) (DiskRequestDetailPage, error) {
			close(entered)
			<-release
			return DiskRequestDetailPage{}, context.Canceled
		})
		done <- err
	}()
	<-entered
	cancel()
	step, err := f.p.pruneWindowStep(context.Background(), windowTestCutoff, exported, windowTestLimits)
	if !errors.Is(err, ErrDiskUsageBusy) || step != (DiskWindowProgress{}) {
		close(release)
		<-done
		t.Fatal("cancel released actual job", step, err)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := f.p.pruneWindowStep(context.Background(), windowTestCutoff, exported, windowTestLimits); err != nil {
		t.Fatal(err)
	}
	entered, release, done = make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.p.read(context.Background(), func(*bbolt.DB) error { close(entered); <-release; return nil })
	}()
	<-entered
	if _, err := f.p.pruneWindowStep(context.Background(), windowTestCutoff, exported, windowTestLimits); !errors.Is(err, ErrDiskUsageBusy) {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDiskWindowLateSettlementDoesNotReviveFinalizedSummary(t *testing.T) {
	rows := diskFailureRequest("late", "provider_error", 1, true, false)
	f := newDiskFailureFixture(t, rows[:len(rows)-1])
	exported := windowPublish(t, f)
	f.a.PruneBefore(windowTestCutoff, exported)
	windowDrain(t, f.p, windowTestCutoff, exported, windowTestLimits)
	appendDiskDetailEvent(t, f, rows[len(rows)-1])
	stats, err := f.p.ReadStats(context.Background())
	if err != nil || stats.Totals != f.a.Snapshot().Totals || stats.Metrics != f.a.Metrics() || stats.Window.Attempts != 1 || stats.Window.Summaries != 0 {
		t.Fatal(stats, err)
	}
	page, err := f.p.RequestDetailPage(context.Background(), DiskRequestDetailQuery{RequestID: "late", Limit: 1}, DiskRequestDetailCursor{}, diskTestLimits)
	if err != nil || page.Found {
		t.Fatal("late settlement resurrected summary", page, err)
	}
	appendDiskDetailEvent(t, f, rows[len(rows)-1])
	after, err := f.p.ReadStats(context.Background())
	if err != nil || after.Totals != stats.Totals || after.Window.Attempts != 1 || after.Window.Summaries != 0 {
		t.Fatal(after, err)
	}
}

func TestDiskWindowArchiveCannotClaimDeletedAttemptPrefixIsComplete(t *testing.T) {
	events := windowRequest("old", false, false)
	events = append(events, windowRequest("new", true, true)...)
	f := newDiskFailureFixture(t, events)
	exported := windowPublish(t, f)
	windowDrain(t, f.p, windowTestCutoff, exported, windowTestLimits)
	stats, err := f.p.ReadStats(context.Background())
	if err != nil || stats.Window.AttemptAfter == 0 {
		t.Fatal(stats, err)
	}
	cut, err := f.p.StreamArchive(context.Background(), 0, archiveTestLimits, func(context.Context, []AttemptEvent) error { t.Fatal("deleted prefix was read"); return nil })
	if !errors.Is(err, ErrDiskUsageCursor) || cut != (DiskArchiveCut{}) {
		t.Fatal(cut, err)
	}
	var rows []AttemptEvent
	cut, err = f.p.StreamArchive(context.Background(), stats.Window.AttemptAfter, archiveTestLimits, func(_ context.Context, a []AttemptEvent) error { rows = append(rows, a...); return nil })
	if err != nil || len(rows) != 2 || cut.ThroughSequence != exported {
		t.Fatal(cut, err, rows)
	}
}

func TestDiskWindowByteBoundariesCommitProgressInsteadOfRetryingTheSameBatch(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 8; i++ {
		events = append(events, windowRequest(fmt.Sprint("bytes_", i), false, false)...)
	}
	f := newDiskFailureFixture(t, events)
	exported := windowPublish(t, f)
	a, s, versions, calls := windowDrain(t, f.p, windowTestCutoff, exported, DiskWindowLimits{Rows: 128, InputBytes: 3500})
	if a != 16 || s != 8 || versions == 0 || calls < 8 {
		t.Fatal(a, s, versions, calls)
	}
}
