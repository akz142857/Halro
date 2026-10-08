package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func deferredRAMManager(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 65536, Disk: 512}, MaxClaims: 4, MaxOwners: 2, MaxPressureAge: time.Minute}
	l.Classes[resourcebudget.Active] = resourcebudget.Cost{RAM: 40000}
	l.Classes[resourcebudget.Background] = resourcebudget.Cost{RAM: 40000}
	l.Classes[resourcebudget.Completion] = resourcebudget.Cost{Disk: 512}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 4 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDeferredResourceBudgetRefusalPreservesQueuedInputWithoutReadOrLedger(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "ram-refusal")
	record := f.record(t, submitted.ID)
	m := deferredRAMManager(t)
	f.service.resourceBudget, f.service.deferredRAMBytes = m, 40000
	// If the worker reads before acquiring, this missing file will terminally
	// fail accepted work. Keep it available to restore for the successful retry.
	path := filepath.Join(f.objectDir, record.InputObjectPath)
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	before := f.state.Watermark()
	if !f.service.runDeferredResponse(context.Background(), record) {
		t.Fatal("unknown pressure must retry queued work")
	}
	if got := f.record(t, record.ID); !reflect.DeepEqual(got, record) || f.state.Watermark() != before || f.adapter.calls != 0 {
		t.Fatal("refusal mutated accepted work", got)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	f.service.completionDiskBytes = 512
	heldCompletion, err := m.TryAcquireWork(resourcebudget.Active, resourcebudget.Cost{RAM: 1}, resourcebudget.Cost{Disk: 512})
	if err != nil {
		t.Fatal(err)
	}
	if !f.service.runDeferredResponse(context.Background(), record) {
		t.Fatal("completion-capacity refusal must preserve accepted work")
	}
	if got := f.record(t, record.ID); !reflect.DeepEqual(got, record) || f.state.Watermark() != before || f.adapter.calls != 0 {
		t.Fatal("completion refusal mutated accepted work", got)
	}
	_ = heldCompletion.Active.Release()
	_ = heldCompletion.Completion.Release()
	held, err := m.TryAcquire(resourcebudget.Active, resourcebudget.Cost{RAM: 40000})
	if err != nil {
		t.Fatal(err)
	}
	if !f.service.runDeferredResponse(context.Background(), record) {
		t.Fatal("shared total must defer, not fail")
	}
	if got := f.record(t, record.ID); !reflect.DeepEqual(got, record) || f.state.Watermark() != before || f.adapter.calls != 0 {
		t.Fatal("exhaustion mutated accepted work", got)
	}
	_ = held.Release()
	if err := os.Rename(path+".held", path); err != nil {
		t.Fatal(err)
	}
	if f.service.runDeferredResponse(context.Background(), record) {
		t.Fatal("successful work unexpectedly requeued")
	}
	if got := f.record(t, record.ID); got.Status != domain.DeferredCompleted || f.adapter.calls != 1 {
		t.Fatal(got.Status, f.adapter.calls)
	}
	if s := m.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.NormalClaims != 0 || s.Owners != 0 {
		t.Fatal("deferred envelope leaked", s)
	}
}

func TestDeferredResourceBudgetOwnedEnvelopeSurvivesPressureThroughTerminalWrite(t *testing.T) {
	f := newDeferredFixture(t)
	submitted := f.submit(t, "ram-active")
	record := f.record(t, submitted.ID)
	m := deferredRAMManager(t)
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	f.service.resourceBudget, f.service.deferredRAMBytes = m, 40000
	f.service.completionDiskBytes = 512
	started, release := make(chan struct{}, 1), make(chan struct{})
	f.adapter.started, f.adapter.release = started, release
	done := make(chan bool, 1)
	go func() { done <- f.service.runDeferredResponse(context.Background(), record) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("provider did not start")
	}
	if s := m.Snapshot(); s.Classes[resourcebudget.Background].RAM != 40000 || s.CompletionReserved.Disk != 512 || s.Owners != 2 {
		close(release)
		t.Fatal(s)
	}
	if err := m.SetPressure(resourcebudget.Pressured); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case requeue := <-done:
		if requeue {
			t.Fatal("pressure rolled admitted work back")
		}
	case <-time.After(time.Second):
		t.Fatal("provider did not complete")
	}
	if got := f.record(t, record.ID); got.Status != domain.DeferredCompleted || f.state.PendingReservations() != 0 {
		t.Fatal(got.Status, f.state.PendingReservations())
	}
	if s := m.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Owners != 0 {
		t.Fatal("terminal write leaked envelope", s)
	}
}

func TestResourceBudgetServiceOptionsRequireDeferredEnvelope(t *testing.T) {
	f := newFixture(t, 1000000)
	defer f.close()
	for _, options := range []ServiceOptions{{ResourceBudget: deferredRAMManager(t)}, {DeferredRAMBytes: 100}, {ResourceBudget: deferredRAMManager(t), DeferredRAMBytes: -1}} {
		if _, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, options); err == nil {
			t.Fatal("invalid RAM options accepted")
		}
	}
}
