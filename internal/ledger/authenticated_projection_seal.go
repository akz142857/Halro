package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// SealReference exports a stable Ledger component root and its exact native
// position under one projection read lock. Advance and Close cannot split the
// returned pair. The root represents the complete ordinal-keyed Ledger
// namespace as one logical index; it is not a collection of per-bucket roots.
func (p *AuthenticatedLedgerProjection) SealReference(ctx context.Context) (result AuthenticatedLedgerSealReference, resultErr error) {
	if p == nil {
		return result, fmt.Errorf("Ledger component owner missing: %w", ErrProjectionUnavailable)
	}
	if !p.mu.TryRLock() {
		return result, fmt.Errorf("Ledger component read lock: %w", authenticatedstate.ErrBusy)
	}
	defer p.mu.RUnlock()
	return p.sealReferenceLocked(ctx)
}

// ReadUsageSourceReference supplies the actual immutable root/anchor for a
// routine source read. It grants no structural or common publication authority.
func (p *AuthenticatedLedgerProjection) ReadUsageSourceReference(ctx context.Context) (result AuthenticatedLedgerSealReference, resultErr error) {
	if p != nil && p.businessReads.Load() {
		release, err := p.admitBusinessRead(ctx)
		if err != nil {
			return AuthenticatedLedgerSealReference{}, err
		}
		defer func() {
			resultErr = errors.Join(resultErr, release())
			if resultErr != nil {
				result = AuthenticatedLedgerSealReference{}
			}
		}()
		return p.rootReferenceLocked(ctx, false)
	}
	if p == nil || !p.mu.TryRLock() {
		return AuthenticatedLedgerSealReference{}, authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	return p.rootReferenceLocked(ctx, false)
}

func (p *AuthenticatedLedgerProjection) sealReferenceLocked(ctx context.Context) (AuthenticatedLedgerSealReference, error) {
	return p.rootReferenceLocked(ctx, true)
}
func (p *AuthenticatedLedgerProjection) rootReferenceLocked(ctx context.Context, structural bool) (result AuthenticatedLedgerSealReference, resultErr error) {
	if err := p.selectedGrantError(ctx, structural); err != nil {
		return result, err
	}
	if !p.ready || p.readFailure.Load() != nil || p.closeContext.Err() != nil {
		return result, errors.Join(ErrProjectionUnavailable, p.failure, p.readFault())
	}
	permit, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, ctx.Err(), p.closeContext.Err(), p.readFault(), p.selectedGrantError(ctx, structural))
		resultErr = finishAuthenticatedSourceCredit(permit, resultErr)
		if resultErr != nil {
			result = AuthenticatedLedgerSealReference{}
			if errors.Is(resultErr, authenticatedstate.ErrCorrupt) || errors.Is(resultErr, ErrProjectionCorrupt) {
				p.readFailure.CompareAndSwap(nil, &authenticatedLedgerReadFault{err: resultErr})
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := p.readFault(); err != nil {
		return result, err
	}
	pin, err := p.store.Snapshot(ctx)
	if err != nil {
		return result, fmt.Errorf("Ledger component snapshot: %w", err)
	}
	position, found, readErr := authenticatedPointGet[projectionPosition](ctx, pin, "meta", []byte("position"))
	closeErr := pin.Close()
	if err := errors.Join(readErr, closeErr); err != nil || !found {
		return result, errors.Join(ErrProjectionCorrupt, err)
	}
	anchor := ReplayAnchor{Head: position.Watermark, Hash: position.Hash, Epoch: position.Epoch}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState,
		Component: authenticatedstate.SealComponentLedger,
		IndexID:   AuthenticatedLedgerSealIndexID,
		Codec:     AuthenticatedLedgerSealCodec,
	}
	root, err := p.store.SealRootReference(ctx, schema)
	if err != nil {
		return result, fmt.Errorf("Ledger component Store root: %w", err)
	}
	identity, err := p.store.RootIdentity(ctx)
	if err != nil {
		return result, err
	}
	if identity.Scope != p.scope || identity.Cut != authenticatedLedgerCut(p.scope, anchor) || identity.ID != root.FileID && !root.Empty || identity.Rows != root.Rows || identity.Revision == 0 {
		return result, ErrProjectionCorrupt
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return AuthenticatedLedgerSealReference{Schema: schema, Root: root, Scope: p.scope, Anchor: anchor}, nil
}
