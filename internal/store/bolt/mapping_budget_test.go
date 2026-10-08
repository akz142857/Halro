package bolt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func metadataMappingTestBudget(t *testing.T, maxBytes int64) (*resourcebudget.Manager, int64) {
	t.Helper()
	ram, err := metadatalimits.MappingRAMBytes(maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	limits := resourcebudget.Limits{
		Total:           resourcebudget.Cost{RAM: 4*ram + 16*resourcebudget.ClaimRAMBytes},
		RecoveryReserve: resourcebudget.Cost{RAM: 2*ram + 4*resourcebudget.ClaimRAMBytes},
		MaxClaims:       16, RecoveryClaims: 4, MaxOwners: 4, MaxPressureAge: time.Minute,
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{RAM: 2 * ram}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	return budget, ram
}

func TestMetadataMappingAdmissionPrecedesFileIO(t *testing.T) {
	ctx := context.Background()
	budget, ram := metadataMappingTestBudget(t, 1<<20)
	occupied, err := budget.TryAcquireRecovery(resourcebudget.Cost{RAM: 2 * ram})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Release()
	path := filepath.Join(t.TempDir(), "absent", "metadata.db")
	for _, manager := range []*resourcebudget.Manager{nil, budget} {
		if store, err := OpenWithMappingBudget(ctx, path, 1<<20, manager); store != nil || err == nil {
			t.Fatal("unowned mapping entered IO", store, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if store, err := OpenWithMappingBudget(canceled, path, 1<<20, budget); store != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled mapping entered IO", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal created a directory", err)
	}
}

func TestMetadataMappingFeeJoinsActualViewAndClose(t *testing.T) {
	budget, ram := metadataMappingTestBudget(t, 1<<20)
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := OpenWithMappingBudget(context.Background(), path, 1<<20, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	go func() { readDone <- store.view(func(*Tx) error { close(entered); <-release; return nil }) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatal("Close escaped a live native view", err)
	case <-time.After(30 * time.Millisecond):
	}
	if s := budget.Snapshot(); s.Classes[resourcebudget.Recovery].RAM != ram || s.RecoveryClaims != 1 {
		close(release)
		t.Fatal("Close cancellation refunded a live mapping", s)
	}
	close(release)
	if err := errors.Join(<-readDone, <-closed); err != nil {
		t.Fatal(err)
	}
	if s := budget.Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("actual DB close retained mapping fee", s)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("mapping retirement deleted persistent metadata", err)
	}
}

func TestMetadataMappingReservationRequiresSameLiveOwner(t *testing.T) {
	budget, ram := metadataMappingTestBudget(t, 1<<20)
	other, _ := metadataMappingTestBudget(t, 1<<20)
	credit, err := budget.TryAcquireRecovery(resourcebudget.Cost{RAM: ram})
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	path := filepath.Join(t.TempDir(), "metadata.db")
	if store, err := OpenWithMappingReservation(context.Background(), path, 1<<20, other, credit); store != nil || !errors.Is(err, resourcebudget.ErrInvalid) {
		t.Fatal("mapping borrowed a foreign manager", err)
	}
	store, err := OpenWithMappingReservation(context.Background(), path, 1<<20, budget, credit)
	if err != nil {
		t.Fatal(err)
	}
	if s := budget.Snapshot(); s.RecoveryClaims != 1 || s.Owners != 2 || s.Classes[resourcebudget.Recovery].RAM != ram {
		t.Fatal("borrow forked or lost the whole mapping fee", s)
	}
	if err := credit.ReleasePart(resourcebudget.Cost{RAM: ram}); !errors.Is(err, resourcebudget.ErrInvalid) {
		t.Fatal("live native borrower lost its fee", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if s := budget.Snapshot(); s.Owners != 1 || s.Classes[resourcebudget.Recovery].RAM != ram {
		t.Fatal("bootstrap Close refunded Runtime's reservation", s)
	}
}

func TestMetadataCompactionRequiresSeparateDestinationMapping(t *testing.T) {
	budget, ram := metadataMappingTestBudget(t, 1<<20)
	dir := t.TempDir()
	store, err := OpenWithMappingBudget(context.Background(), filepath.Join(dir, "source.db"), 1<<20, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	occupied, err := budget.TryAcquireRecovery(resourcebudget.Cost{RAM: ram})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "absent", "compact.db")
	if err := store.CompactSnapshot(destination); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("compaction used source fee for a second mmap", err)
	}
	if _, err := os.Stat(filepath.Dir(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("compaction allocated before admission", err)
	}
	if err := occupied.Release(); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactSnapshot(destination); err != nil {
		t.Fatal(err)
	}
	if s := budget.Snapshot(); s.RecoveryClaims != 1 || s.Classes[resourcebudget.Recovery].RAM != ram {
		t.Fatal("closed compaction destination retained mapping fee", s)
	}
	if info, err := os.Stat(destination); err != nil || info.Size() > 1<<20 {
		t.Fatal("compaction escaped its data-file limit", info, err)
	}
}

func TestMetadataFailedEngineOpenKeepsUnprovenMappingFee(t *testing.T) {
	budget, ram := metadataMappingTestBudget(t, 1<<20)
	path := filepath.Join(t.TempDir(), "invalid.db")
	original := make([]byte, 4096)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenReadOnlyWithMappingBudget(context.Background(), path, 1<<20, budget)
	if store != nil || !errors.Is(err, ErrMetadataFileRetirement) {
		t.Fatal("opaque engine cleanup was treated as a known close", err)
	}
	if snapshot := budget.Snapshot(); snapshot.RecoveryClaims != 1 || snapshot.Classes[resourcebudget.Recovery].RAM != ram {
		t.Fatal("failed engine Open refunded unproven mmap retirement", snapshot)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("failed Open changed source", err)
	}
}
