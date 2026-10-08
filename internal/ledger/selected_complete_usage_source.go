package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithSelectedCompleteUsageSource lends a complete native history only from
// the actual selected Standalone owner after its historical audit has finished.
// It is a scoped reference-build loan, not permission to publish a candidate or
// to bypass the normal selected routine source's saved suffix boundary.
func (p *AuthenticatedLedgerProjection) WithSelectedCompleteUsageSource(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, scope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	if p == nil || native == nil || manager == nil || visit == nil || p.selectedNative.Load() != native || native.startupProvenance == nil || native.startupProvenance.ha != nil || native.startupBudget != manager {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || replicated || lease.readOnly {
		return errors.Join(ErrProjectionSource, err)
	}
	if err := native.validateSelectedStartupGrant(ctx, true); err != nil {
		return err
	}
	jobCtx, exit, err := lease.enter(ctx)
	if err != nil {
		return err
	}
	defer exit()
	return p.withFrozenUsageSourcePermissions(jobCtx, native, lease, manager, scope, visit, false, true)
}
