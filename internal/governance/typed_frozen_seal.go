package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func (p *GovernanceProjection) ValidateTypedSource(native *Log, manager *resourcebudget.Manager) error {
	if p == nil || native == nil || manager == nil || !p.mu.TryRLock() {
		return authenticatedstate.ErrRecovery
	}
	defer p.mu.RUnlock()
	if p.retired.Load() || !p.ready || p.cow == nil || p.cowNative != native || p.cowBudget != manager {
		return authenticatedstate.ErrRecovery
	}
	return nil
}

// WithFrozenTypedSealReference excludes all component reducers during one
// qualified root/native-position callback. Native writer/role freezing remains
// external. Close cancels and joins the entire job before physical retirement.
// visit must not reenter/Close p or retain the context/reference.
func (p *GovernanceProjection) WithFrozenTypedSealReference(ctx context.Context, manager *resourcebudget.Manager, identity [32]byte, visit func(context.Context, TypedGovernanceReference) error) (resultErr error) {
	if p == nil || manager == nil || identity == ([32]byte{}) || visit == nil {
		return authenticatedstate.ErrRecovery
	}
	opCtx, cancel := context.WithCancel(ctx)
	p.advanceMu.Lock()
	if p.retired.Load() || p.advanceDone != nil {
		p.advanceMu.Unlock()
		cancel()
		return authenticatedstate.ErrBusy
	}
	done := make(chan struct{})
	p.advanceCancel, p.advanceDone = cancel, done
	p.advanceMu.Unlock()
	defer func() {
		cancel()
		p.advanceMu.Lock()
		p.advanceCancel, p.advanceDone = nil, nil
		close(done)
		p.advanceMu.Unlock()
	}()
	if !p.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer p.mu.Unlock()
	if p.retired.Load() || !p.ready || p.cow == nil || p.cowBudget != manager {
		return authenticatedstate.ErrRecovery
	}
	credit, err := manager.TryAcquireContext(opCtx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			p.sealDebt = credit
			p.retirementErr = errors.Join(p.retirementErr, resultErr)
			p.retired.Store(true)
			p.ready = false
		} else {
			if err := credit.Release(); err != nil {
				resultErr = errors.Join(resultErr, err)
				p.sealDebt = credit
				p.retirementErr = errors.Join(p.retirementErr, resultErr)
				p.retired.Store(true)
				p.ready = false
			}
		}
	}()
	reference, err := p.typedSealReferenceLocked(opCtx)
	if err != nil {
		return err
	}
	if reference.Projection.Scope != typedGovernanceScope(identity) {
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
