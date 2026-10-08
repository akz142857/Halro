package ledger

import (
	"context"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// SelectedPhysicalStartupSource is an optional extension of the real finite
// selected Standalone source loan. Physical methods reject HA, incomplete full
// history authentication and expired loans. They grant no new source owner.
type SelectedPhysicalStartupSource interface {
	SelectedStartupSource
	PhysicalNativeReference(context.Context) (authenticatedstate.PhysicalNativeReference, error)
	VerifyPhysicalNativePrefix(context.Context, authenticatedstate.PhysicalNativeReference, int64) error
	PhysicalNativePreparedEnd(context.Context) (int64, error)
}

func (s *selectedStartupSource) physicalCheckLocked(ctx context.Context) error {
	if err := s.checkLocked(ctx); err != nil {
		return err
	}
	if s.seal.Role != authenticatedstate.SealStandalone || !s.native.selectedStartupStructuralAllowedLocked() {
		return ErrProjectionSource
	}
	return nil
}
func (s *selectedStartupSource) PhysicalNativeReference(ctx context.Context) (authenticatedstate.PhysicalNativeReference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheckLocked(ctx); err != nil {
		return authenticatedstate.PhysicalNativeReference{}, err
	}
	return s.native.physicalNativeReferenceLocked()
}
func (s *selectedStartupSource) VerifyPhysicalNativePrefix(ctx context.Context, prior authenticatedstate.PhysicalNativeReference, end int64) error {
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheckLocked(ctx); err != nil {
		return err
	}
	actual, err := s.native.physicalNativeReferenceLocked()
	if err != nil {
		return err
	}
	if prior.File != actual.File || prior.Parent != actual.Parent || prior.Generation != actual.Generation || prior.Sequence > actual.Sequence || prior.EOF < 0 || prior.EOF > actual.EOF || end < actual.EOF {
		return ErrProjectionSource
	}
	verifier := &chainVerifier{key: s.native.chainKey, hash: s.native.chainAnchor, sequence: s.native.startSequence}
	head, partial, err := scan(io.NewSectionReader(s.native.file, 0, prior.EOF), s.native.generation, 0, s.native.startSequence, nil, verifier)
	if err != nil || partial || head.Offset != prior.EOF || head.Sequence != prior.Sequence || verifier.hash != prior.Hash {
		return errors.Join(ErrProjectionSource, err)
	}
	return errors.Join(resourcebudget.CheckPhysicalTail(s.native.file, actual.EOF, end), s.physicalCheckLocked(ctx))
}
func (s *selectedStartupSource) PhysicalNativePreparedEnd(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.physicalCheckLocked(ctx); err != nil {
		return 0, err
	}
	if err := s.native.checkPhysicalNativeLocked(); err != nil {
		return 0, err
	}
	owner := s.native.physical.Load()
	if owner == nil || owner.failure != nil || owner.allocatedEnd < s.native.offset {
		return 0, authenticatedstate.ErrRecovery
	}
	var rangeErr error
	if owner.allocatedEnd > s.native.offset {
		rangeErr = resourcebudget.CheckPhysicalRange(s.native.file, s.native.offset, owner.allocatedEnd)
	}
	if err := errors.Join(rangeErr, resourcebudget.CheckPhysicalTail(s.native.file, s.native.offset, owner.allocatedEnd)); err != nil {
		return 0, err
	}
	return owner.allocatedEnd, s.physicalCheckLocked(ctx)
}
