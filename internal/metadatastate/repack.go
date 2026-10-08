package metadatastate

import (
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func (b *Baseline) PrepareSelectedRepack(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, selected authenticatedstate.CandidateSeal, reservation resourcebudget.Permit) (*authenticatedstate.StoreRepack, error) {
	if b == nil || !b.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if b.haPeerTransition != nil {
		return nil, ErrSource
	}
	ref, err := b.sealReferenceLocked(ctx)
	if err != nil {
		return nil, err
	}
	// The exact epoch/trim baseline is also bound in the native header. Repack
	// only a real suffix, whose selected full root supersedes that fixed anchor.
	if b.position.Sequence == b.position.TrimmedThrough {
		return nil, nil
	}
	return b.store.PrepareRepack(ctx, publisher, guard, selected, ref.Schema, reservation)
}
