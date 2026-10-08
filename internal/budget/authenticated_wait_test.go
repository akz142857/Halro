package budget

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestAuthenticatedAppendQueueHardDeadlinePreservesNativeHeadAndRefundsOnlyTicket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p, _, resources := newAuthenticatedManagerFixtureWithResources(t, ledger.Options{MaxBatch: 1})
		owner := m.owner.(authenticatedAccountingOwner)
		defer owner.native.Close()
		defer p.Discard()
		head, hash, _ := owner.native.ChainHead()
		before := resources.Snapshot()
		owner.appendGate.lane <- struct{}{}
		done := make(chan error, 1)
		go func() {
			done <- owner.withOwnedAppendWorkspace(context.Background(), func(context.Context) error { t.Error("timed-out append entered native callback"); return nil })
		}()
		synctest.Wait()
		if owner.appendGate.waiters.Load() != 1 || resources.Snapshot().Used.RAM <= before.Used.RAM {
			t.Fatal("waiting append did not retain actual paid ticket")
		}
		time.Sleep(resourcebudget.MaxWorkQueueWait)
		synctest.Wait()
		if err := <-done; !errors.Is(err, resourcebudget.ErrWorkQueueWait) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if actual, actualHash, _ := owner.native.ChainHead(); actual != head || actualHash != hash || owner.native.Status().Load() != ledger.AccountingHealthy {
			t.Fatal("queue timeout changed or poisoned native facts", actual)
		}
		if after := resources.Snapshot(); after.Used != before.Used || after.Owners != before.Owners || after.NormalClaims != before.NormalClaims || owner.appendGate.waiters.Load() != 0 {
			t.Fatal("expired queue retained ticket or refunded source fee", before, after)
		}
		<-owner.appendGate.lane
		if err := owner.withOwnedAppendWorkspace(context.Background(), func(ctx context.Context) error {
			time.Sleep(resourcebudget.MaxWorkQueueWait + time.Second)
			return ctx.Err()
		}); err != nil {
			t.Fatal("queue timer canceled already entered append", err)
		}
	})
}
