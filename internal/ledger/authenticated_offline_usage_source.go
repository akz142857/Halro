package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithOfflineMaintenanceUsageSource loans one exact fresh Ledger root and the
// complete frozen native source to a finite offline reducer. The real lease is
// entered and joined; its cancellation and the actual owner's Close cancel the
// entire job. This does not attach or authorize a selected startup owner.
// Callbacks must not reenter/Close the owner, native or lease.
func (p *AuthenticatedLedgerProjection) WithOfflineMaintenanceUsageSource(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, expectedLedgerScope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	if p == nil || native == nil || lease == nil || visit == nil {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || replicated {
		return errors.Join(ErrProjectionSource, err)
	}
	if p.selectedNative.Load() != nil || native.startupProvenance != nil {
		return ErrProjectionSource
	}
	jobCtx, exit, err := lease.enter(ctx)
	if err != nil {
		return err
	}
	defer exit()
	return p.withFrozenUsageSource(jobCtx, native, lease, manager, expectedLedgerScope, visit)
}
