package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestAdminAuditBatchOwnerBoundsOutstandingAndJoinsActualNativeIO(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	before := r.audit.Summary()
	master, err := unlockMasterKey(context.Background(), r.config)
	if err != nil {
		t.Fatal(err)
	}
	key, err := loadAuditHMACKey(r.store, r.vault, master)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if err := r.audit.Close(); err != nil {
		t.Fatal(err)
	}
	var blocked *projectedWriterBlockedDurability
	log, err := audit.OpenWithOptions(r.config.AuditPath(), key, audit.Options{WrapDurability: func(f *os.File) audit.DurabilityWriter {
		blocked = &projectedWriterBlockedDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
		blocked.block.Store(true)
		return blocked
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.audit = log
	done := make(chan error, adminAuditMaxEntries)
	appendEvent := func() {
		done <- r.appendAdminAudit("system", "test", "bounded.audit", "gateway", "local", "success", "")
	}
	go appendEvent()
	<-blocked.entered
	for i := 1; i < adminAuditMaxEntries; i++ {
		go appendEvent()
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.auditBatches.mu.Lock()
		count := r.auditBatches.count
		r.auditBatches.mu.Unlock()
		if count == adminAuditMaxEntries {
			break
		}
		if time.Now().After(deadline) {
			close(blocked.release)
			t.Fatal("waiting admissions did not fill fixed slots", count)
		}
		time.Sleep(time.Millisecond)
	}
	if err := r.appendAdminAudit("system", "overflow", "bounded.audit", "gateway", "local", "success", ""); !errors.Is(err, errAdminAuditBatchUnavailable) {
		close(blocked.release)
		t.Fatal("unbounded queue accepted", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- r.closeAdminAuditBatches() }()
	select {
	case err := <-closeDone:
		close(blocked.release)
		t.Fatal("Close passed blocked native writer", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocked.release)
	for i := 0; i < adminAuditMaxEntries; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := r.appendAdminAudit("system", "retired", "bounded.audit", "gateway", "local", "success", ""); !errors.Is(err, errAdminAuditBatchUnavailable) {
		t.Fatal("retired owner admitted", err)
	}
	if head := r.audit.Summary(); head.Records != before.Records+adminAuditMaxEntries {
		t.Fatal("accepted audit lost or duplicated", head)
	}
	r.auditBatches.mu.Lock()
	defer r.auditBatches.mu.Unlock()
	if r.auditBatches.pending != nil || r.auditBatches.count != 0 || r.auditBatches.bytes != 0 || r.auditBatches.running {
		t.Fatal("joined owner retained event references")
	}
}

func TestAdminAuditBatchOwnerSharedEnvelopeRefusesBeforeNativeWrite(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	b := runtimeRAMBudgetFixture()
	manager, err := resourcebudget.New(b.Limits(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	r.resourcePressure = &resourcePressureRuntime{budget: manager}
	held, err := manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: adminAuditBatchRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	head := r.audit.Summary()
	if err := r.appendAdminAudit("system", "test", "bounded.audit", "gateway", "local", "success", ""); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("admitted without shared envelope", err)
	}
	if r.audit.Summary() != head || r.auditBatches.count != 0 {
		t.Fatal("refusal queued or wrote event")
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := r.appendAdminAudit("system", "test", "bounded.audit", "gateway", "local", "success", ""); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); got.Used != (resourcebudget.Cost{}) || got.NormalClaims != 0 {
		t.Fatal("finished audit did not refund", got)
	}
}

func TestAdminAuditInvalidEncodingRefundsEnvelopeBeforeQueue(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	b := runtimeRAMBudgetFixture()
	manager, err := resourcebudget.New(b.Limits(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	r.resourcePressure = &resourcePressureRuntime{budget: manager}
	before := r.audit.Summary()
	// Finite input passes preflight but lacks the required Action. Validation
	// happens inside the admitted job and its fee must really retire on failure.
	event := audit.Event{EventID: "invalid", ActorType: "system", Outcome: "success", OccurredAt: time.Now().UTC()}
	if err := r.enqueueAdminAudit(event); err == nil {
		t.Fatal("invalid event accepted")
	}
	s := manager.Snapshot()
	if s.Acquired[resourcebudget.IO] != 1 || s.Used != (resourcebudget.Cost{}) || r.audit.Summary() != before || r.auditBatches.pending != nil || r.auditBatches.running {
		t.Fatal("failed encoding escaped ownership or leaked queue", s)
	}
}

func TestAdminAuditQueueOwnsMetadataBeforeWaitingForNativeIO(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	master, err := unlockMasterKey(context.Background(), r.config)
	if err != nil {
		t.Fatal(err)
	}
	key, err := loadAuditHMACKey(r.store, r.vault, master)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if err := r.audit.Close(); err != nil {
		t.Fatal(err)
	}
	var blocked *projectedWriterBlockedDurability
	r.audit, err = audit.OpenWithOptions(r.config.AuditPath(), key, audit.Options{WrapDurability: func(f *os.File) audit.DurabilityWriter {
		blocked = &projectedWriterBlockedDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
		blocked.block.Store(true)
		return blocked
	}})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { close(blocked.release) }) }
	defer release()
	done := make(chan error, 2)
	go func() {
		done <- r.appendAdminAudit("system", "first", "bounded.audit", "gateway", "local", "success", "")
	}()
	<-blocked.entered
	metadata := map[string]any{"before": "immutable", "number": uint64(18446744073709551615)}
	event := audit.Event{EventID: "owned-metadata", ActorType: "system", Action: "bounded.audit", Outcome: "success", OccurredAt: time.Now().UTC(), Metadata: metadata}
	go func() { done <- r.enqueueAdminAudit(event) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.auditBatches.mu.Lock()
		count := r.auditBatches.count
		r.auditBatches.mu.Unlock()
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiting event was never admitted", count)
		}
		time.Sleep(time.Millisecond)
	}
	// Mutation happens after queue admission but before its actual append. The
	// queued graph must be owned and preserve exact numbers, including under race.
	metadata["before"] = "changed"
	metadata["number"] = uint64(1)
	release()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	found := false
	if _, err := r.audit.Replay(func(record audit.Record) error {
		if record.Event.EventID == event.EventID {
			found = true
			if record.Event.Metadata["before"] != "immutable" || fmt.Sprint(record.Event.Metadata["number"]) != "18446744073709551615" {
				t.Fatal("queue kept borrowed metadata", record.Event.Metadata)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("queued event was not persisted")
	}
}
