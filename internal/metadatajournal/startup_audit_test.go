package metadatajournal

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/startupaudit"
)

func TestMetadataFiniteAuditActualReaderCloseDebtRetainsFDKeyAndWholeJob(t *testing.T) {
	l := retirementJournal(t)
	for range 2 {
		if _, err := l.Append([]Op{put("projects", "old", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	budget := metadataFrozenBudget(t)
	l.startupSaved, l.startupAuditPending, l.startupBudget = l.Head(), true, budget
	writer := l.file
	var reader *os.File
	a, err := l.startSelectedStartupAudit(context.Background(), budget, StartupAuditOptions{Deadline: 5 * time.Second, ProgressTimeout: time.Second}, func(_ context.Context, record Record, file *os.File) {
		if record.Kind == KindEpoch {
			reader = file
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Wait(ctx); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("actual failed reader Close did not retain debt", err)
	}
	if l.startupReaderDebt == nil || l.startupReaderDebt.file != reader || len(l.startupReaderDebt.key) != KeySize || budget.Snapshot().Used.RAM < FrozenReadRAMBytes || l.SelectedStartupReady() {
		t.Fatal("lost actual reader/key/fee", budget.Snapshot(), l.HealthError())
	}
	for range 2 {
		if !errors.Is(l.Close(), ownedfile.ErrRetirement) || l.file != writer || len(l.key) != KeySize || !errors.Is(l.Discard(), ownedfile.ErrRetirement) {
			t.Fatal("repeat cleanup lost unresolved native/reader owner")
		}
	}
	if _, err := os.Stat(l.path); err != nil {
		t.Fatal("source removed before actual reader retirement", err)
	}
}

func TestMetadataFiniteAuditExpiryRefusesActualPreparedBarrierBeforeNativeWrite(t *testing.T) {
	l := retirementJournal(t)
	if _, err := l.Append([]Op{put("projects", "old", "v")}); err != nil {
		t.Fatal(err)
	}
	budget := metadataFrozenBudget(t)
	l.startupSaved, l.startupAuditPending, l.startupBudget = l.Head(), true, budget
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = l.Close()
		_ = l.Discard()
	}()
	a, err := l.StartSelectedStartupAuditObserved(context.Background(), budget, StartupAuditOptions{Deadline: 5 * time.Second, ProgressTimeout: 100 * time.Millisecond}, func(ctx context.Context, records uint64, bytes int64) error {
		if records == 1 {
			<-release
		}
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	before := l.Head()
	_, err = l.AppendPreparedContext(context.Background(), []Op{put("projects", "refused", "v")}, func(_ Record, durable func() error) error {
		deadline := time.Now().Add(2 * time.Second)
		for a.Snapshot().Ready && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return durable()
	})
	if !errors.Is(err, ErrStartupAuditPending) || !errors.Is(err, startupaudit.ErrProgress) || l.Head() != before {
		t.Fatal("expired actual barrier wrote native", err, l.Head(), before)
	}
	close(release)
	if err := a.Wait(ctx); err == nil {
		t.Fatal("expired reader became complete")
	}
}
