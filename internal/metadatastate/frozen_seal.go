package metadatastate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithFrozenStandaloneSealReference additionally verifies the actual native
// mode inside its freeze; prepared format2 roots never imply HA permission.
func (b *Baseline) WithFrozenStandaloneSealReference(ctx context.Context, manager *resourcebudget.Manager, identity [32]byte, instanceID string, keyGeneration uint64, visit func(context.Context, AuthenticatedMetadataSealReference, metadatajournal.Head) error) error {
	if visit == nil {
		return ErrSource
	}
	return b.WithFrozenSealReference(ctx, manager, identity, instanceID, keyGeneration, func(ctx context.Context, ref AuthenticatedMetadataSealReference, head metadatajournal.Head) error {
		if err := b.journal.ValidateFrozenStandalone(); err != nil {
			return err
		}
		return visit(ctx, ref, head)
	})
}

// WithFrozenSealReference excludes all baseline reducers/trim/epoch changes and
// freezes/authenticates its actual format2 native source while one root receipt
// is lent to visit. Identity and key generation must be independently supplied.
// Close cancels and joins this callback. It grants no role or common-cut policy;
// visit must not reenter/Close this baseline or retain its context/receipt.
func (b *Baseline) WithFrozenSealReference(ctx context.Context, manager *resourcebudget.Manager, identity [32]byte, instanceID string, keyGeneration uint64, visit func(context.Context, AuthenticatedMetadataSealReference, metadatajournal.Head) error) (resultErr error) {
	if b == nil || manager == nil || identity == ([32]byte{}) || b.budget != manager || b.evidence.Identity != identity || b.evidence.InstanceID != instanceID || b.evidence.KeyGeneration != keyGeneration || visit == nil {
		return ErrSource
	}
	credit, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return err
	}

	if !b.mu.TryLock() {
		return errors.Join(authenticatedstate.ErrBusy, credit.Release())
	}
	defer b.mu.Unlock()
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			b.sealDebt = credit
			b.retirementErr = errors.Join(b.retirementErr, resultErr)
			b.ready.Store(false)
		} else {
			if err := credit.Release(); err != nil {
				resultErr = errors.Join(resultErr, err)
				b.sealDebt = credit
				b.retirementErr = errors.Join(b.retirementErr, resultErr)
				b.ready.Store(false)
			}
		}
	}()
	if !b.ready.Load() || !b.linked || b.journal == nil {
		return authenticatedstate.ErrClosed
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	return b.journal.WithFrozenHead(opCtx, manager, func(ctx context.Context, head metadatajournal.Head) error {
		reference, err := b.sealReferenceLocked(ctx)
		if err != nil {
			return err
		}
		if err := visit(ctx, reference, head); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		final, err := b.sealReferenceLocked(ctx)
		if err != nil || final != reference {
			return errors.Join(ErrSource, err)
		}
		return ctx.Err()
	})
}
