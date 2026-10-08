package resourcebudget

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestSharedWorkspaceWaitHasHardDeadlineWithoutRefundingAcceptedWork(t *testing.T) {
	for _, callerWait := range []time.Duration{0, time.Second, time.Hour} {
		t.Run(callerWait.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, s, ws := sharedFixture(t, 2)
				entered, release := make(chan struct{}), make(chan struct{})
				rootDone := make(chan error, 1)
				go func() {
					rootDone <- ws[0].WithWorkspace(context.Background(), func(ctx context.Context) error {
						close(entered)
						<-release
						p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 200})
						if err != nil {
							return err
						}
						return p.Release()
					})
				}()
				<-entered
				m.SetPressure(Pressured)
				m.CloseAdmission()
				before := m.Snapshot()
				ctx := context.Background()
				if callerWait > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, callerWait)
					defer cancel()
				}
				done := make(chan error, 1)
				go func() {
					done <- ws[1].WithWorkspace(ctx, func(context.Context) error { t.Error("timed-out waiter ran callback"); return nil })
				}()
				synctest.Wait()
				if stats := s.Snapshot(); stats.Waiting != 1 || !stats.InUse {
					t.Fatal(stats)
				}
				elapsed := MaxWorkQueueWait
				if callerWait > 0 && callerWait < elapsed {
					elapsed = callerWait
				}
				time.Sleep(elapsed)
				synctest.Wait()
				err := <-done
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				ownDeadline := callerWait == 0 || callerWait > MaxWorkQueueWait
				if errors.Is(err, ErrWorkQueueWait) != ownDeadline {
					t.Fatal("lost distinction between owner/caller deadline", err)
				}
				if ownDeadline && ctx.Err() != nil {
					t.Fatal("queue timeout canceled original owner", ctx.Err())
				}
				if stats := s.Snapshot(); stats.Waiting != 0 || !stats.InUse || stats.Members != 2 {
					t.Fatal("queue timeout revoked accepted members", stats)
				}
				if after := m.Snapshot(); after.Used != before.Used || after.Owners != before.Owners || after.NormalClaims != before.NormalClaims || after.CompletionReserved != before.CompletionReserved {
					t.Fatal("wait timeout refunded accepted fees", before, after)
				}
				close(release)
				if err := <-rootDone; err != nil {
					t.Fatal("accepted terminal failed after wait timeout", err)
				}
				// The timeout is a finite wait outcome, not permanent member withdrawal.
				if err := ws[1].WithWorkspace(context.Background(), func(ctx context.Context) error {
					p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 200})
					if err != nil {
						return err
					}
					return p.Release()
				}); err != nil {
					t.Fatal("accepted member could not retry completion", err)
				}
				closeShared(t, m, s, ws)
			})
		})
	}
}

func TestSharedWorkspaceWaitDeadlineDoesNotExpireAnEnteredCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, s, ws := sharedFixture(t, 2)
		held, release := make(chan struct{}), make(chan struct{})
		rootDone := make(chan error, 1)
		go func() {
			rootDone <- ws[0].WithWorkspace(context.Background(), func(context.Context) error { close(held); <-release; return nil })
		}()
		<-held
		type sourceKey struct{}
		caller := context.WithValue(context.Background(), sourceKey{}, "original source")
		entered, finish := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- ws[1].WithWorkspace(caller, func(ctx context.Context) error {
				close(entered)
				<-finish
				if ctx.Err() != nil || ctx.Done() != nil || ctx.Value(sourceKey{}) != "original source" {
					t.Error("queue timer replaced entered source context")
				}
				p, err := m.TryAcquireContext(ctx, IO, Cost{RAM: 200})
				if err != nil {
					return err
				}
				return p.Release()
			})
		}()
		synctest.Wait()
		if s.Snapshot().Waiting != 1 {
			t.Fatal("waiter not accepted")
		}
		time.Sleep(time.Second)
		close(release)
		if err := <-rootDone; err != nil {
			t.Fatal(err)
		}
		<-entered
		time.Sleep(MaxWorkQueueWait + time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatal("queue timer retired active callback", err)
		default:
		}
		if err := ws[1].Close(); !errors.Is(err, ErrExhausted) {
			t.Fatal("entered callback lost its member", err)
		}
		close(finish)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		closeShared(t, m, s, ws)
	})
}
