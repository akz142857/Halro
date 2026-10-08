package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// Close may follow the actual native FD or projection FD exit. It rechecks
// the authenticated publisher's current selection without reopening either
// exited owner, using only an already-owned caller workspace when pressured.
func (l *Log) checkPhysicalCloseSelectionLocked(ctx context.Context) error {
	p, o := l.startupProvenance, l.physical.Load()
	if p == nil {
		return nil
	}
	if p.ha != nil {
		return l.checkHAPhysicalCloseSelectionLocked(ctx)
	}
	if o == nil || !o.commonQualified || p.publisher == nil || p.guard == nil {
		return authenticatedstate.ErrRecovery
	}
	if err := p.publisher.ValidateOwner(p.guard, o.projection.budget); err != nil {
		return err
	}
	seal, found, err := p.publisher.CurrentContext(ctx)
	if err != nil || !found || seal.Role != authenticatedstate.SealStandalone || seal.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	raw, err := json.Marshal(seal)
	if err != nil || sha256.Sum256(raw) != o.selection {
		return errors.Join(authenticatedstate.ErrRecovery, err)
	}
	return ctx.Err()
}

// Called with actual native.mu. SealReference uses a nonblocking projection
// read lock and the lock-free original startup grant, so this does not invert
// the ordinary projection→native lock order. Public fingerprints are only
// comparison values; the actual captured publisher/root supplies authority.
func (l *Log) checkPhysicalCommonSelectionLocked(ctx context.Context, selection [32]byte) error {
	p := l.startupProvenance
	if p == nil {
		return nil // compatibility for unselected component test/legacy owners
	}
	o := l.physical.Load()
	if p.ha != nil || o == nil || o.projection.selectedNative.Load() != l || p.publisher == nil || p.guard == nil || !l.selectedStartupStructuralAllowedLocked() {
		return ErrProjectionSource
	}
	if err := p.publisher.ValidateOwner(p.guard, o.projection.budget); err != nil {
		return err
	}
	seal, found, err := p.publisher.CurrentContext(ctx)
	if err != nil || !found || seal.Role != authenticatedstate.SealStandalone || seal.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return errors.Join(ErrProjectionSource, err)
	}
	raw, err := json.Marshal(seal)
	if err != nil || sha256.Sum256(raw) != selection {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	ref, err := o.projection.SealReference(ctx)
	if err != nil || ref.Anchor.Head.Generation != l.generation || ref.Anchor.Head.Sequence != l.chainSequence || ref.Anchor.Head.Offset != l.offset || ref.Anchor.Hash != l.chainHash || ref.Anchor.Epoch != l.chainEpoch {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	rootFound, positionFound := false, false
	for _, root := range seal.Roots[:seal.RootCount] {
		if root == ref.Root {
			rootFound = true
		}
	}
	for _, position := range seal.Positions[:seal.PositionCount] {
		if position.Component == authenticatedstate.SealComponentLedger && position.Generation == l.generation && position.Sequence == l.chainSequence && position.Offset == uint64(l.offset) && position.Hash == l.chainHash && position.Epoch == uint64(l.chainEpoch) {
			positionFound = true
		}
	}
	if !rootFound || !positionFound {
		return authenticatedstate.ErrSealIneligible
	}
	return ctx.Err()
}
