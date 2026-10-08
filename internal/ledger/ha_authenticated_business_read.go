package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Caller owns the actual projection read lane/lock. Ordinary HA business reads
// retain native -> Member -> Ordering permission throughout physical snapshot
// read/Close. Replica roots staged beyond Member applied are refused. A real
// already-frozen startup capability may borrow its own domain; it cannot mint
// live permission or escape the original maintenance job.
func (p *AuthenticatedLedgerProjection) withHASelectedBusinessReadLocked(ctx context.Context, visit func(context.Context, *HASelectedLedgerPosition) error) (resultErr error) {
	native := p.selectedNative.Load()
	if native == nil || native.startupProvenance == nil || native.startupProvenance.ha == nil {
		return visit(ctx, nil)
	}
	h := native.startupProvenance.ha
	callback := func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedLedgerPosition) error {
		if !validHAUsageSourceMode(native, nil, h, a) {
			return ErrProjectionSource
		}
		return visit(ctx, &allowed)
	}
	if scope, ok := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope); ok && scope.owner == h {
		return h.withScope(ctx, callback)
	}
	if scope, ok := ctx.Value(haSelectedMaintenanceScopeKey{}).(*haSelectedMaintenanceScope); ok && scope.lease != nil && scope.lease.log == native && scope.lease.borrowsHASelectedMaintenance(ctx) {
		if replicated, err := scope.lease.ValidateOwner(native); err != nil || !replicated {
			return errors.Join(ErrProjectionSource, err)
		}
		native.mu.Lock()
		defer native.mu.Unlock()
		if native.file == nil || native.closeContext.Err() != nil {
			return ErrProjectionUnavailable
		}
		return h.withLiveScope(ctx, callback)
	}
	credit, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return err
	}
	defer func() {
		resultErr = finishAuthenticatedSourceCredit(credit, resultErr)
		if resultErr != nil && (errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) || errors.Is(resultErr, authenticatedstate.ErrCorrupt)) {
			p.readFailure.CompareAndSwap(nil, &authenticatedLedgerReadFault{err: resultErr})
		}
	}()
	finish, err := native.beginMutation(nil)
	if err != nil {
		return err
	}
	defer finish()
	native.mu.Lock()
	defer native.mu.Unlock()
	if native.file == nil || native.closeContext.Err() != nil {
		return ErrProjectionUnavailable
	}
	return h.withLiveScope(ctx, callback)
}
