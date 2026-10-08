package app

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// This adapter retains the actual HA selected projection, never a caller-made
// standalone confirmation or compatibility DB. The shared writer still owns
// native receipts, full Ordering RecoveryTargets, withdrawal and term checks.
type haSelectedRecoveryProjection struct {
	authenticatedRecoveryProjection
}

func (f *haSelectedRuntimeFoundation) newHASelectedRecoveryWriter(ctx context.Context, lease *ledger.MaintenanceLease) (*projectedRecoveryWriter, error) {
	if f == nil || f.reopened == nil || f.replication == nil || f.replication.role != replication.RolePrimary || f.ctx.Err() != nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	c := f.reopened.Owners.Components
	if err := c.Ledger.ValidateHASelectedNativeWriter(ctx, c.NativeLedger, lease, c.Budget); err != nil {
		return nil, err
	}
	maintenance := &Runtime{config: f.cfg, ledger: c.NativeLedger, replication: f.replication}
	maintenance.draining.Store(true)
	w, err := maintenance.newRecoveryProjectionWriter(ctx, lease, haSelectedRecoveryProjection{authenticatedRecoveryProjection{c.Ledger, c.NativeLedger}})
	if err != nil {
		return nil, err
	}
	w.borrowedLease = true
	return w, nil
}

// Ordinary producers must never be started before this startup-only call. All
// installed component managers and their actual collector are withdrawn and
// joined first. The actual startup lease remains owned through later service
// reconstruction and final all-source confirmation; this method grants no Ready.
func (f *haSelectedRuntimeFoundation) recoverHASelectedAccounting(ctx context.Context, lease *ledger.MaintenanceLease) (result budget.ProjectedRecoveryResult, resultErr error) {
	return f.recoverHASelectedAccountingMode(ctx, lease, false)
}

func (f *haSelectedRuntimeFoundation) recoverHASelectedAccountingMode(ctx context.Context, lease *ledger.MaintenanceLease, keepServices bool) (result budget.ProjectedRecoveryResult, resultErr error) {
	if f == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	f.lifecycle.Lock()
	if f.closing || f.activeCallbacks >= 4 || f.ctx.Err() != nil {
		f.lifecycle.Unlock()
		return result, authenticatedstate.ErrClosed
	}
	f.activeCallbacks++
	f.callbacks.Add(1)
	f.lifecycle.Unlock()
	defer func() { f.lifecycle.Lock(); f.activeCallbacks--; f.lifecycle.Unlock(); f.callbacks.Done() }()
	if f.reopened == nil || f.cfg.ResourceBudget == nil || f.replication.role != replication.RolePrimary || f.replication.startupReady.Load() {
		return result, authenticatedstate.ErrSealIneligible
	}
	if !f.recovery.CompareAndSwap(false, true) {
		return result, errAccountingRecoveryBusy
	}
	defer f.recovery.Store(false)
	if keepServices {
		// Only the privately installed startup continuation may borrow these
		// not-yet-published business handles. Runtime producers wait for its
		// startupProducers channel, and actual Managers join accepted calls.
		if f.replication.startupFinish == nil || f.replication.startupProducers == nil || f.business == nil || f.business.accounting == nil {
			return result, authenticatedstate.ErrSealIneligible
		}
		var resumeGovernance func() error
		if f.business.governance != nil {
			var err error
			resumeGovernance, err = f.business.governance.PauseOperations(ctx)
			if err != nil {
				return result, err
			}
		}
		resumeAccounting, err := f.business.accounting.PauseOperations(ctx)
		if err != nil {
			return result, err
		}
		defer func() {
			if resultErr == nil {
				resultErr = errors.Join(resultErr, resumeAccounting())
				if resumeGovernance != nil {
					resultErr = errors.Join(resultErr, resumeGovernance())
				}
			}
		}()
	} else {
		if err := f.business.close(ctx); err != nil {
			return result, err
		}
		f.business = nil
	}
	c, b := f.reopened.Owners.Components, f.cfg.ResourceBudget
	if err := c.Ledger.ValidateHASelectedNativeWriter(ctx, c.NativeLedger, lease, c.Budget); err != nil {
		return result, fmt.Errorf("HA recovery original writer qualification: %w", err)
	}
	const descendants = ledger.AuthenticatedRecoveryRAMBytes + ledger.AuthenticatedLedgerSourceRAMBytes + ledger.AuthenticatedLedgerReadRAMBytes + authenticatedstate.ReadRAMBytes + authenticatedstate.UpdateRAMBytes + 2*4096
	const claims = 8
	amount := b.AccountingRecoveryRAMBytes
	if amount < accountinglimits.MinRecoveryRAMBytes || amount > math.MaxInt64-descendants-claims*resourcebudget.ClaimRAMBytes || b.MaxOwners < 2 || b.RecoveryClaims < claims || b.RecoveryRAMBytes < amount+descendants || b.RecoveryReserveRAMBytes < amount+descendants+claims*resourcebudget.ClaimRAMBytes {
		return result, errors.New("HA selected recovery requires bounded workspace and eight pre-admitted descendant claims")
	}
	workspace, err := c.Budget.TryAcquireRecovery(resourcebudget.Cost{RAM: amount})
	if err != nil {
		return result, fmt.Errorf("HA recovery original workspace reservation: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, workspace.Release()) }()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	// The outer Directory preparation pool pays for finite qualification reads.
	// Recovery owns a separately charged, larger lifetime and its descendants;
	// do not let that outer profile intercept the recovery view's 64 MiB claim.
	// Cancellation and original source/lease authority remain on the context.
	opCtx = resourcebudget.WithoutRAMWorkspace(opCtx)
	resultErr = workspace.WithRecoveryScope(opCtx, func(ctx context.Context) (jobErr error) {
		return lease.WithHASelectedRecoveryContext(ctx, func(ctx context.Context) (jobErr error) {
			w, err := f.newHASelectedRecoveryWriter(ctx, lease)
			if err != nil {
				return fmt.Errorf("HA recovery scoped writer construction: %w", err)
			}
			defer func() { jobErr = errors.Join(jobErr, w.Close()) }()
			result, jobErr = budget.RecoverAuthenticatedRequests(ctx, c.Ledger, w, budget.RecoveryPrimary, ledger.ProjectionPageLimits{Rows: 64, Bytes: 8 << 20})
			if jobErr != nil {
				jobErr = fmt.Errorf("HA recovery complete two-stream walk: %w", jobErr)
			}
			return jobErr
		})
	})
	return result, resultErr
}

var _ budget.RecoveryWriter = (*projectedRecoveryWriter)(nil)
