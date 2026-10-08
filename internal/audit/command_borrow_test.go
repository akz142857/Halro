package audit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSelectedCommandBorrowJoinsReplayAndPreservesNativeWriter(t *testing.T) {
	f := newStartupCacheFixture(t, 6)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.native.BorrowSelectedCommand(ctx, f.selected.Guard, f.options.CacheBudget, f.key); err == nil {
		t.Fatal("ordinary native granted selected command ownership")
	}
	reopenSelectedForAudit(t, f)
	if _, err := f.native.BorrowSelectedCommand(ctx, f.selected.Guard, f.options.CacheBudget, f.key); err == nil {
		t.Fatal("unaudited native granted command ownership")
	}
	a, err := f.native.StartSelectedStartupAudit(ctx, f.options.CacheBudget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	before := f.options.CacheBudget.Snapshot()
	loan, err := f.native.BorrowSelectedCommand(ctx, f.selected.Guard, f.options.CacheBudget, f.key)
	if err != nil {
		t.Fatal(err)
	}
	entered, leave := make(chan struct{}), make(chan struct{})
	replayed, closed := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := loan.ReplayContext(ctx, func(r Record) error {
			if r.Sequence == 1 {
				close(entered)
				<-leave
			}
			return nil
		})
		replayed <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { closed <- loan.Close() }()
	select {
	case err := <-closed:
		t.Fatal("borrower closed before its accepted reader exited", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(leave)
	if err := <-replayed; !errors.Is(err, context.Canceled) {
		t.Fatal("Close did not cancel replay", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	after := f.options.CacheBudget.Snapshot()
	if after.Used != before.Used || after.Owners != before.Owners || after.NormalClaims != before.NormalClaims {
		t.Fatal("joined borrower retained temporary fees", before, after)
	}
	if _, err := loan.Replay(nil); err == nil {
		t.Fatal("retired borrower accepted a new read")
	}
	if _, err := f.native.Append(ctx, validEvent(7, "key.create")); err != nil {
		t.Fatal("borrower Close retired its original native writer", err)
	}
	if f.native.Summary().Records != 7 || loan.Summary().Records != 6 {
		t.Fatal("borrower retained a mutable native alias")
	}
}
