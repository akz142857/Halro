package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type emptyAdmissionSource struct{}

func (emptyAdmissionSource) Read(replication.Store, uint64, uint64, uint64) ([]byte, error) {
	return nil, errors.New("unexpected durable-source read")
}
func (emptyAdmissionSource) Cursor(replication.Store) (replication.StoreCursor, error) {
	return replication.StoreCursor{}, nil
}
func (emptyAdmissionSource) HeadAt(replication.Store, replication.StoreCursor) ([32]byte, error) {
	return [32]byte{}, nil
}
func (emptyAdmissionSource) Truncate(replication.Store, replication.StoreCursor) error { return nil }

type emptyAdmissionOutbound struct{}

func (emptyAdmissionOutbound) QueueFrame(uint64, []byte) error        { return nil }
func (emptyAdmissionOutbound) QueueCommitNotice(uint64, []byte) error { return nil }
func (emptyAdmissionOutbound) Acknowledge(string, uint64, uint64)     {}

func TestProviderObjectBudgetRefusalPrecedesSourcePublication(t *testing.T) {
	manager, err := resourcebudget.New(resourcebudget.Limits{
		Total:           resourcebudget.Cost{RAM: 16 << 20, Unconfirmed: 1},
		RecoveryReserve: resourcebudget.Cost{RAM: 8 << 20, Unconfirmed: 1},
		Classes: [resourcebudget.ClassCount]resourcebudget.Cost{
			resourcebudget.Configuration: {},
			resourcebudget.IO:            {Unconfirmed: 1},
			resourcebudget.Recovery:      {RAM: 8 << 20, Unconfirmed: 1},
			resourcebudget.Control:       {RAM: 8 * resourcebudget.ClaimRAMBytes},
		},
		MaxClaims: 8, RecoveryClaims: 4, MaxOwners: 2, MaxPressureAge: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(t.TempDir(), "ordering.journal")
	journal, err := replication.OpenOrderingJournal(journalPath, []byte("0123456789abcdef0123456789abcdef"), replication.OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	limits := replication.DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = manager
	limits.MaxUnconfirmedEntries = 4
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 1
	coordinator, err := replication.NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1"}, journal, emptyAdmissionOutbound{}, func(replication.PrimaryProgress) error { return nil }, emptyAdmissionSource{}, limits)
	if err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		coordinator.Stop()
		_ = journal.Close()
		_ = coordinator.ReleaseUnconfirmedForShutdown()
	})

	directory := filepath.Join(t.TempDir(), "provider-objects")
	runtime := &replicationRuntime{role: replication.RolePrimary, coordinator: coordinator, objectSourceDir: directory}
	err = runtime.replicateProviderObject(context.Background(), "object.content", []byte("sealed provider object"))
	if !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatalf("provider-object append did not propagate budget refusal: %v", err)
	}
	if _, statErr := os.Stat(directory); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("budget-refused provider object created its source directory: %v", statErr)
	}
	if stats := manager.Snapshot(); stats.Used.Unconfirmed != 0 || stats.Rejected[resourcebudget.IO] != 1 {
		t.Fatalf("refused provider object leaked a reservation or did not register refusal: %+v", stats)
	}
}
