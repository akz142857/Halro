package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithOfflineHAMaintenanceUsageSource lends a fresh root and complete frozen
// native facts under a real Replica read lease. The HA caller owns independent
// Member/Ordering permission; neither this lease nor a saved root confirms data.
func (p *AuthenticatedLedgerProjection) WithOfflineHAMaintenanceUsageSource(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, expectedLedgerScope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	if p == nil || native == nil || lease == nil || !lease.readOnly || !native.options.Replica || visit == nil || p.selectedNative.Load() != nil || native.startupProvenance != nil {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || !replicated {
		return errors.Join(ErrProjectionSource, err)
	}
	jobCtx, exit, err := lease.enter(ctx)
	if err != nil {
		return err
	}
	defer exit()
	return p.withFrozenUsageSourceMode(jobCtx, native, lease, manager, expectedLedgerScope, func(ctx context.Context, source AuthenticatedUsageSource) error {
		if source.Reference().Anchor != source.NativeHead() {
			return ErrProjectionSource
		}
		return visit(ctx, source)
	}, true)
}

// BuildHAAuthenticatedDirectory commits the complete physical descriptors and
// exact fresh Ledger anchor under a read-only Replica lease. Before constructing
// the caller must establish that this full cut is independently role permitted;
// PublishHA authenticates that same cut again before admitting the seven roots.
func BuildHAAuthenticatedDirectory(ctx context.Context, native *Log, lease *MaintenanceLease, owner *AuthenticatedLedgerProjection, output string, o AuthenticatedDirectoryOptions) (*AuthenticatedDirectory, error) {
	return buildAuthenticatedDirectory(ctx, native, lease, owner, output, o, true)
}

// OpenHAAuthenticatedDirectory authenticates a saved common directory under
// the same actual read lease. It grants no native mutation or role permission.
func OpenHAAuthenticatedDirectory(ctx context.Context, native *Log, lease *MaintenanceLease, selected AuthenticatedDirectoryReference, output string, o AuthenticatedDirectoryOptions) (*AuthenticatedDirectory, error) {
	return openAuthenticatedDirectory(ctx, native, lease, selected, output, o, true)
}
