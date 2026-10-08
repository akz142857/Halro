package usage

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// These fault reducers test admission/errors only; semantic equivalence below
// uses real MAC-authenticated native facts and the selected disk Ledger owner.
type collectorFaultTarget struct {
	read  func(context.Context) (ledger.Watermark, error)
	apply func(context.Context, ledger.Record) error
}

func (f collectorFaultTarget) ReadWatermark(ctx context.Context) (ledger.Watermark, error) {
	return f.read(ctx)
}
func (f collectorFaultTarget) ApplyContext(ctx context.Context, r ledger.Record) error {
	return f.apply(ctx, r)
}

func collectorSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("collector phase did not arrive")
	}
}
func collectorSlots(t *testing.T, c *Collector, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(c.slots) != n {
		if time.Now().After(deadline) {
			t.Fatalf("slots=%d want=%d", len(c.slots), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProjectedCollectorNativeDiskRetryAndBorrowedNotification(t *testing.T) {
	ctx := context.Background()
	events := append(diskFailureRequest("normal", "success", 2, false, true), diskFailureRequest("late", "recovered", 1, true, false)...)
	events = append(events, events[0]) // native duplicate preserves first-event semantics
	f := newDiskFailureFixture(t, events)
	d, p, err := NewDiskLedgerDerivative(f.owner, filepath.Join(t.TempDir(), "collected.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); _ = p.Close() })
	allowed, _, err := f.owner.ReadPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("interrupted authoritative source")
	first := true
	var scans int
	c, err := NewProjectedCollector(d, func(ctx context.Context, from ledger.Watermark, visit func(ledger.Record) error) (ledger.Watermark, error) {
		scans++
		count := 0
		_, err := f.native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
			position := ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}
			if !position.After(from) || position.After(allowed) {
				return nil
			}
			if err := visit(r); err != nil {
				return err
			}
			count++
			if first && count == 5 {
				first = false
				return fault
			}
			return nil
		})
		if err != nil {
			return ledger.Watermark{}, err
		}
		return allowed, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Retire(ctx) })
	// The notification's body is deliberately unrelated and then mutated. Only
	// the streamed source supplies facts; no queued payload may affect reduction.
	borrowed := ledger.Record{Event: events[0], Sequence: 999}
	for i := 0; i < 40; i++ {
		c.Observe(borrowed)
	}
	borrowed.Event.ProjectID = "forged"
	if c.Stats().QueueDepth != 1 || c.Stats().Dropped != 39 {
		t.Fatal(c.Stats())
	}
	if err := c.CatchUp(ctx); !errors.Is(err, fault) || !c.Stats().Lagging {
		t.Fatal("source failure masked", err, c.Stats())
	}
	head, err := d.ReadWatermark(ctx)
	if err != nil || head.Sequence != 5 {
		t.Fatal("partial committed position", head, err)
	}
	if err := c.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReadStats(ctx)
	want, wantErr := f.p.ReadStats(ctx)
	if err != nil || wantErr != nil || got != want || c.Stats().Lagging || scans != 2 {
		t.Fatal("replay differs", got, want, err, wantErr, c.Stats(), scans)
	}
	period := Period{ID: events[0].PeriodID, TimezoneVersion: 1}
	now := events[0].OccurredAt.Add(time.Hour)
	dashboard, err := p.ReadDashboard(ctx, now, period, diskDashboardTestLimits)
	if err != nil || !reflect.DeepEqual(dashboard.Dashboard, f.a.Dashboard(now, period)) {
		t.Fatal("selected-owner dashboard differs", err)
	}
	if err := c.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadStats(ctx); err != nil {
		t.Fatal("collector closed borrowed DB", err)
	}
	if _, err := d.ReadWatermark(ctx); err != nil {
		t.Fatal("collector retired borrowed reducer", err)
	}
}

func TestProjectedCollectorWatermarkErrorsNeverFallBack(t *testing.T) {
	fault := errors.New("selected reducer unavailable")
	for _, phase := range []string{"initial", "final", "head"} {
		t.Run(phase, func(t *testing.T) {
			reads, scans := 0, 0
			head := ledger.Watermark{Generation: 1, Sequence: 4, Offset: 100}
			target := collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) {
				reads++
				if phase == "initial" || (phase == "final" && reads == 2) {
					return ledger.Watermark{}, fault
				}
				return head, nil
			}, apply: func(context.Context, ledger.Record) error { t.Fatal("no suffix expected"); return nil }}
			c, err := NewProjectedCollector(target, func(_ context.Context, from ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
				scans++
				if from != head {
					t.Fatal("fallback to genesis", from)
				}
				out := head
				if phase == "head" {
					out.Sequence++
				}
				return out, nil
			}, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Retire(context.Background())
			err = c.CatchUp(context.Background())
			want := fault
			if phase == "head" {
				want = ErrCollectorHead
			}
			if !errors.Is(err, want) || !c.Stats().Lagging || (phase == "initial" && scans != 0) {
				t.Fatal(err, c.Stats(), scans)
			}
		})
	}
}

func TestProjectedCollectorNotificationDuringScanKeepsWakeAndReturns(t *testing.T) {
	var c *Collector
	head := ledger.Watermark{Generation: 1, Sequence: 1, Offset: 1}
	target := collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return head, nil }, apply: func(context.Context, ledger.Record) error { return nil }}
	var scans int
	var err error
	c, err = NewProjectedCollector(target, func(context.Context, ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error) {
		scans++
		if scans == 1 {
			for i := 0; i < 1000; i++ {
				c.Observe(ledger.Record{})
			}
		}
		return head, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Retire(context.Background())
	if err := c.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scans != 1 || !c.Stats().Lagging || c.Stats().QueueDepth != 1 {
		t.Fatal("new wake lost or scan extended", scans, c.Stats())
	}
	// The existing Run consumes that wake without another append or HTTP poll.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for c.Stats().Lagging {
		if time.Now().After(deadline) {
			t.Fatal("Run missed wake")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	collectorSignal(t, done)
	if scans != 2 {
		t.Fatal("unexpected replay count", scans)
	}
}

func TestProjectedCollectorRetirementJoinsAdmittedWaiterAndBlockedIO(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	canceled := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	target := collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }, apply: func(context.Context, ledger.Record) error { return nil }}
	c, err := NewProjectedCollector(target, func(ctx context.Context, _ ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release // intentionally ignores cancellation until real IO exits
		return ledger.Watermark{}, ctx.Err()
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	t.Cleanup(func() { unblock(); _ = c.Retire(context.Background()) })
	go func() { first <- c.CatchUp(context.Background()) }()
	collectorSignal(t, entered)
	go func() { second <- c.CatchUp(context.Background()) }()
	collectorSlots(t, c, 2)
	if err := c.CatchUp(context.Background()); !errors.Is(err, ErrCollectorBusy) {
		t.Fatal("unbounded catch-up admission", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Retire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	collectorSignal(t, canceled)
	select {
	case <-c.retiredDone:
		t.Fatal("retirement released blocked IO")
	default:
	}
	if len(c.slots) != 2 {
		t.Fatal("cancellation released admitted work", len(c.slots))
	}
	if err := c.CatchUp(context.Background()); !errors.Is(err, ErrCollectorRetired) {
		t.Fatal(err)
	}
	before := c.Stats()
	c.Observe(ledger.Record{})
	c.Run(context.Background())
	if c.Stats() != before {
		t.Fatal("retired notification admitted")
	}
	unblock()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, ErrCollectorRetired) {
		t.Fatal(err)
	}
	if err := c.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.slots) != 0 || c.target != nil || c.replay != nil || calls.Load() != 1 {
		t.Fatal("retirement did not join/clear owners", len(c.slots), calls.Load())
	}
}

func TestProjectedCollectorRetirementJoinsRun(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }, apply: func(context.Context, ledger.Record) error { return nil }}, func(ctx context.Context, _ ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
		close(entered)
		<-release
		return ledger.Watermark{}, ctx.Err()
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = c.Retire(context.Background()) })
	c.Observe(ledger.Record{})
	done := make(chan struct{})
	go func() { c.Run(context.Background()); close(done) }()
	collectorSignal(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Retire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("Run exited before actual source IO")
	default:
	}
	unblock()
	collectorSignal(t, done)
	if err := c.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.slots) != 0 || c.target != nil || c.replay != nil {
		t.Fatal("Run retained owner")
	}
}

func TestProjectedCollectorSelectedDiskWatermarkFailureRefusesSource(t *testing.T) {
	f := newDiskFailureFixture(t, diskFailureRequest("selected_corrupt", "success", 1, false, true))
	// Explicit derived fault: an invalid selected DB head must never silently
	// choose the independently populated Aggregate/reference or replay genesis.
	if err := f.p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("state"), []byte("invalid")) }); err != nil {
		t.Fatal(err)
	}
	var scans int
	c, err := NewProjectedCollector(f.d, func(context.Context, ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error) {
		scans++
		return ledger.Watermark{}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Retire(context.Background())
	if err := c.CatchUp(context.Background()); err == nil || scans != 0 || !c.Stats().Lagging {
		t.Fatal("selected read failure fell back", err, scans, c.Stats())
	}
	if f.a.Watermark().Sequence == 0 {
		t.Fatal("reference must have historical data")
	}
}

func TestProjectedCollectorRunRetainsWakeWhenSlotsFullAndWaiterCanceled(t *testing.T) {
	oldEntered, releaseOld, newEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseOld) }) }
	var sequence atomic.Uint64
	var calls atomic.Int32
	target := collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) {
		return ledger.Watermark{Generation: 1, Sequence: sequence.Load()}, nil
	}, apply: func(_ context.Context, r ledger.Record) error { sequence.Store(r.Sequence); return nil }}
	c, err := NewProjectedCollector(target, func(ctx context.Context, from ledger.Watermark, visit func(ledger.Record) error) (ledger.Watermark, error) {
		if calls.Add(1) == 1 {
			close(oldEntered)
			<-releaseOld
			return ledger.Watermark{Generation: 1}, nil
		}
		if from.Sequence != 0 {
			t.Error("wrong old head", from)
		}
		if err := visit(ledger.Record{Sequence: 1}); err != nil {
			return ledger.Watermark{}, err
		}
		close(newEntered)
		return ledger.Watermark{Generation: 1, Sequence: 1}, ctx.Err()
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { unblock(); cancel(); _ = c.Retire(context.Background()) })
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- c.CatchUp(context.Background()) }()
	collectorSignal(t, oldEntered)
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	go func() { second <- c.CatchUp(waitCtx) }()
	collectorSlots(t, c, 2)
	cancelWait()
	c.Observe(ledger.Record{Sequence: 1})
	runDone := make(chan struct{})
	go func() { c.Run(ctx); close(runDone) }()
	deadline := time.Now().Add(5 * time.Second)
	for c.Stats().QueueDepth != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not retain wake")
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 || len(c.slots) != 2 {
		t.Fatal("fixture did not hold old scan and waiter")
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	collectorSignal(t, newEntered) // no further Observe or external CatchUp
	cancel()
	collectorSignal(t, runDone)
	if sequence.Load() != 1 || calls.Load() != 2 {
		t.Fatal("unique suffix wake lost", sequence.Load(), calls.Load())
	}
}
