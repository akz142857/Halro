package replication

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestMemberReadAuthorityQueueHardDeadlineRetiresWaiterAndPreservesActiveAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, _ := memberReadAuthorityFixture(t)
		defer p.Close()
		b := queuedMemberAuthorityBudget(t)
		entered, release := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- p.WithReadAuthority(context.Background(), b, func(ctx context.Context, _ MemberState, authorize func(func() error) error) error {
				close(entered)
				<-release
				if ctx.Err() != nil {
					t.Error("queue timeout canceled active authority")
				}
				return authorize(func() error { return nil })
			})
		}()
		<-entered
		before := b.Snapshot()
		second := make(chan error, 1)
		go func() {
			second <- p.WithReadAuthority(context.Background(), b, func(context.Context, MemberState, func(func() error) error) error {
				t.Error("expired member queue entered callback")
				return nil
			})
		}()
		synctest.Wait()
		if b.Snapshot().Used.RAM <= before.Used.RAM {
			t.Fatal("waiter had no actual whole reader fee")
		}
		time.Sleep(resourcebudget.MaxWorkQueueWait)
		synctest.Wait()
		if err := <-second; !errors.Is(err, resourcebudget.ErrWorkQueueWait) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		p.mu.Lock()
		count := 0
		for _, job := range p.authorityJobs {
			if job != nil {
				count++
			}
		}
		fault := p.authorityFault
		p.mu.Unlock()
		if count != 1 || fault != nil {
			t.Fatal("queue timeout withdrew active reader or latched publication fault", count, fault)
		}
		if after := b.Snapshot(); after.Used != before.Used || after.Owners != before.Owners {
			t.Fatal("queue timeout refunded active source", before, after)
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal("original authority could not finish", err)
		}
		if err := p.WithReadAuthority(context.Background(), b, func(_ context.Context, _ MemberState, authorize func(func() error) error) error {
			return authorize(func() error { return nil })
		}); err != nil {
			t.Fatal("fresh reader could not enter after timeout", err)
		}
		if b.Snapshot().Used != (resourcebudget.Cost{}) {
			t.Fatal("member reader leaked fees", b.Snapshot())
		}
	})
}
