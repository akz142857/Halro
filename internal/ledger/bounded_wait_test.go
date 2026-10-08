package ledger

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestSelectedBusinessReadQueueAndOwnerLockHaveHardDeadline(t *testing.T) {
	for _, holdLane := range []bool{true, false} {
		name := "owner_lock"
		if holdLane {
			name = "lane"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "native.wal")
				native := openChained(t, path, nil)
				defer native.Close()
				report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
				if err != nil || partial {
					t.Fatal(partial, err)
				}
				opts := authenticatedLedgerTestOptions(t)
				p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "root"), report, opts)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Discard()
				p.businessReadLane = make(chan struct{}, 1)
				before := opts.Budget.Snapshot()
				anchor, err := p.ReadReplayAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if holdLane {
					p.businessReadLane <- struct{}{}
				} else {
					p.mu.Lock()
				}
				done := make(chan error, 1)
				go func() {
					release, err := p.admitBusinessRead(ctx)
					if release != nil {
						err = errors.Join(err, release())
						t.Error("expired read entered owner")
					}
					done <- err
				}()
				synctest.Wait()
				if p.businessReadWaiters.Load() != 1 || opts.Budget.Snapshot().Used.RAM <= before.Used.RAM {
					t.Fatal("waiter has no actual ticket")
				}
				time.Sleep(resourcebudget.MaxWorkQueueWait)
				synctest.Wait()
				if err := <-done; !errors.Is(err, resourcebudget.ErrWorkQueueWait) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				if holdLane {
					<-p.businessReadLane
				} else {
					p.mu.Unlock()
				}
				after := opts.Budget.Snapshot()
				if after.Used != before.Used || after.NormalClaims != before.NormalClaims || after.Owners != before.Owners || p.businessReadWaiters.Load() != 0 || p.readFailure.Load() != nil {
					t.Fatal("queue timeout lost fees or poisoned reader", before, after)
				}
				if actual, err := p.ReadReplayAnchor(ctx); err != nil || actual != anchor {
					t.Fatal(actual, anchor, err)
				}
				release, err := p.admitBusinessRead(ctx)
				if err != nil {
					t.Fatal("same owner retry", err)
				}
				time.Sleep(resourcebudget.MaxWorkQueueWait + time.Second)
				if len(p.businessReadLane) != 1 || p.businessReadWaiters.Load() != 1 {
					t.Fatal("entered read lost ownership")
				}
				if err := release(); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
func TestMaintenanceQueueDeadlineDoesNotRevokeAcceptedNativeOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		l := openChained(t, filepath.Join(t.TempDir(), "native.wal"), nil)
		defer l.Close()
		lease, err := l.BeginMaintenance(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		lease.gate <- struct{}{}
		err = lease.WithHead(ctx, func(context.Context, Watermark, [32]byte) error {
			t.Error("expired callback entered native read")
			return nil
		})
		if !errors.Is(err, resourcebudget.ErrWorkQueueWait) {
			t.Fatal(err)
		}
		if _, err := lease.ValidateOwner(l); err != nil {
			t.Fatal("timeout revoked actual native owner", err)
		}
		<-lease.gate
		if err := lease.WithHead(ctx, func(ctx context.Context, _ Watermark, _ [32]byte) error {
			time.Sleep(resourcebudget.MaxWorkQueueWait + time.Second)
			return ctx.Err()
		}); err != nil {
			t.Fatal("queue timer reached native callback", err)
		}
	})
}
