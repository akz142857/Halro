package usage

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"testing"
	"testing/synctest"
	"time"
)

func TestCollectorReducerWaitHasDeadlineAndDoesNotCancelEnteredReplay(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "catch_up"
		if paused {
			name = "paused"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) { return ledger.Watermark{}, nil }, apply: func(context.Context, ledger.Record) error { return nil }}, func(ctx context.Context, _ ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
					calls++
					time.Sleep(resourcebudget.MaxWorkQueueWait + time.Second)
					return ledger.Watermark{}, ctx.Err()
				}, 1)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Retire(context.Background())
				c.applyMu.Lock()
				operation := func() error {
					if paused {
						return c.WithPausedCatchUp(context.Background(), func(context.Context) error { return nil })
					}
					return c.CatchUp(context.Background())
				}
				done := make(chan error, 1)
				go func() { done <- operation() }()
				synctest.Wait()
				if len(c.slots) != 1 {
					t.Fatal("waiter slot absent")
				}
				time.Sleep(resourcebudget.MaxWorkQueueWait)
				synctest.Wait()
				if err := <-done; !errors.Is(err, resourcebudget.ErrWorkQueueWait) {
					t.Fatal(err)
				}
				if calls != 0 || len(c.slots) != 0 || c.retired.Load() {
					t.Fatal("timeout entered replay or revoked collector")
				}
				c.applyMu.Unlock()
				if err := operation(); err != nil || calls != 1 {
					t.Fatal("entered replay inherited queue deadline", calls, err)
				}
			})
		})
	}
}
func TestCollectorOwnedSlotWaitHasDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := &Collector{slots: make(chan struct{}, 2), stopCtx: ctx}
		c.slots <- struct{}{}
		c.slots <- struct{}{}
		if err := c.catchUpOwned(context.Background()); !errors.Is(err, resourcebudget.ErrWorkQueueWait) || len(c.slots) != 2 {
			t.Fatal("timeout withdrew accepted slots", err)
		}
	})
}

func TestCollectorRunPreservesWakeAfterQueueDeadlineWithoutAnotherEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		c, err := NewProjectedCollector(collectorFaultTarget{read: func(context.Context) (ledger.Watermark, error) {
			return ledger.Watermark{}, nil
		}, apply: func(context.Context, ledger.Record) error { return nil }}, func(context.Context, ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error) {
			calls++
			return ledger.Watermark{}, nil
		}, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Retire(context.Background())
		c.slots <- struct{}{}
		c.slots <- struct{}{}
		c.Observe(ledger.Record{})
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		synctest.Wait()
		time.Sleep(resourcebudget.MaxWorkQueueWait + time.Second)
		synctest.Wait()
		if calls != 0 || len(c.slots) != 2 {
			t.Fatal("timeout displaced accepted slots", calls, len(c.slots))
		}
		<-c.slots
		<-c.slots
		synctest.Wait()
		if calls != 1 || c.lagging.Load() {
			t.Fatal("original wake lost until another event", calls, c.lagging.Load())
		}
		cancel()
		<-done
	})
}
func TestTypedUsageViewQueueDeadlineRetainsRootAndCanRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newTypedUsageFixture(t, false)
		defer f.native.Close()
		defer f.lease.Close()
		defer f.owner.Discard()
		p, _, err := BuildTypedUsageProjection(context.Background(), f.options, f.lease)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Discard()
		p.engine.viewLane = make(chan struct{}, 1)
		p.engine.viewLane <- struct{}{}
		root, err := p.engine.cow.RootIdentity(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := p.engine.View(func(*usageTx) error { t.Error("expired view entered root"); return nil }); !errors.Is(err, resourcebudget.ErrWorkQueueWait) {
			t.Fatal(err)
		}
		<-p.engine.viewLane
		if actual, err := p.engine.cow.RootIdentity(context.Background()); err != nil || actual != root {
			t.Fatal(actual, root, err)
		}
		if err := p.engine.View(func(row *usageTx) error {
			time.Sleep(resourcebudget.MaxWorkQueueWait + time.Second)
			return row.ctx.Err()
		}); err != nil {
			t.Fatal("entered view inherited queue timer", err)
		}
	})
}
