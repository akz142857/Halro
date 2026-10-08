package usage

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ValidateTypedSource excludes mixing a frozen writer with another Usage
// owner's source, lease or Ledger projection, even if their cuts are equal.
func (p *DiskProjection) ValidateTypedSource(native *ledger.Log, lease *ledger.MaintenanceLease, owner *ledger.AuthenticatedLedgerProjection, manager *resourcebudget.Manager) error {
	if p == nil || native == nil || lease == nil || owner == nil || manager == nil || !p.mu.TryRLock() {
		return authenticatedstate.ErrRecovery
	}
	defer p.mu.RUnlock()
	if p.retired.Load() || p.engine == nil || p.engine.cow == nil || p.engine.native != native || p.engine.lease != lease || p.engine.owner != owner || p.engine.budget != manager {
		return authenticatedstate.ErrRecovery
	}
	return nil
}

// WithFrozenTypedSealReference freezes the complete direct Usage root/window
// and exact Ledger cut for one bounded caller job. Native and role freezing is
// external. The supplied independent identity/Manager must match this owner.
// Close cancels and joins visit; visit must not reenter/Close p or retain ctx.
func (p *DiskProjection) WithFrozenTypedSealReference(ctx context.Context, manager *resourcebudget.Manager, identity [32]byte, visit func(context.Context, TypedUsageReference) error) (resultErr error) {
	if p == nil || manager == nil || identity == ([32]byte{}) || visit == nil {
		return authenticatedstate.ErrRecovery
	}
	opCtx, cancel := context.WithCancel(ctx)
	p.catchUpMu.Lock()
	if p.retired.Load() || p.catchUpDone != nil {
		p.catchUpMu.Unlock()
		cancel()
		return ErrDiskUsageBusy
	}
	done := make(chan struct{})
	p.catchUpCancel, p.catchUpDone = cancel, done
	p.catchUpActive.Store(true)
	p.catchUpMu.Unlock()
	defer func() {
		cancel()
		p.catchUpMu.Lock()
		p.catchUpCancel, p.catchUpDone = nil, nil
		p.catchUpActive.Store(false)
		close(done)
		p.catchUpMu.Unlock()
	}()
	if !p.mu.TryLock() {
		return ErrDiskUsageBusy
	}
	defer p.mu.Unlock()
	if p.retired.Load() || p.engine == nil || p.engine.cow == nil || p.engine.budget != manager {
		return authenticatedstate.ErrRecovery
	}
	credit, err := manager.TryAcquireContext(opCtx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			p.typedSealDebt = credit
			p.closeErr = errors.Join(p.closeErr, resultErr)
			p.retired.Store(true)
		} else {
			if err := credit.Release(); err != nil {
				resultErr = errors.Join(resultErr, err)
				p.typedSealDebt = credit
				p.closeErr = errors.Join(p.closeErr, resultErr)
				p.retired.Store(true)
			}
		}
	}()
	reference, err := p.typedSealReferenceLocked(opCtx)
	if err != nil {
		return err
	}
	if reference.Projection.Scope != typedUsageScope(identity, reference.Ledger.Scope) {
		return authenticatedstate.ErrRecovery
	}
	if err := visit(opCtx, reference); err != nil {
		return err
	}
	if err := opCtx.Err(); err != nil {
		return err
	}
	final, err := p.typedSealReferenceLocked(opCtx)
	if err != nil || final != reference {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	return opCtx.Err()
}
