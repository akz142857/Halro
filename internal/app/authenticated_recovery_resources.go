package app

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"math"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// This explicit maintenance job requires a candidate built against the SAME
// Runtime Manager and the accounting owner's shared zero-queue recovery slot.
// Its whole algorithm/encoding/native IO/confirmation lifetime is charged before
// writer construction, then joined before credit/slot retirement. It neither
// selects a Runtime backend nor creates/reconciles persistent disk ownership.
// Temporary source/query/update operations borrow callback-scoped Recovery
// authority; persistent disk ownership must already exist independently.
func (r *Runtime) recoverAuthenticatedAccounting(ctx context.Context, lease *ledger.MaintenanceLease, projection *ledger.AuthenticatedLedgerProjection, role budget.RecoveryRole) (result budget.ProjectedRecoveryResult, resultErr error) {
	if r == nil || r.ledger == nil || projection == nil || r.config.ResourceBudget == nil || r.accountingState.recovery == nil || !r.draining.Load() || (role != budget.RecoveryStandalone && role != budget.RecoveryPrimary) {
		return result, errors.New("authenticated recovery requires configured shared owners, withdrawn admission and a valid role")
	}
	manager := r.resourcePressure.logicalBudget()
	if err := projection.ValidateResourceOwner(ctx, manager); err != nil {
		return result, err
	}
	if !r.accountingState.recovery.CompareAndSwap(false, true) {
		return result, errAccountingRecoveryBusy
	}
	defer r.accountingState.recovery.Store(false)
	b := r.config.ResourceBudget
	// Parent workspace plus the fixed recovery view, native source, point job,
	// query and two simultaneous snapshot pins. This structural floor is not
	// physical calibration or a guarantee against other live Recovery owners.
	const descendants = ledger.AuthenticatedRecoveryRAMBytes + ledger.AuthenticatedLedgerSourceRAMBytes + ledger.AuthenticatedLedgerReadRAMBytes + authenticatedstate.ReadRAMBytes + authenticatedstate.UpdateRAMBytes + 2*4096
	const claims = 8
	amount := b.AccountingRecoveryRAMBytes
	if amount < accountinglimits.MinRecoveryRAMBytes || amount > math.MaxInt64-descendants-claims*resourcebudget.ClaimRAMBytes || b.MaxOwners < 2 || b.RecoveryClaims < claims || b.RecoveryRAMBytes < amount+descendants || b.RecoveryReserveRAMBytes < amount+descendants+claims*resourcebudget.ClaimRAMBytes {
		return result, errors.New("authenticated recovery requires workspace and pre-admitted COW descendant Recovery capacity, eight claims and two owners")
	}
	workspace, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: amount})
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, workspace.Release()) }()
	resultErr = workspace.WithRecoveryScope(ctx, func(scopedCtx context.Context) (jobErr error) {
		w, err := r.newAuthenticatedRecoveryWriter(scopedCtx, lease, projection)
		if err != nil {
			return err
		}
		defer func() { jobErr = errors.Join(jobErr, w.Close()) }()
		if w.role != role {
			return errors.New("authenticated recovery role differs from native authority")
		}
		result, jobErr = budget.RecoverAuthenticatedRequests(scopedCtx, projection, w, role, ledger.ProjectionPageLimits{Rows: 64, Bytes: 8 << 20})
		return jobErr
	})
	return result, resultErr
}
