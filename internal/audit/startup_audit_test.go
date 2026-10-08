package audit

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/startupaudit"
)

func reopenSelectedForAudit(t *testing.T, f *startupCacheFixture) {
	t.Helper()
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	n, _, err := OpenWithSelectedSeal(context.Background(), f.path, f.key, f.options, f.selected)
	if err != nil {
		t.Fatal(err)
	}
	f.native = n
}

func TestSelectedStartupAuditActualProgressGrantsAppendBeforeCompletion(t *testing.T) {
	f := newStartupCacheFixture(t, 6)
	reopenSelectedForAudit(t, f)
	entered, leave := make(chan struct{}), make(chan struct{})
	a, err := f.native.startSelectedStartupAudit(context.Background(), f.options.CacheBudget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second}, func(seq uint64, _ *os.File) {
		if seq == 1 {
			close(entered)
			<-leave
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := a.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := a.Snapshot(); !p.Ready || p.Complete || p.Records != 1 {
		t.Fatal(p)
	}
	if !f.native.SelectedStartupReady() || f.native.SelectedStartupStructuralReady() {
		t.Fatal("native grants did not distinguish append and structure")
	}
	if _, err := f.native.Append(context.Background(), validEvent(7, "key.create")); err != nil {
		t.Fatal("same inode append blocked", err)
	}
	if _, err := f.native.SealReference(context.Background(), f.options.CacheBudget, f.native.Summary()); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal("publication granted before audit retirement", err)
	}
	close(leave)
	if err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := a.Snapshot(); !p.Complete || p.Records != 6 || !f.native.SelectedStartupStructuralReady() {
		t.Fatal(p)
	}
}

func TestSelectedStartupAuditProgressExpiryWithdrawsBeforeActualReaderExit(t *testing.T) {
	f := newStartupCacheFixture(t, 6)
	reopenSelectedForAudit(t, f)
	entered, leave := make(chan struct{}), make(chan struct{})
	a, err := f.native.startSelectedStartupAudit(context.Background(), f.options.CacheBudget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: 40 * time.Millisecond}, func(seq uint64, _ *os.File) {
		if seq == 1 {
			close(entered)
			<-leave
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	before := f.options.CacheBudget.Snapshot().Used
	deadline := time.Now().Add(time.Second)
	for a.Snapshot().Ready && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p := a.Snapshot(); p.Ready || !errors.Is(p.Err, startupaudit.ErrProgress) {
		t.Fatal(p)
	}
	if f.native.SelectedStartupReady() {
		t.Fatal("native ready outlived progress grant")
	}
	if _, err := f.native.Append(context.Background(), validEvent(7, "key.create")); err == nil {
		t.Fatal("expired grant still admitted append")
	}
	if got := f.options.CacheBudget.Snapshot().Used; got != before {
		t.Fatal("blocked actual IO fee refunded", got, before)
	}
	closed := make(chan error, 1)
	go func() { closed <- f.native.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close did not join actual reader", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(leave)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(context.Background()); err == nil {
		t.Fatal("expired historical audit accepted")
	}
}

func TestSelectedStartupAuditHardDeadlineAndCancellation(t *testing.T) {
	for _, cancelJob := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancellation"}[cancelJob], func(t *testing.T) {
			f := newStartupCacheFixture(t, 3)
			reopenSelectedForAudit(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, leave := make(chan struct{}), make(chan struct{})
			a, err := f.native.startSelectedStartupAudit(ctx, f.options.CacheBudget, StartupAuditOptions{Deadline: 40 * time.Millisecond, ProgressTimeout: 40 * time.Millisecond}, func(seq uint64, _ *os.File) {
				if seq == 1 {
					close(entered)
					<-leave
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			if cancelJob {
				cancel()
			}
			deadline := time.Now().Add(time.Second)
			for a.Snapshot().Ready && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if f.native.SelectedStartupReady() {
				t.Fatal("expired/canceled native grant")
			}
			close(leave)
			if err := a.Wait(context.Background()); err == nil {
				t.Fatal("invalid audit succeeded")
			}
		})
	}
}

func TestSelectedStartupAuditActualCloseDebtPinsWholeOwner(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	reopenSelectedForAudit(t, f)
	a, err := f.native.startSelectedStartupAudit(context.Background(), f.options.CacheBudget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second}, func(seq uint64, file *os.File) {
		if seq == 3 {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(context.Background()); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if f.native.SelectedStartupReady() || f.native.startupReaderDebt == nil || f.native.startupReaderDebt.file == nil || len(f.native.startupReaderDebt.key) != 32 {
		t.Fatal("uncertain actual audit reader owner lost")
	}
	f.expectDebt = true
	if err := f.native.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
}

func TestSelectedStartupAuditRefusesUnlimitedDeadline(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	reopenSelectedForAudit(t, f)
	before := f.options.CacheBudget.Snapshot().Used
	if a, err := f.native.StartSelectedStartupAudit(context.Background(), f.options.CacheBudget, StartupAuditOptions{Deadline: time.Hour, ProgressTimeout: time.Second}); a != nil || !errors.Is(err, startupaudit.ErrOptions) {
		t.Fatal(a, err)
	}
	if got := f.options.CacheBudget.Snapshot().Used; got != before {
		t.Fatal(got, before)
	}
}
