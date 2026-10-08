package app

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

// finishHAPhysicalBusiness withdraws the original services and joins actual
// transport, then publishes the independently confirmable complete seven-root
// cut. It is terminal maintenance, never a grant to resume producers or HTTP.
func (f *haSelectedRuntimeFoundation) finishHAPhysicalBusiness(ctx context.Context) error {
	if f == nil || f.reopened == nil || f.business == nil || f.cfg.ResourceBudget == nil || !f.cfg.ResourceBudget.PhysicalTerminalReserve || f.ctx.Err() != nil {
		return authenticatedstate.ErrSealIneligible
	}
	// HTTP/accepted accounting has already joined. Finish the actual live
	// collector before its source is rebound to the new maintenance lease.
	if f.business.collector != nil {
		if err := f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, f.business.collector.CatchUp); err != nil {
			return fmt.Errorf("HA terminal original collector catch-up: %w", err)
		}
	}
	if err := f.business.close(ctx); err != nil {
		return err
	}
	if f.replication.role == replication.RolePrimary {
		c := f.reopened.Owners.Components
		if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			head, _, err := c.Ledger.ReadPosition(ctx)
			if err != nil {
				return err
			}
			target, err := f.replication.coordinator.CaptureRecoveryTarget(ctx, replication.StoreCursor{Generation: head.Generation, Sequence: head.Sequence})
			if err != nil {
				return err
			}
			return f.replication.coordinator.WaitRecoveryAppliedTarget(ctx, target)
		}); err != nil {
			return fmt.Errorf("HA terminal complete Ordering peer application before transport retirement: %w", err)
		}
	}
	f.transportOnce.Do(func() {
		r := f.replication
		r.stopPrimaryCompletion()
		if r.coordinator != nil {
			r.coordinator.Stop()
		}
		if r.manager != nil {
			f.transportErr = r.manager.Close()
		}
	})
	if f.transportErr != nil {
		return f.transportErr
	}
	f.callbacks.Wait()
	c := f.reopened.Owners.Components
	lease := c.Lease
	if _, err := lease.ValidateOwner(c.NativeLedger); err != nil {
		var acquireErr error
		if f.replication.role == replication.RoleReplica {
			lease, acquireErr = c.NativeLedger.BeginReplicaReadMaintenance(ctx)
		} else {
			lease, acquireErr = c.NativeLedger.BeginMaintenance(ctx)
		}
		if acquireErr != nil {
			return acquireErr
		}
		f.owners.Components.Lease, f.reopened.Owners.Components.Lease = lease, lease
	}
	return c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		if err := c.Usage.BindHASelectedTypedMaintenance(ctx, c.NativeLedger, lease, c.Ledger, c.Budget); err != nil {
			return fmt.Errorf("HA terminal Usage maintenance binding: %w", err)
		}
		if _, err := c.Directory.AdvanceHASelectedMaintenanceFromNative(ctx, c.NativeLedger, lease, c.Ledger); err != nil {
			return fmt.Errorf("HA terminal Directory catch-up: %w", err)
		}
		base := f.reopened.Seal
		if base.Revision == math.MaxUint64 {
			return authenticatedstate.ErrLimit
		}
		base.Revision++
		if observe, ok := ctx.Value(haPhysicalFinishObserverKey{}).(func(string) error); ok {
			if err := observe("common-ready"); err != nil {
				return err
			}
		}
		publication, err := statecut.PublishHASelectedMaintenanceCapturedStartupCaches(ctx, f.reopened.Owners, base, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
			if err := statecut.RequireNoHAOrderingPeerPromises(ctx, f.reopened.Owners, f.physicalPublication); err != nil {
				return statecut.StartupCacheCandidates{}, err
			}
			return f.prepareHAPhysicalCheckpointCaches(ctx, f.reopened.Seal)
		})
		if err != nil {
			return fmt.Errorf("HA terminal whole common publication: %w", err)
		}
		f.reopened.Seal = publication.Seal
		if err := publication.CleanupError(); err != nil {
			return err
		}
		if observe, ok := ctx.Value(haPhysicalFinishObserverKey{}).(func(string) error); ok {
			if err := observe("common-published"); err != nil {
				return err
			}
		}
		if f.replication.role == replication.RoleReplica {
			if err := c.Ledger.CloseHAReplicaPhysicalCatchup(ctx, c.NativeLedger, lease); err != nil {
				return fmt.Errorf("HA terminal Replica original receive window: %w", err)
			}
		}
		return f.retireHAPhysicalDerivedVectorAfterCommon(ctx)
	})
}

type haPhysicalFinishObserverKey struct{}

// The final native close must run outside the source's own authority callback.
func (f *haSelectedRuntimeFoundation) closeHAPhysicalNative(ctx context.Context) error {
	c := f.reopened.Owners.Components
	if err := f.withSelectedCut(ctx, func(ctx context.Context, _ statecut.HASelectedCut) error {
		return statecut.RequireNoHAOrderingPeerPromises(ctx, f.reopened.Owners, f.physicalPublication)
	}); err != nil {
		return err
	}
	entered := false
	err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		entered = true
		return c.NativeLedger.CloseHAQualifiedPhysicalContext(ctx, c.Lease)
	})
	if !entered {
		// Preserve the caller's physical qualification context. Native shutdown
		// still joins its actual writers and descriptors before reporting exit.
		err = errors.Join(err, c.NativeLedger.CloseContext(ctx))
	}
	return err
}
