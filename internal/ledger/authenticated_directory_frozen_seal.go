package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithFrozenSealReference keeps this complete directory receipt alive through
// one common-publication callback. It authenticates the independently supplied
// identity and resource owner; it does not authorize a native or HA cut.
// Close and Discard cancel and join the callback before retiring pages or fees.
// visit must not reenter or close d.
func (d *AuthenticatedDirectory) WithFrozenSealReference(ctx context.Context, manager *resourcebudget.Manager, identity [32]byte, visit func(context.Context, AuthenticatedDirectoryReference) error) (resultErr error) {
	if d == nil || manager == nil || identity == ([32]byte{}) || d.budget != manager || d.scope != authenticatedDirectoryScope(identity) || visit == nil {
		return ErrProjectionSource
	}
	if !d.sealActive.CompareAndSwap(false, true) {
		return authenticatedstate.ErrBusy
	}
	credit, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		d.sealActive.Store(false)
		return err
	}
	if !d.mu.TryLock() {
		d.sealActive.Store(false)
		return errors.Join(authenticatedstate.ErrBusy, credit.Release())
	}
	defer d.mu.Unlock()
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			if err := credit.Release(); err == nil {
				d.sealActive.Store(false)
				return
			} else {
				resultErr = errors.Join(resultErr, err)
			}
		}
		d.sealDebt = credit
		d.sealErr = resultErr
	}()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.closeCtx, cancel)
	defer func() { stop(); cancel() }()
	reference, err := d.sealReferenceLocked(opCtx)
	if err != nil {
		return err
	}
	if err := visit(opCtx, reference); err != nil {
		return err
	}
	final, err := d.sealReferenceLocked(opCtx)
	if err != nil || final != reference {
		return errors.Join(ErrProjectionSource, err)
	}
	return opCtx.Err()
}
