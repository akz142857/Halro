package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
)

var errAccountingRecoveryBusy = errors.New("accounting recovery is already running")

// Own the full algorithm, not just individual writer calls. A cancelled caller
// cannot retire pages/encoding/native IO while recovery is still on the stack.
// The writer remains private and its Close joins accepted IO before retirement.
func (r *Runtime) recoverProjectedAccounting(ctx context.Context, lease *ledger.MaintenanceLease, projection *ledger.LedgerProjection, role budget.RecoveryRole) (result budget.ProjectedRecoveryResult, resultErr error) {
	if r == nil || projection == nil || r.accountingState.projection != projection || r.accountingState.recovery == nil || (role != budget.RecoveryStandalone && role != budget.RecoveryPrimary) {
		return result, errors.New("invalid accounting recovery owner or role")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !r.accountingState.recovery.CompareAndSwap(false, true) {
		return result, errAccountingRecoveryBusy
	}
	defer r.accountingState.recovery.Store(false)
	releaseWorkspace, err := acquireProjectionWorkspace(ctx, r.config, r.resourcePressure)
	if err != nil {
		return result, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, releaseWorkspace())
		}
	}()
	w, err := r.newProjectedRecoveryWriter(ctx, lease, projection)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, w.Close()) }()
	if w.role != role {
		return result, errors.New("accounting recovery role differs from native authority")
	}
	return budget.RecoverProjectedRequests(ctx, projection, w, role, ledger.ProjectionPageLimits{Rows: 64, Bytes: 8 << 20})
}
