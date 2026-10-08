package metadatastate

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"testing"
	"testing/synctest"
	"time"
)

func TestMetadataBusinessQueueDeadlineRetainsActualRootAndSourceFees(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, s, o := metadataFacadeFixture(t)
		defer s.Close()
		ctx := context.Background()
		root, err := b.store.RootIdentity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		before := o.Budget.Snapshot()
		b.businessLane <- struct{}{}
		done := make(chan error, 1)
		go func() { _, err := s.GetProject(ctx, "absent"); done <- err }()
		synctest.Wait()
		if b.businessWaiters.Load() != 1 || o.Budget.Snapshot().Used.RAM <= before.Used.RAM {
			t.Fatal("missing paid waiter")
		}
		time.Sleep(resourcebudget.MaxWorkQueueWait)
		synctest.Wait()
		if err := <-done; !errors.Is(err, resourcebudget.ErrWorkQueueWait) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		after := o.Budget.Snapshot()
		if after.Used != before.Used || after.Owners != before.Owners || after.NormalClaims != before.NormalClaims || b.businessWaiters.Load() != 0 {
			t.Fatal("expired waiter lost source ownership", before, after)
		}
		<-b.businessLane
		if actual, err := b.store.RootIdentity(ctx); err != nil || actual != root {
			t.Fatal(actual, root, err)
		}
		f := &standaloneFacade{owner: b}
		if err := f.View(ctx, func(boltstore.FacadeTransaction) error {
			time.Sleep(resourcebudget.MaxWorkQueueWait + time.Second)
			return ctx.Err()
		}); err != nil {
			t.Fatal("entered business transaction canceled by queue timer", err)
		}
	})
}
