package metadatajournal

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestMetadataSelectedHistoricalAuditCloseCancelsAndJoinsActualReader(t *testing.T) {
	l := retirementJournal(t)
	if _, err := l.Append([]Op{put("projects", "historical", "v")}); err != nil {
		t.Fatal(err)
	}
	budget := metadataFrozenBudget(t)
	l.startupSaved, l.startupAuditPending, l.startupBudget = l.Head(), true, budget
	entered, revoked, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	result := make(chan error, 1)
	go func() {
		result <- l.auditSelectedStartup(context.Background(), budget, func(ctx context.Context, record Record) {
			if record.Kind != KindOperations {
				return
			}
			close(entered)
			<-ctx.Done()
			close(revoked)
			<-release
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual old frame not reached")
	}
	fee := budget.Snapshot().Used
	if fee.RAM < FrozenReadRAMBytes || l.SelectedStartupReady() {
		t.Fatal("job missing fee or falsely ready", fee)
	}
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case <-revoked:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not revoke actual history reader")
	}
	select {
	case err := <-closed:
		t.Fatal("Close skipped actual job join", err)
	case <-time.After(25 * time.Millisecond):
	}
	if budget.Snapshot().Used != fee || l.file == nil {
		t.Fatal("work/FD retired before callback exit", budget.Snapshot())
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("revoked audit reported completion", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if l.SelectedStartupReady() || budget.Snapshot().Used != (resourcebudget.Cost{}) || l.file != nil {
		t.Fatal("close/audit final ownership", budget.Snapshot())
	}
	if err := l.Discard(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataSelectedHistoricalAuditUnknownNativeCloseKeepsActualOwner(t *testing.T) {
	l := retirementJournal(t)
	if _, err := l.Append([]Op{put("projects", "historical", "v")}); err != nil {
		t.Fatal(err)
	}
	budget := metadataFrozenBudget(t)
	l.startupSaved, l.startupAuditPending, l.startupBudget = l.Head(), true, budget
	actual := l.file
	err := l.auditSelectedStartup(context.Background(), budget, func(_ context.Context, record Record) {
		if record.Kind == KindOperations {
			if err := actual.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err == nil || l.SelectedStartupReady() {
		t.Fatal("actual closed descriptor passed audit", err)
	}
	if !errors.Is(l.Close(), ownedfile.ErrRetirement) || l.file != actual || len(l.key) == 0 {
		t.Fatal("Close dropped actual unknown owner")
	}
	// The old-reader scan has exited, so its read job fee can be released;
	// the actual native FD/key ownership remains stable and never retries.
	if budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("known read job failed to retire", budget.Snapshot())
	}
	if !errors.Is(l.Close(), ownedfile.ErrRetirement) || !errors.Is(l.Discard(), ownedfile.ErrRetirement) {
		t.Fatal("repeat close erased debt")
	}
	if _, err := os.Stat(l.path); err != nil {
		t.Fatal("uncertain source removed", err)
	}
}
