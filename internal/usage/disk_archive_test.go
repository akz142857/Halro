package usage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

var archiveTestLimits = DiskArchiveLimits{Pages: 4096, Rows: 65536, InputBytes: 64 << 20, OutputBytes: 64 << 20, PageRows: 160, PageInputBytes: 8 << 20, PageOutputBytes: 8 << 20, MaxDuration: time.Minute}

func TestDiskArchiveAuthenticatedCompletePrefixNativeOrderAndWriterBetweenPages(t *testing.T) {
	var events []ledger.Event
	for i := 0; i < 166; i++ {
		rows := diskFailureRequest(fmt.Sprint("archive_", i), "success", 1, false, true)
		for j := range rows {
			rows[j].OccurredAt = rows[j].OccurredAt.Add(time.Duration(i%2) * 24 * time.Hour)
		}
		events = append(events, rows...)
	}
	f := newDiskFailureFixture(t, events)
	want := f.a.Snapshot().Attempts
	head := f.a.Watermark()
	after := want[3].Sequence
	limits := archiveTestLimits
	limits.PageRows = 10
	var got []AttemptEvent
	cut, err := f.p.StreamArchive(context.Background(), after, limits, func(ctx context.Context, page []AttemptEvent) error {
		if f.p.db.Stats().OpenTxN != 0 || len(f.p.readers) != 0 || len(page) > limits.PageRows {
			t.Fatal("visitor owns a DB transaction or unbounded page")
		}
		if len(got) == 0 {
			// A real writer completes inside the sink callback. Its facts are
			// newer than the frozen H and must be absent from this complete cut.
			for _, e := range diskFailureRequest("archive_future", "success", 1, false, true) {
				appendDiskDetailEvent(t, f, e)
			}
		}
		got = append(got, page...)
		return nil
	})
	if err != nil || cut.Head != head || cut.AfterSequence != after || cut.Rows != len(want)-4 || cut.Pages < 2 || !reflect.DeepEqual(got, want[4:]) {
		t.Fatal("source did not cover the frozen native prefix", cut, err, len(got), len(want))
	}
	if cut.Head.Sequence <= got[len(got)-1].Sequence || cut.Head.Sequence >= f.a.Watermark().Sequence {
		t.Fatal("receipt uses a date/last-attempt frontier instead of full H", cut)
	}
	cut, err = f.p.StreamArchive(context.Background(), f.a.Watermark().Sequence, limits, func(context.Context, []AttemptEvent) error {
		t.Fatal("empty complete prefix produced rows")
		return nil
	})
	if err != nil || cut.Rows != 0 || cut.Pages != 1 {
		t.Fatal("empty interval could not prove completion", cut, err)
	}
}

func TestDiskArchiveWholeBudgetSinkFailureAndCorruptionNeverProduceReceipt(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("archive_budget", "success", 3, false, true))
	for _, edit := range []func(*DiskArchiveLimits){func(l *DiskArchiveLimits) { l.PageRows = 1; l.Pages = 1 }, func(l *DiskArchiveLimits) { l.Rows = 1 }, func(l *DiskArchiveLimits) { l.InputBytes = 1 }, func(l *DiskArchiveLimits) { l.OutputBytes = 1 }, func(l *DiskArchiveLimits) { l.PageInputBytes = 1 }, func(l *DiskArchiveLimits) { l.PageOutputBytes = 1 }, func(l *DiskArchiveLimits) { l.MaxDuration = 0 }} {
		limits := archiveTestLimits
		edit(&limits)
		cut, err := f.p.StreamArchive(context.Background(), 0, limits, func(context.Context, []AttemptEvent) error { return nil })
		if !errors.Is(err, ErrDiskUsageLimit) || cut != (DiskArchiveCut{}) {
			t.Fatal("incomplete source produced receipt", cut, err)
		}
	}
	sentinel := errors.New("sink publication failed")
	cut, err := f.p.StreamArchive(context.Background(), 0, archiveTestLimits, func(context.Context, []AttemptEvent) error { return sentinel })
	if !errors.Is(err, sentinel) || cut != (DiskArchiveCut{}) {
		t.Fatal(cut, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cut, err = f.p.StreamArchive(ctx, 0, archiveTestLimits, func(context.Context, []AttemptEvent) error { t.Fatal("canceled visitor"); return nil })
	if !errors.Is(err, context.Canceled) || cut != (DiskArchiveCut{}) {
		t.Fatal(cut, err)
	}
	cut, err = f.p.StreamArchive(context.Background(), f.a.Watermark().Sequence+1, archiveTestLimits, func(context.Context, []AttemptEvent) error { t.Fatal("ahead visitor"); return nil })
	if !errors.Is(err, ErrDiskUsageCursor) || cut != (DiskArchiveCut{}) {
		t.Fatal(cut, err)
	}
	if err := f.p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("attempts"))
		k, _ := b.Cursor().First()
		return b.Put(k, []byte("broken"))
	}); err != nil {
		t.Fatal(err)
	}
	cut, err = f.p.StreamArchive(context.Background(), 0, archiveTestLimits, func(context.Context, []AttemptEvent) error { t.Fatal("corrupt visitor"); return nil })
	if !errors.Is(err, ErrDiskUsageCorrupt) || cut != (DiskArchiveCut{}) {
		t.Fatal(cut, err)
	}
}

func TestDiskArchiveCloseJoinsPhysicalSinkAndRejectsReceiptAfterCancellation(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("archive_join", "success", 1, false, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		cut, err := f.p.StreamArchive(ctx, 0, archiveTestLimits, func(context.Context, []AttemptEvent) error { close(entered); <-release; return nil })
		if cut != (DiskArchiveCut{}) {
			err = errors.Join(err, errors.New("retired source produced a receipt"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sink did not begin")
	}
	_, err := f.p.StreamArchive(context.Background(), 0, archiveTestLimits, func(context.Context, []AttemptEvent) error { return nil })
	if !errors.Is(err, ErrDiskUsageBusy) {
		t.Fatal("heavy source queued", err)
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- f.p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.p.retired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatal("Close passed physical sink", err)
	default:
	}
	if f.p.db.Stats().OpenTxN != 0 {
		t.Fatal("sink kept a transaction")
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
