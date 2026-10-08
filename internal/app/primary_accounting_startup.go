package app

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

func (r *Runtime) awaitPrimaryStartup(ctx context.Context) bool {
	if r.replication == nil || r.replication.startupProducers == nil {
		return ctx.Err() == nil
	}
	select {
	case <-r.replication.startupProducers:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

// Transport is active, ordinary producers have not started and the selected
// native/projection lease is still owned. Only this private maintenance adapter
// can write recovery facts; it is never a Standalone confirmation substitute.
func (r *Runtime) finishPrimaryAccountingStartup(ctx context.Context) (err error) {
	defer func() {
		err = errors.Join(err, r.accountingState.releaseStartupLease())
		if err != nil {
			r.ledger.Status().RequireRecovery()
		} else if r.usage != nil && r.usage.owner != nil {
			r.usage.owner.activateArchiveIO()
		}
	}()
	maintenance := &Runtime{config: r.config, ledger: r.ledger, replication: r.replication, runtimeResourceOwnership: runtimeResourceOwnership{resourcePressure: r.resourcePressure}, accountingState: r.accountingState}
	maintenance.draining.Store(true)
	_, err = maintenance.recoverProjectedAccounting(ctx, r.accountingState.startupLease, r.accountingState.projection, budget.RecoveryPrimary)
	if err != nil {
		return err
	}
	if err = r.usageCollector.CatchUp(ctx); err != nil {
		return err
	}
	// Recovery may produce late old-date facts. Reapply the query window before
	// admitting any reader/producer, using a fresh exclusive native lease.
	lease, err := r.ledger.BeginMaintenance(ctx)
	if err != nil {
		return err
	}
	_, windowErr := r.restoreUsageDiskWindow(ctx, lease, time.Now())
	if windowErr == nil {
		// Window/anchor repair can add Metadata facts after recovery's final
		// confirmation. Readiness covers the entire final ordering prefix.
		windowErr = lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
			actual, actualHash, err := r.accountingState.projection.ReadPosition(ctx)
			if err != nil || actual != head || actualHash != hash {
				return errors.Join(err, errors.New("Primary startup final projection differs from native"))
			}
			target, err := r.replication.coordinator.CaptureRecoveryTarget(ctx, replication.StoreCursor{Generation: head.Generation, Sequence: head.Sequence})
			if err != nil {
				return err
			}
			return r.replication.coordinator.WaitRecoveryTarget(ctx, target)
		})
	}
	err = errors.Join(windowErr, lease.Close())
	if err != nil {
		return err
	}
	// Final readiness joins the independently authenticated member file,
	// complete ordering source and all four native permitted heads. Primary
	// local Apply can lead confirmation during normal recovery; at this final
	// boundary the entire startup target must have become transferable.
	cut, err := r.replication.authenticateHARuntimeCut(ctx, r.resourcePressure.logicalBudget())
	if err != nil {
		return err
	}
	if cut.member.DurableIndex != cut.member.ConfirmedIndex || cut.member.ConfirmedIndex != cut.member.AppliedIndex {
		return errors.New("Primary startup common cut has not been fully confirmed and applied")
	}
	return nil
}
