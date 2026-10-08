package ledger

import (
	"context"
	"errors"
)

type selectedStartupScopeKey struct{}

// ValidateSelectedStartupRead checks the finite native history grant without
// reentering native.mu. The only initialization exception is the unexported
// context capability from the actual WithSelectedStartupOwner source loan.
// It expires at callback exit and permits no later business use.
func (l *Log) ValidateSelectedStartupRead(ctx context.Context) error {
	return l.validateSelectedStartupGrant(ctx, false)
}

func (l *Log) validateSelectedStartupGrant(ctx context.Context, structural bool) error {
	if l == nil {
		return ErrProjectionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.closeContext.Err() != nil || l.status.Load() != AccountingHealthy {
		return ErrProjectionUnavailable
	}
	if l.startupProvenance == nil {
		return nil
	}
	if source, ok := ctx.Value(selectedStartupScopeKey{}).(*selectedStartupSource); ok && source.native == l {
		return source.Check(ctx)
	}
	if l.startupVerified.Load() {
		return nil
	}
	a := l.startupAuditPtr.Load()
	if a == nil {
		return ErrStartupAuditPending
	}
	p := a.Snapshot()
	if p.Err != nil || !p.Ready || structural && !p.Complete {
		return errors.Join(ErrStartupAuditPending, p.Err)
	}
	return nil
}

func (p *AuthenticatedLedgerProjection) selectedGrantError(ctx context.Context, structural bool) error {
	if native := p.selectedNative.Load(); native != nil {
		return native.validateSelectedStartupGrant(ctx, structural)
	}
	return nil
}
