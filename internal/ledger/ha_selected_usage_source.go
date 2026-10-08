package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func validHAUsageSourceMode(native *Log, lease *MaintenanceLease, h *haSelectedStartupProvenance, a authenticatedstate.SealEligibility) bool {
	if h == nil || a.Role != h.role || a.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return false
	}
	o := native.options
	if h.role == authenticatedstate.SealReplica {
		return o.Replica && o.BeforeDurable == nil && o.AfterDurable == nil && o.WaitConfirmed == nil && (lease == nil || lease.readOnly)
	}
	return h.role == authenticatedstate.SealPrimary && !o.Replica && o.BeforeDurable != nil && o.AfterDurable != nil && o.WaitConfirmed != nil && (lease == nil || !lease.readOnly)
}

// WithHASelectedFrozenUsageSource follows the actual durable writer's lock
// order: projection, native, then independent Member/Ordering read permission.
// Only a constructor-installed single-Ledger callback can grant it. It neither
// borrows the startup common-cut scope nor grants seven-root publication.
func (p *AuthenticatedLedgerProjection) WithHASelectedFrozenUsageSource(ctx context.Context, native *Log, manager *resourcebudget.Manager, scope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	if ctx.Value(haSelectedStartupScopeKey{}) != nil {
		return ErrProjectionSource
	}
	return p.withHASelectedUsageSource(ctx, native, nil, manager, scope, visit)
}

func (p *AuthenticatedLedgerProjection) WithHASelectedMaintenanceUsageSource(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, scope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	if lease == nil {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || !replicated {
		return errors.Join(ErrProjectionSource, err)
	}
	opCtx, release, err := lease.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	return p.withHASelectedUsageSource(opCtx, native, lease, manager, scope, visit)
}

func (p *AuthenticatedLedgerProjection) withHASelectedUsageSource(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, scope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	if p == nil || native == nil || manager == nil || p.selectedNative.Load() != native || native.startupProvenance == nil || native.startupProvenance.ha == nil || native.startupBudget != manager {
		return ErrProjectionSource
	}
	if err := native.ValidateSelectedStartupRead(ctx); err != nil {
		return err
	}
	return p.withFrozenUsageSourcePermissionsMode(ctx, native, lease, manager, scope, visit, false, false, true)
}
