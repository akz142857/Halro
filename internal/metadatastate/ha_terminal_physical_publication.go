package metadatastate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// RetireHAPhysicalPreparationAfterCommon is a joined component step after the
// whole-vector maintenance caller proved actual Ledger NoDebt and published
// the actual HA seven-root cut. This owner independently selects that cut and
// authenticates all its native bytes before retiring only unused originals.
func (b *Baseline) RetireHAPhysicalPreparationAfterCommon(ctx context.Context) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.physicalEnabled {
		return nil
	}
	s := b.haPhysicalSource
	if s == nil || !b.ready.Load() {
		return ErrSource
	}
	w := s.receiveWindow
	if w == nil {
		w = b.physicalRecovery
	}
	if b.physicalRecovery != nil && s.role == authenticatedstate.SealPrimary && !b.physicalPinRecoveryVerified {
		return authenticatedstate.ErrRecovery
	}
	var succeeded bool
	visit := func(ctx context.Context) error {
		actual, err := b.sealReferenceLocked(ctx)
		if err != nil {
			return err
		}
		return b.withHAPhysicalAuthorityLocked(ctx, func(ctx context.Context, seal authenticatedstate.CandidateSeal, q *metadatajournal.PhysicalNativeLoan) error {
			if !metadataPhysicalExactCommon(seal, actual) {
				return ErrSource
			}
			ref, err := q.Reference()
			if err != nil {
				return err
			}
			if ref.Sequence != b.position.Sequence || ref.Hash != b.position.Hash || ref.EOF != b.position.Offset {
				return ErrSource
			}
			if w != nil {
				if err := w.cow.Close(); err != nil {
					return err
				}
				b.physicalDebt -= w.remaining
				w.remaining = 0
			}
			if b.physicalDebt != 0 {
				return authenticatedstate.ErrRecovery
			}
			if err := b.store.RetirePhysicalPreparationKeepingControls(ctx); err != nil {
				return err
			}
			if err := q.RetireUnused(); err != nil {
				return err
			}
			succeeded = true
			return nil
		})
	}
	var err error
	if w != nil {
		err = resourcebudget.WithCompletionWorkspace(w.completion.Bind(w.Bind(ctx)), visit)
	} else {
		err = visit(ctx)
	}
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ownedfile.ErrReplaced) || errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
			b.retirementErr = errors.Join(b.retirementErr, err)
			b.ready.Store(false)
		}
		return err
	}
	if !succeeded {
		return ErrSource
	}
	if w != nil {
		if err := w.closeLocked(); err != nil {
			return err
		}
	}
	if s.receiveWindow == w {
		s.receiveWindow = nil
	}
	if b.physicalRecovery == w {
		b.physicalRecovery = nil
	}
	return nil
}

func metadataPhysicalExactCommon(seal authenticatedstate.CandidateSeal, actual AuthenticatedMetadataSealReference) bool {
	rootFound, positionFound := false, false
	for _, r := range seal.Roots[:seal.RootCount] {
		if r == actual.Root {
			rootFound = true
		}
	}
	for _, p := range seal.Positions[:seal.PositionCount] {
		if p == actual.Position {
			positionFound = true
		}
	}
	return rootFound && positionFound
}

// BindHAPhysicalCommonSelection requires the original HA constructor's current
// independent selection again; a supplied digest or candidate is never a grant.
func (b *Baseline) BindHAPhysicalCommonSelection(ctx context.Context) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.physicalEnabled {
		return nil
	}
	if b.haPhysicalSource == nil || b.physicalDebt != 0 || !b.ready.Load() {
		return ErrSource
	}
	actual, err := b.sealReferenceLocked(ctx)
	if err != nil {
		return err
	}
	return b.withHAPhysicalAuthorityLocked(ctx, func(ctx context.Context, seal authenticatedstate.CandidateSeal, q *metadatajournal.PhysicalNativeLoan) error {
		if !metadataPhysicalExactCommon(seal, actual) {
			return ErrSource
		}
		ref, err := q.Reference()
		if err != nil {
			return err
		}
		if ref.Sequence != b.position.Sequence || ref.Hash != b.position.Hash || ref.EOF != b.position.Offset {
			return ErrSource
		}
		raw, err := json.Marshal(seal)
		if err != nil {
			return err
		}
		return b.store.BindPhysicalSelectionDigest(sha256.Sum256(raw), ref)
	})
}
