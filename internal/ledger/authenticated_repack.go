package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func (p *AuthenticatedLedgerProjection) PrepareSelectedRepack(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, selected authenticatedstate.CandidateSeal, reservation resourcebudget.Permit) (*authenticatedstate.StoreRepack, error) {
	if p == nil || !p.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer p.mu.Unlock()
	ref, err := p.rootReferenceLocked(ctx, true)
	if err != nil {
		return nil, err
	}
	return p.store.PrepareRepack(ctx, publisher, guard, selected, ref.Schema, reservation)
}

// PrepareSelectedRepackForPhysicalHeadroom uses the same independently selected
// root and paused projection as ordinary repack, with the physical demand gate.
func (p *AuthenticatedLedgerProjection) PrepareSelectedRepackForPhysicalHeadroom(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, selected authenticatedstate.CandidateSeal, reservation resourcebudget.Permit) (*authenticatedstate.StoreRepack, error) {
	if p == nil || !p.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer p.mu.Unlock()
	ref, err := p.rootReferenceLocked(ctx, true)
	if err != nil {
		return nil, err
	}
	return p.store.PrepareRepackForPhysicalHeadroom(ctx, publisher, guard, selected, ref.Schema, reservation)
}

func (d *AuthenticatedDirectory) PrepareSelectedRepack(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, selected authenticatedstate.CandidateSeal, reservation resourcebudget.Permit) (*authenticatedstate.StoreRepack, error) {
	if d == nil || !d.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer d.mu.Unlock()
	if err := errors.Join(ctx.Err(), d.closeCtx.Err(), d.sealErr); err != nil {
		return nil, err
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentLogDirectory, IndexID: AuthenticatedDirectorySealIndexID, Codec: AuthenticatedDirectorySealCodec}
	return d.store.PrepareRepack(ctx, publisher, guard, selected, schema, reservation)
}
