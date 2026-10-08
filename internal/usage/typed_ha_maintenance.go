package usage

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// BindHASelectedTypedMaintenance borrows the actual constructor's exclusive
// native lease and independent Member/source permission after live owners join.
// It changes only this exact Usage owner's source lease, never the root or cut.
func (p *DiskProjection) BindHASelectedTypedMaintenance(ctx context.Context, native *ledger.Log, lease *ledger.MaintenanceLease, owner *ledger.AuthenticatedLedgerProjection, manager *resourcebudget.Manager) error {
	if p == nil || native == nil || lease == nil || owner == nil || manager == nil {
		return authenticatedstate.ErrRecovery
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || !replicated {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	return native.WithHASelectedSealAuthority(ctx, func(ctx context.Context, _ authenticatedstate.SealEligibility, allowed ledger.HASelectedLedgerPosition) error {
		if !p.mu.TryLock() {
			return ErrDiskUsageBusy
		}
		defer p.mu.Unlock()
		e := p.engine
		if p.retired.Load() || e == nil || e.cow == nil || !e.selectedHA || !e.live || e.native != native || e.owner != owner || e.budget != manager {
			return authenticatedstate.ErrRecovery
		}
		ref, err := owner.ReadUsageSourceReference(ctx)
		if err != nil {
			return err
		}
		return owner.WithHASelectedMaintenanceUsageSource(ctx, native, lease, manager, ref.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
			ctx = context.WithValue(ctx, typedLiveSourceKey{}, &typedLiveSourceScope{cow: e.cow, native: native, owner: owner, source: source})
			position := source.Reference()
			var anchor ledger.ReplayAnchor
			err := e.scoped(ctx).View(func(tx *usageTx) error {
				state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
				if err != nil || !found {
					return errors.Join(ErrDiskUsageCorrupt, err)
				}
				anchor = usageStateAnchor(state)
				return nil
			})
			if err != nil || anchor != position.Anchor || source.NativeHead() != position.Anchor || position.Anchor.Head.Generation != allowed.Generation || position.Anchor.Head.Sequence != allowed.Sequence || position.Anchor.Hash != allowed.Hash {
				return errors.Join(authenticatedstate.ErrRecovery, err)
			}
			identity, err := e.cow.RootIdentity(ctx)
			if err != nil || identity.Cut != typedUsageCut(anchor) || identity.Scope != typedUsageScope(e.identity, position.Scope) {
				return errors.Join(authenticatedstate.ErrRecovery, err)
			}
			if err := source.Check(ctx); err != nil {
				return err
			}
			// Keep the ordinary dynamic HA read lane. Only the actual frozen
			// maintenance context may borrow this new lease; ordinary callers
			// must still requalify Member on every query, including after Promise.
			e.lease = lease
			return ctx.Err()
		})
	})
}
