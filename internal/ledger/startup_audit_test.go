package ledger

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/startupaudit"
)

func TestSelectedStartupAuditActualProgressAllowsAppendExcludesRoll(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "sealed"}[sealed], func(t *testing.T) {
			f := newSelectedStartupFixture(t, sealed)
			f.reopen(t)
			entered, leave := make(chan struct{}), make(chan struct{})
			a, err := f.native.startSelectedStartupAudit(context.Background(), f.o.DirectoryOptions.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second}, func(r Record, _ *os.File) {
				if r.Sequence == 1 {
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
			if !f.native.SelectedStartupReady() || f.native.SelectedStartupStructuralReady() {
				t.Fatal(a.Snapshot())
			}
			if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := f.native.Roll(); !errors.Is(err, ErrStartupAuditPending) {
				t.Fatal("Roll entered before actual reader exit", err)
			}
			close(leave)
			if err := a.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !f.native.SelectedStartupStructuralReady() {
				t.Fatal(a.Snapshot())
			}
			if _, err := f.native.Roll(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSelectedStartupAuditProgressDeadlineWithdrawsAndCloseJoins(t *testing.T) {
	f := newSelectedStartupFixture(t, true)
	f.reopen(t)
	entered, leave := make(chan struct{}), make(chan struct{})
	a, err := f.native.startSelectedStartupAudit(context.Background(), f.o.DirectoryOptions.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: 40 * time.Millisecond}, func(r Record, _ *os.File) {
		if r.Sequence == 1 {
			close(entered)
			<-leave
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	used := f.o.DirectoryOptions.Budget.Snapshot().Used
	deadline := time.Now().Add(time.Second)
	for a.Snapshot().Ready && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p := a.Snapshot(); p.Ready || !errors.Is(p.Err, startupaudit.ErrProgress) {
		t.Fatal(p)
	}
	if f.native.SelectedStartupReady() {
		t.Fatal("expired native readiness")
	}
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err == nil {
		t.Fatal("expired native append")
	}
	if got := f.o.DirectoryOptions.Budget.Snapshot().Used; got != used {
		t.Fatal("live IO fee released", got, used)
	}
	done := make(chan error, 1)
	go func() { done <- f.native.Close() }()
	select {
	case err := <-done:
		t.Fatal("actual historical reader not joined", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(context.Background()); err == nil {
		t.Fatal("expired audit completed successfully")
	}
}

func TestSelectedStartupAuditActualCloseDebtPinsReaderAndKey(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	f.reopen(t)
	last := f.native.startupSaved.Head.Sequence
	a, err := f.native.startSelectedStartupAudit(context.Background(), f.o.DirectoryOptions.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second}, func(r Record, file *os.File) {
		if r.Sequence == last {
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
		t.Fatal("actual audit debt owner lost")
	}
	f.expectDebt = true
	if err := f.native.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
}

func TestSelectedStartupAuditRejectsUnlimitedGrant(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	f.reopen(t)
	if a, err := f.native.StartSelectedStartupAudit(context.Background(), f.o.DirectoryOptions.Budget, StartupAuditOptions{Deadline: time.Hour, ProgressTimeout: time.Second}); a != nil || !errors.Is(err, startupaudit.ErrOptions) {
		t.Fatal(a, err)
	}
}

func TestSelectedStartupAuditAuthenticatesCompressedHistory(t *testing.T) {
	f := newSelectedStartupCompressedFixture(t, true, true)
	f.reopen(t)
	a, err := f.native.StartSelectedStartupAudit(context.Background(), f.o.DirectoryOptions.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.native.SelectedStartupReady() || !a.Snapshot().Complete {
		t.Fatal(a.Snapshot())
	}
}
