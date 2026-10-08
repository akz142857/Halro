package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithFrozenSealReference pins the independently qualified complete component
// root and position across one callback; writes cannot split that receipt.
// Close cancels and joins the callback. Native/role/common-cut authority and
// directory ownership remain separate. The callback must not reenter/Close p.
func (p *AuthenticatedLedgerProjection) WithFrozenSealReference(ctx context.Context, manager *resourcebudget.Manager, identity [32]byte, visit func(context.Context, AuthenticatedLedgerSealReference) error) (resultErr error) {
	if identity == ([32]byte{}) {
		return ErrProjectionSource
	}
	return p.withFrozenSealReference(ctx, manager, authenticatedLedgerScope(identity), visit)
}

func (p *AuthenticatedLedgerProjection) withFrozenSealReference(ctx context.Context, manager *resourcebudget.Manager, scope [32]byte, visit func(context.Context, AuthenticatedLedgerSealReference) error) error {
	return p.withFrozenRootReference(ctx, manager, scope, true, visit)
}
func (p *AuthenticatedLedgerProjection) withFrozenRootReference(ctx context.Context, manager *resourcebudget.Manager, scope [32]byte, structural bool, visit func(context.Context, AuthenticatedLedgerSealReference) error) (resultErr error) {
	if p == nil || manager == nil || scope == ([32]byte{}) || p.budget != manager || p.scope != scope || visit == nil {
		return ErrProjectionSource
	}
	if !p.sealActive.CompareAndSwap(false, true) {
		return fmt.Errorf("Ledger frozen-root callback already active: %w", authenticatedstate.ErrBusy)
	}
	credit, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		p.sealActive.Store(false)
		return err
	}

	if !p.mu.TryLock() {
		p.sealActive.Store(false)
		return errors.Join(fmt.Errorf("Ledger frozen-root projection lock: %w", authenticatedstate.ErrBusy), credit.Release())
	}
	defer p.mu.Unlock()
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			p.sealDebt = credit
			p.sealErr = resultErr
			p.readFailure.CompareAndSwap(nil, &authenticatedLedgerReadFault{err: resultErr})
		} else {
			if err := credit.Release(); err != nil {
				resultErr = errors.Join(resultErr, err)
				p.sealDebt = credit
				p.sealErr = resultErr
				p.readFailure.CompareAndSwap(nil, &authenticatedLedgerReadFault{err: resultErr})
			} else {
				p.sealActive.Store(false)
			}
		}
	}()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.closeContext, cancel)
	defer func() { stop(); cancel() }()
	reference, err := p.rootReferenceLocked(opCtx, structural)
	if err != nil {
		return err
	}
	if err := visit(opCtx, reference); err != nil {
		return err
	}
	if err := errors.Join(opCtx.Err(), p.closeContext.Err(), p.readFault()); err != nil {
		return err
	}
	final, err := p.rootReferenceLocked(opCtx, structural)
	if err != nil || final != reference {
		return errors.Join(ErrProjectionSource, err)
	}
	return opCtx.Err()
}
