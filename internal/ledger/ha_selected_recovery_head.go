package ledger

import (
	"context"
	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// WithHASelectedRecoveryHead lends only the actual already-entered Primary
// maintenance gate to nested selected position reads. It captures no Member
// permission itself: each selected read must independently obtain its short
// source loan. Quorum waits can therefore run after that loan has exited.
func (m *MaintenanceLease) WithHASelectedRecoveryHead(ctx context.Context, visit func(context.Context, Watermark, [32]byte) error) error {
	if m == nil || m.log == nil || m.readOnly || visit == nil {
		return ErrProjectionSource
	}
	l := m.log
	if l.startupProvenance == nil || l.startupProvenance.ha == nil || l.startupProvenance.ha.role != authenticatedstate.SealPrimary || l.options.Replica || l.options.BeforeDurable == nil || l.options.AfterDurable == nil || l.options.WaitConfirmed == nil {
		return ErrProjectionSource
	}
	return m.WithHead(ctx, func(ctx context.Context, head Watermark, hash [32]byte) error {
		scope := &haSelectedMaintenanceScope{lease: m}
		scope.active.Store(true)
		defer scope.active.Store(false)
		return visit(context.WithValue(ctx, haSelectedMaintenanceScopeKey{}, scope), head, hash)
	})
}

// WithHASelectedRecoveryContext owns the actual exclusive lease gate through
// the whole bounded recovery algorithm. Its selected reads acquire short
// independent Member/source loans; its semantic writes still require the real
// Primary hooks and native AppendApply receipts. Callers must join all work
// before returning; the context cannot reauthorize a later recovery job.
func (m *MaintenanceLease) WithHASelectedRecoveryContext(ctx context.Context, visit func(context.Context) error) error {
	if m == nil || m.log == nil || m.readOnly || visit == nil {
		return ErrProjectionSource
	}
	l := m.log
	if l.startupProvenance == nil || l.startupProvenance.ha == nil || l.startupProvenance.ha.role != authenticatedstate.SealPrimary || l.options.Replica || l.options.BeforeDurable == nil || l.options.AfterDurable == nil || l.options.WaitConfirmed == nil {
		return ErrProjectionSource
	}
	opCtx, release, err := m.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	finish, err := l.beginMutation(m)
	if err != nil {
		return err
	}
	defer finish()
	scope := &haSelectedMaintenanceScope{lease: m}
	scope.active.Store(true)
	defer scope.active.Store(false)
	return visit(context.WithValue(opCtx, haSelectedMaintenanceScopeKey{}, scope))
}
