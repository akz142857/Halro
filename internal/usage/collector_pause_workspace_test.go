package usage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// This exercises the original RAM pool, not a mock mutex standing in for its
// generation/child-retirement rules. Amounts are small because only ownership
// and ordering, not capacity acceptance, are being tested.
func collectorPauseWork(t *testing.T) (*resourcebudget.Manager, *resourcebudget.CompletableWork) {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1 << 20}, MaxClaims: 8, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	w, err := m.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 1}, resourcebudget.Cost{RAM: 100}, []resourcebudget.WorkspaceRequirement{{Class: resourcebudget.IO, RAM: 100}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(w.Close(), w.Active.Release()); err != nil {
			t.Error(err)
		}
		if got := m.Snapshot(); got.Used != (resourcebudget.Cost{}) || got.Owners != 0 || got.NormalClaims != 0 {
			t.Error("actual RAM pool did not retire", got)
		}
	})
	return m, w
}

func TestCollectorPausedWorkspaceJoinsBeforeLoanAndEndsLoanBeforeVisit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, work := collectorPauseWork(t)
	before := m.Snapshot()
	entered, release, visitEntered, releaseVisit := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once, visitOnce sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	unblockVisit := func() { visitOnce.Do(func() { close(releaseVisit) }) }
	defer unblock()
	defer unblockVisit()
	var calls atomic.Int32
	c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) {
		return ledger.Watermark{}, nil
	}, apply: func(context.Context, ledger.Record) error { return nil }}, func(ctx context.Context, _ ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
		if calls.Add(1) == 1 {
			err := work.WithWorkspace(ctx, func(ctx context.Context) error {
				p, err := m.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 100})
				if err != nil {
					return err
				}
				close(entered)
				<-release
				return p.Release()
			})
			return ledger.Watermark{}, err
		}
		return ledger.Watermark{}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); unblockVisit(); _ = c.Retire(context.Background()) })
	first, paused := make(chan error, 1), make(chan error, 1)
	go func() { first <- c.CatchUp(ctx) }()
	collectorSignal(t, entered)
	var borrowed atomic.Bool
	var expired context.Context
	go func() {
		paused <- c.WithPausedCatchUpWorkspace(ctx, func(ctx context.Context, catchUp func(context.Context) error) error {
			borrowed.Store(true)
			return work.WithWorkspace(ctx, func(ctx context.Context) error {
				expired = ctx
				return catchUp(ctx)
			})
		}, func(ctx context.Context) error {
			if _, err := m.TryAcquireContext(expired, resourcebudget.IO, resourcebudget.Cost{RAM: 1}); !errors.Is(err, resourcebudget.ErrClosed) {
				return errors.New("expired replay loan reached maintenance")
			}
			// A fresh loan must succeed here: carrying the catch-up loan into
			// a producer join would otherwise keep this original pool busy.
			if err := work.WithWorkspace(ctx, func(ctx context.Context) error {
				p, err := m.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 100})
				if err != nil {
					return err
				}
				return p.Release()
			}); err != nil {
				return err
			}
			close(visitEntered)
			<-releaseVisit
			return nil
		})
	}()
	collectorSlots(t, c, 2)
	if borrowed.Load() {
		t.Fatal("maintenance borrowed RAM before accepted reducer joined")
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	collectorSignal(t, visitEntered)
	if !borrowed.Load() || calls.Load() != 2 {
		t.Fatal("joined catch-up absent", calls.Load())
	}
	// A canceled new waiter must not run another replay during maintenance.
	waitCtx, cancelWait := context.WithCancel(ctx)
	waiter := make(chan error, 1)
	go func() { waiter <- c.CatchUp(waitCtx) }()
	collectorSlots(t, c, 2)
	cancelWait()
	if calls.Load() != 2 {
		t.Fatal("collector exclusion ended before maintenance")
	}
	unblockVisit()
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got := m.Snapshot(); got.Used != before.Used || got.NormalClaims != before.NormalClaims || got.Owners != before.Owners {
		t.Fatal("pause changed the actual persistent RAM fees", before, got)
	}
}

func TestCollectorPausedWorkspaceCancellationBeforeJoinDoesNotBorrow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }}, func(context.Context, ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error) {
		close(entered)
		<-release
		return ledger.Watermark{}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = c.Retire(context.Background()) })
	first := make(chan error, 1)
	go func() { first <- c.CatchUp(ctx) }()
	collectorSignal(t, entered)
	pauseCtx, cancelPause := context.WithCancel(ctx)
	defer cancelPause()
	paused := make(chan error, 1)
	go func() {
		paused <- c.WithPausedCatchUpWorkspace(pauseCtx, func(context.Context, func(context.Context) error) error {
			t.Error("canceled join borrowed RAM")
			return nil
		}, func(context.Context) error { t.Error("canceled join entered maintenance"); return nil })
	}()
	collectorSlots(t, c, 2)
	cancelPause()
	select {
	case err := <-paused:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("paused join ignored cancellation")
	}
	if len(c.slots) != 1 {
		t.Fatal("canceled pause released accepted source", len(c.slots))
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestCollectorPausedWorkspaceFailuresKeepVisitorExcludedAndReleaseLocks(t *testing.T) {
	fault := errors.New("original RAM loan failed")
	c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }}, func(context.Context, ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error) {
		return ledger.Watermark{}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Retire(context.Background())
	for _, duringReplay := range []bool{false, true} {
		c.target = collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) {
			if duringReplay {
				return ledger.Watermark{}, fault
			}
			return ledger.Watermark{}, nil
		}}
		err := c.WithPausedCatchUpWorkspace(context.Background(), func(ctx context.Context, catchUp func(context.Context) error) error {
			if duringReplay {
				return catchUp(ctx)
			}
			return fault
		}, func(context.Context) error { t.Fatal("failed catch-up entered maintenance"); return nil })
		if !errors.Is(err, fault) || len(c.slots) != 0 {
			t.Fatal("failed pause retained locks/slot", err, len(c.slots))
		}
	}
	c.target = collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }}
	if err := c.CatchUp(context.Background()); err != nil {
		t.Fatal("collector could not resume", err)
	}
}

func TestCollectorPausedWorkspaceRejectsMissingRepeatedAndEscapedReplay(t *testing.T) {
	var scans atomic.Int32
	c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }}, func(context.Context, ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error) {
		scans.Add(1)
		return ledger.Watermark{}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Retire(context.Background())
	var expired func(context.Context) error
	if err := c.WithPausedCatchUpWorkspace(context.Background(), func(context.Context, func(context.Context) error) error { return nil }, func(context.Context) error {
		t.Fatal("missing catch-up entered maintenance")
		return nil
	}); !errors.Is(err, ErrCollectorHead) {
		t.Fatal(err)
	}
	if err := c.WithPausedCatchUpWorkspace(context.Background(), func(ctx context.Context, catchUp func(context.Context) error) error {
		expired = catchUp
		if err := catchUp(ctx); err != nil {
			return err
		}
		if err := catchUp(ctx); !errors.Is(err, ErrCollectorBusy) {
			t.Fatal("repeated replay loan accepted", err)
		}
		return nil
	}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := expired(context.Background()); !errors.Is(err, ErrCollectorBusy) || scans.Load() != 1 {
		t.Fatal("expired replay reentered reducer", err, scans.Load())
	}
}

func TestCollectorPausedWorkspaceJoinsEnteredReplayBeforeRetirement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, providerReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }}, func(context.Context, ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error) {
		close(entered)
		<-release // actual IO deliberately remains live after cancellation
		return ledger.Watermark{}, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = c.Retire(context.Background()) })
	paused, child := make(chan error, 1), make(chan error, 1)
	go func() {
		paused <- c.WithPausedCatchUpWorkspace(ctx, func(ctx context.Context, catchUp func(context.Context) error) error {
			go func() { child <- catchUp(ctx) }()
			<-entered
			close(providerReturned)
			return nil // deliberately violates the provider contract
		}, func(context.Context) error { t.Error("retired collector entered maintenance"); return nil })
	}()
	collectorSignal(t, providerReturned)
	retireCtx, cancelRetire := context.WithCancel(ctx)
	cancelRetire()
	if err := c.Retire(retireCtx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case err := <-paused:
		t.Fatal("pause released accepted replay before real IO exit", err)
	default:
	}
	if len(c.slots) != 1 {
		t.Fatal("retirement erased accepted replay slot")
	}
	unblock()
	if err := <-child; !errors.Is(err, ErrCollectorRetired) && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-paused; !errors.Is(err, context.Canceled) && !errors.Is(err, ErrCollectorRetired) {
		t.Fatal(err)
	}
	if err := c.Retire(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorPausedWorkspaceCallerCancelStillJoinsAcceptedIO(t *testing.T) {
	m, work := collectorPauseWork(t)
	before := m.Snapshot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }}, func(ctx context.Context, _ ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
		p, err := m.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 100})
		if err != nil {
			return ledger.Watermark{}, err
		}
		defer p.Release()
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return ledger.Watermark{}, ctx.Err()
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = c.Retire(context.Background()) })
	paused := make(chan error, 1)
	go func() {
		paused <- c.WithPausedCatchUpWorkspace(ctx, func(ctx context.Context, catchUp func(context.Context) error) error {
			return work.WithWorkspace(ctx, func(ctx context.Context) error {
				// A provider cannot strip the caller's cancellation authority.
				return catchUp(context.WithoutCancel(ctx))
			})
		}, func(context.Context) error { t.Error("canceled catch-up entered maintenance"); return nil })
	}()
	collectorSignal(t, entered)
	cancel()
	collectorSignal(t, canceled)
	select {
	case err := <-paused:
		t.Fatal("caller cancel released actual IO before exit", err)
	default:
	}
	if err := work.Close(); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("Close erased live original pool", err)
	}
	if got := m.Snapshot(); got.Used != before.Used || len(c.slots) != 1 {
		t.Fatal("caller cancel erased owned fees", got, len(c.slots))
	}
	unblock()
	if err := <-paused; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
