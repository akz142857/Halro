package ledger

import (
	"context"
	"errors"
)

// ValidateHASelectedFrozenMaintenanceRead recognizes only this constructor's
// actual exclusive maintenance loan and still-active full Member/source scope.
// It cannot open a loan, turn a live single-source permission into a common cut,
// or admit writes. A matching caller-supplied role/head grants no permission.
func (l *Log) ValidateHASelectedFrozenMaintenanceRead(ctx context.Context, lease *MaintenanceLease) error {
	if l == nil || lease == nil || l.startupProvenance == nil || l.startupProvenance.ha == nil || !lease.borrowsHASelectedMaintenance(ctx) {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(l); err != nil || !replicated {
		return errors.Join(ErrProjectionSource, err)
	}
	h := l.startupProvenance.ha
	scope, ok := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
	if !ok || scope.owner != h || !scope.active.Load() || ctx.Err() != nil || l.closeContext.Err() != nil {
		return errors.Join(ErrProjectionSource, ctx.Err(), l.closeContext.Err())
	}
	release, err := scope.borrow(ctx)
	if err != nil {
		return err
	}
	defer release()
	return l.validateSelectedStartupGrant(ctx, true)
}
