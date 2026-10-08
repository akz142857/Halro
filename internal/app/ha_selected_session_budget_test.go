package app

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestSelectedHASessionBudgetChargesOnlyActualSessionProfile(t *testing.T) {
	b := config.ResourceBudget{RecoveryRAMBytes: 896 << 20, RecoveryReserveRAMBytes: 896 << 20, RecoveryClaims: 12, ReplicationCandidateRAMBytes: replicationlimits.MinCandidateRAMBytes, ReplicationQueueRAMBytes: replicationlimits.MinQueueRAMBytes, ReplicationReadRAMBytes: replicationlimits.MinReadRAMBytes, ReplicationReaderRAMBytes: replicationlimits.MinNativeReaderRAMBytes}
	if err := validateSelectedHASessionBudget(b); err != nil {
		t.Fatal(err)
	}
	exact := 3*(b.ReplicationCandidateRAMBytes+b.ReplicationQueueRAMBytes+b.ReplicationReadRAMBytes) + b.ReplicationReaderRAMBytes + replication.MetadataBaselineReaderRAMBytes + 12*resourcebudget.ClaimRAMBytes
	b.RecoveryRAMBytes = exact
	b.RecoveryReserveRAMBytes = exact
	if err := validateSelectedHASessionBudget(b); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*config.ResourceBudget)
	}{
		{"recovery one byte short", func(v *config.ResourceBudget) { v.RecoveryRAMBytes-- }},
		{"reserve one byte short", func(v *config.ResourceBudget) { v.RecoveryReserveRAMBytes-- }},
		{"claim count", func(v *config.ResourceBudget) { v.RecoveryClaims-- }},
		{"native reader calibration", func(v *config.ResourceBudget) { v.ReplicationReaderRAMBytes-- }},
		{"candidate calibration", func(v *config.ResourceBudget) { v.ReplicationCandidateRAMBytes-- }},
		{"queue calibration", func(v *config.ResourceBudget) { v.ReplicationQueueRAMBytes-- }},
		{"incoming calibration", func(v *config.ResourceBudget) { v.ReplicationReadRAMBytes-- }},
		{"native overflow", func(v *config.ResourceBudget) { v.ReplicationReaderRAMBytes = math.MaxInt64 }},
		{"candidate overflow", func(v *config.ResourceBudget) { v.ReplicationCandidateRAMBytes = math.MaxInt64 }},
		{"queue overflow", func(v *config.ResourceBudget) { v.ReplicationQueueRAMBytes = math.MaxInt64 }},
		{"incoming overflow", func(v *config.ResourceBudget) { v.ReplicationReadRAMBytes = math.MaxInt64 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invalid := b
			tt.mutate(&invalid)
			if validateSelectedHASessionBudget(invalid) == nil {
				t.Fatal("invalid profile qualified")
			}
		})
	}
}

func TestSelectedHASessionBudgetConfigCannotCreateSelectedAuthority(t *testing.T) {
	cfg := config.Config{ResourceBudget: &config.ResourceBudget{MemoryStateRequired: true, RecoveryRAMBytes: 896 << 20, RecoveryReserveRAMBytes: 896 << 20}}
	f := &haSelectedRuntimeFoundation{ctx: context.Background(), cfg: cfg}
	if _, err := f.selectedReplicationSessionOptions(context.Background()); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal(err)
	}
}

func TestSelectedHASessionBudgetOriginalDAOAndFactoryRequired(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, owners, metadata := prepareHASelectedTransportFixture(t, ctx)
	f := owners[0]
	c := f.reopened.Owners.Components
	before := c.Budget.Snapshot().Used
	if err := f.reopened.ValidateHAMetadataStoreOwner(metadata[0], c.Budget); err != nil {
		t.Fatal(err)
	}
	if err := f.reopened.ValidateHAMetadataStoreOwner(&boltstore.Store{}, c.Budget); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal("foreign DAO", err)
	}
	if err := f.reopened.ValidateHAMetadataStoreOwner(metadata[1], c.Budget); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal("other incarnation DAO", err)
	}
	if err := f.reopened.ValidateHAMetadataStoreOwner(metadata[0], owners[1].owners.Components.Budget); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal("foreign Manager", err)
	}
	// Installed transport is already an externally reachable producer. It cannot
	// re-enter the private pre-install qualification or create a second queue.
	if _, err := f.selectedReplicationSessionOptions(ctx); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal("late session requalification", err)
	}
	if after := c.Budget.Snapshot().Used; before != after {
		t.Fatal("refusal changed actual fees", before, after)
	}
}
