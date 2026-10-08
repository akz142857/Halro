package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Optional extension supplied only by the original finite Standalone source
// loan. HA and regular/native-field callers cannot acquire physical permission.
type AuthenticatedPhysicalUsageSource interface {
	AuthenticatedUsageSource
	PhysicalNativeReference(context.Context) (authenticatedstate.PhysicalNativeReference, error)
	PhysicalNativePreparedEnd(context.Context) (int64, error)
	VerifyPhysicalNativePrefix(context.Context, authenticatedstate.PhysicalNativeReference, int64) error
	PhysicalSelectionDigest(context.Context) ([32]byte, error)
	ValidatePhysicalPublisher(context.Context, *authenticatedstate.SealCandidatePublisher) error
	VerifyPhysicalNoDebt(context.Context) error
}

// VerifyPhysicalNoDebt borrows the original frozen live native source lane;
// a closed startup maintenance lease cannot stand in for this permission.
func (s *authenticatedUsageSource) VerifyPhysicalNoDebt(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheck(ctx); err != nil {
		return err
	}
	o := s.native.physical.Load()
	if s.ref.Anchor != s.head || !o.commonQualified || o.debt != 0 || o.active != 0 {
		return authenticatedstate.ErrRecovery
	}
	p := s.native.startupProvenance
	if err := p.publisher.ValidateOwner(p.guard, s.projection.budget); err != nil {
		return err
	}
	seal, found, err := p.publisher.CurrentContext(ctx)
	if err != nil || !found || seal.Role != authenticatedstate.SealStandalone || seal.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return errors.Join(ErrProjectionSource, err)
	}
	raw, err := json.Marshal(seal)
	if err != nil || sha256.Sum256(raw) != o.selection {
		return errors.Join(ErrProjectionSource, err)
	}
	rootFound, positionFound := false, false
	for _, root := range seal.Roots[:seal.RootCount] {
		rootFound = rootFound || root == s.ref.Root
	}
	for _, position := range seal.Positions[:seal.PositionCount] {
		positionFound = positionFound || position.Component == authenticatedstate.SealComponentLedger && position.Generation == s.head.Head.Generation && position.Sequence == s.head.Head.Sequence && position.Offset == uint64(s.head.Head.Offset) && position.Hash == s.head.Hash && position.Epoch == uint64(s.head.Epoch)
	}
	if !rootFound || !positionFound {
		return authenticatedstate.ErrRecovery
	}
	// The original source holds projection.mu and native.mu and owns this
	// actual pin. Opening another RecoveryView would reenter projection.mu.
	// A complete empty range of each original recovery index proves NoDebt;
	// any row, incomplete page or MAC failure refuses, without decoding it.
	for _, bucket := range []string{"pending", "active_requests"} {
		lo, hi, err := recoveryBounds(bucket, nil)
		if err != nil {
			return err
		}
		page, err := s.pin.Range(ctx, authenticatedstate.RangeOptions{Lo: lo, Hi: hi, MaxRows: 1, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			return s.remember(err)
		}
		if len(page.Rows) != 0 || !page.Done {
			return authenticatedstate.ErrRecovery
		}
	}
	return s.physicalCheck(ctx)
}

// ValidatePhysicalPublisher verifies the original owner identity separately
// from selection renewal. A new common seal may already be durable while the
// original native receipt still retains its old selection until finalization.
func (s *authenticatedUsageSource) ValidatePhysicalPublisher(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheck(ctx); err != nil {
		return err
	}
	p := s.native.startupProvenance
	if publisher == nil || publisher != p.publisher {
		return ErrProjectionSource
	}
	return publisher.ValidateOwner(p.guard, s.projection.budget)
}

func (s *authenticatedUsageSource) physicalCheck(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	n := s.native
	p := s.projection
	o := n.physical.Load()
	if p == nil || o == nil || o.projection != p || p.selectedNative.Load() != n || p.budget != n.startupBudget || n.startupProvenance == nil || n.startupProvenance.ha != nil || n.options.Replica || !n.selectedStartupStructuralAllowedLocked() || o.failure != nil {
		return ErrProjectionSource
	}
	return n.checkPhysicalNativeLocked()
}
func (s *authenticatedUsageSource) PhysicalNativeReference(ctx context.Context) (authenticatedstate.PhysicalNativeReference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheck(ctx); err != nil {
		return authenticatedstate.PhysicalNativeReference{}, err
	}
	return s.native.physicalNativeReferenceLocked()
}
func (s *authenticatedUsageSource) PhysicalNativePreparedEnd(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheck(ctx); err != nil {
		return 0, err
	}
	o := s.native.physical.Load()
	var rangeErr error
	if o.allocatedEnd > s.native.offset {
		rangeErr = resourcebudget.CheckPhysicalRange(s.native.file, s.native.offset, o.allocatedEnd)
	}
	if err := errors.Join(rangeErr, resourcebudget.CheckPhysicalTail(s.native.file, s.native.offset, o.allocatedEnd)); err != nil {
		return 0, err
	}
	return o.allocatedEnd, nil
}
func (s *authenticatedUsageSource) VerifyPhysicalNativePrefix(ctx context.Context, prior authenticatedstate.PhysicalNativeReference, end int64) error {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheck(ctx); err != nil {
		return err
	}
	current, err := s.native.physicalNativeReferenceLocked()
	if err != nil {
		return err
	}
	return directoryVerifyPhysicalPrefixLocked(ctx, s.native, current, prior, end)
}
func (s *authenticatedUsageSource) PhysicalSelectionDigest(ctx context.Context) ([32]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheck(ctx); err != nil {
		return [32]byte{}, err
	}
	p := s.native.startupProvenance
	if err := p.publisher.ValidateOwner(p.guard, s.projection.budget); err != nil {
		return [32]byte{}, err
	}
	seal, found, err := p.publisher.CurrentContext(ctx)
	if err != nil || !found {
		return [32]byte{}, errors.Join(ErrProjectionSource, err)
	}
	raw, err := json.Marshal(seal)
	if err != nil {
		return [32]byte{}, err
	}
	digest := sha256.Sum256(raw)
	if digest != s.native.physical.Load().selection {
		return [32]byte{}, ErrProjectionSource
	}
	return digest, nil
}
