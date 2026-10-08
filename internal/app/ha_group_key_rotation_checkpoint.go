package app

import (
	"context"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

// The command already joined its original TLS/DAO producers. It reuses each
// actual native/COW/cache owner and SAME Manager reservation instead of opening
// duplicate roots or treating an observation as publication authority.
func checkpointHAGroupRotationMember(ctx context.Context, f *haSelectedRuntimeFoundation) (checkpoint *HASelectedCheckpoint, resultErr error) {
	if f == nil || f.reopened == nil || f.replication == nil || f.cfg.ResourceBudget.PhysicalTerminalReserve {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := f.business.close(ctx); err != nil {
		return nil, err
	}
	f.business = nil
	c := f.reopened.Owners.Components
	var lease *ledger.MaintenanceLease
	var err error
	if f.replication.role == replication.RolePrimary {
		lease, err = c.NativeLedger.BeginMaintenance(ctx)
	} else {
		lease, err = c.NativeLedger.BeginReplicaReadMaintenance(ctx)
	}
	if err != nil {
		return nil, err
	}
	// Foundation.Close owns this finite installed lease even on a failed step.
	f.peerMaintenance = lease
	if err := c.Usage.BindHASelectedTypedMaintenance(ctx, c.NativeLedger, lease, c.Ledger, c.Budget); err != nil {
		return nil, err
	}
	o := f.reopened.Owners
	o.Components.Lease = lease
	base := f.reopened.Seal
	if base.Revision == math.MaxUint64 {
		return nil, authenticatedstate.ErrLimit
	}
	base.Revision++
	checkpoint = &HASelectedCheckpoint{owner: f}
	checkpoint.Publication, resultErr = statecut.PublishHASelectedMaintenanceCapturedStartupCaches(ctx, o, base, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
		if reused, reuseErr := checkpoint.reuseUnchangedFrozenCaches(ctx); reused || reuseErr != nil {
			return checkpoint.caches, reuseErr
		}
		return checkpoint.prepareChangedFrozenCaches(ctx)
	})
	return
}
