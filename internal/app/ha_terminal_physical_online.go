package app

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

// Whole HTTP/deferred and administrative audit jobs have already joined. The
// actual service pauses preserve their original managers, collector and DAO;
// transport stays alive so accepted peer work can finish applying and release
// its persistent promise before the reserve producer is paused.
func (f *haSelectedRuntimeFoundation) checkpointHAPhysicalOnline(ctx context.Context) (resultErr error) {
	if f == nil || f.reopened == nil || f.business == nil || f.metadataStore == nil || f.cfg.ResourceBudget == nil || !f.cfg.ResourceBudget.PhysicalTerminalReserve || f.ctx.Err() != nil || f.recovery.Load() {
		return authenticatedstate.ErrSealIneligible
	}
	if !f.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer f.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	if f.business.governance == nil || f.business.collector == nil {
		return authenticatedstate.ErrSealIneligible
	}
	resumeGovernance, err := f.business.governance.PauseOperations(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resumeGovernance()) }()
	if f.business.accounting != nil {
		resumeAccounting, err := f.business.accounting.PauseOperations(ctx)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, resumeAccounting()) }()
	}
	c := f.reopened.Owners.Components
	resumeMetadata, err := f.metadataStore.PauseFacade(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resumeMetadata()) }()
	if f.replication.role == replication.RolePrimary {
		var head ledger.Watermark
		// Read with the original paid source lanes, then release them before
		// entering the coordinator or waiting for accepted peer application.
		if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			var err error
			head, _, err = c.Ledger.ReadPosition(ctx)
			return err
		}); err != nil {
			return err
		}
		// Capture validates this comparison against the coordinator's real
		// durable prefix; a change in between fails rather than moving the cut.
		target, err := f.replication.coordinator.CaptureRecoveryTarget(ctx, replication.StoreCursor{Generation: head.Generation, Sequence: head.Sequence})
		if err != nil {
			return err
		}
		if err := f.replication.coordinator.WaitRecoveryAppliedTarget(ctx, target); err != nil {
			return fmt.Errorf("HA online complete peer application: %w", err)
		}
	}
	return f.business.collector.WithPausedCatchUpWorkspace(ctx, c.Directory.WithPhysicalCatchupWorkspace, func(ctx context.Context) (callbackErr error) {
		changed := false
		defer func() {
			if callbackErr != nil && changed {
				f.recovery.Store(true)
				c.NativeLedger.Status().RequireRecovery()
			}
		}()
		// The collector is excluded but its catch-up RAM loan has ended. Join
		// the original reserve/receiver/applier gates before borrowing again.
		if err := f.withHAPhysicalAdmissionPaused(ctx, func(ctx context.Context) error {
			return c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) (jobErr error) {
				var lease *ledger.MaintenanceLease
				var err error
				if f.replication.role == replication.RoleReplica {
					lease, err = c.NativeLedger.BeginReplicaReadMaintenance(ctx)
				} else if f.replication.role == replication.RolePrimary {
					lease, err = c.NativeLedger.BeginMaintenance(ctx)
				} else {
					return authenticatedstate.ErrSealIneligible
				}
				if err != nil {
					return err
				}
				previous := c.Lease
				f.owners.Components.Lease, f.reopened.Owners.Components.Lease = lease, lease
				defer func() {
					jobErr = errors.Join(jobErr, lease.Close())
					f.owners.Components.Lease, f.reopened.Owners.Components.Lease = previous, previous
				}()
				if err := c.Usage.BindHASelectedTypedMaintenance(ctx, c.NativeLedger, lease, c.Ledger, c.Budget); err != nil {
					return fmt.Errorf("HA online Usage maintenance binding: %w", err)
				}
				if _, err := c.Directory.AdvanceHASelectedMaintenanceFromNative(ctx, c.NativeLedger, lease, c.Ledger); err != nil {
					return fmt.Errorf("HA online Directory catch-up: %w", err)
				}
				base := f.reopened.Seal
				if base.Revision == math.MaxUint64 {
					return authenticatedstate.ErrLimit
				}
				base.Revision++
				changed = true
				publication, err := statecut.PublishHASelectedMaintenanceCapturedStartupCaches(ctx, f.reopened.Owners, base, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
					if err := statecut.RequireNoHAOrderingPeerPromises(ctx, f.reopened.Owners, f.physicalPublication); err != nil {
						return statecut.StartupCacheCandidates{}, err
					}
					return f.prepareHAPhysicalCheckpointCaches(ctx, f.reopened.Seal)
				})
				if err != nil {
					return fmt.Errorf("HA online common publication: %w", err)
				}
				f.reopened.Seal = publication.Seal
				if err := publication.CleanupError(); err != nil {
					return err
				}
				if f.replication.role == replication.RoleReplica {
					if err := c.Ledger.CloseHAReplicaPhysicalCatchup(ctx, c.NativeLedger, lease); err != nil {
						return fmt.Errorf("HA online Replica receive retirement: %w", err)
					}
				}
				if err := f.retireHAPhysicalDerivedVectorAfterCommon(ctx); err != nil {
					return fmt.Errorf("HA online derived retirement: %w", err)
				}
				if err := f.withSelectedCut(ctx, func(ctx context.Context, _ statecut.HASelectedCut) error {
					return f.physicalPublication.ReplenishFrozen(ctx, resourcebudget.MaxCompletionBatch)
				}); err != nil {
					return fmt.Errorf("HA online Ordering replenishment: %w", err)
				}
				if err := f.prepareHAPhysicalDerivedVector(ctx); err != nil {
					return fmt.Errorf("HA online derived preparation: %w", err)
				}
				if err := c.Audit.CheckpointHAPhysicalNative(ctx, true); err != nil {
					return fmt.Errorf("HA online Audit preparation: %w", err)
				}
				if f.replication.role == replication.RoleReplica {
					if err := c.Ledger.PrepareHAReplicaPhysicalCatchup(ctx, c.NativeLedger, lease); err != nil {
						return fmt.Errorf("HA online Replica receive preparation: %w", err)
					}
					if err := c.Metadata.PrepareHAReplicaPhysicalEscrow(ctx, resourcebudget.MaxCompletionBatch); err != nil {
						return fmt.Errorf("HA online Metadata receive preparation: %w", err)
					}
					// Bank observations use the ordinary applied native read lane.
					// Join the maintenance loan first; the original admission pause
					// still owns receiver/applier barriers and excludes all producers.
					if err := lease.Close(); err != nil {
						return fmt.Errorf("HA online Replica maintenance join before observation: %w", err)
					}
				}
				if err := f.refreshHAPhysicalPeerBank(ctx); err != nil {
					return fmt.Errorf("HA online peer bank refresh: %w", err)
				}
				return nil
			})
		}); err != nil {
			return err
		}
		// Advertise after both RAM and receive/source locks exit. The same
		// collector and Metadata pauses still exclude business producers.
		return f.advertiseHAPhysicalPeerBank(ctx)
	})
}
