package authenticatedstate

import (
	"context"
	"errors"
)

// RecordPhysicalNativeSuccessorQualified is consumed only by an actual native
// Roll owner, using its finite private callback to revalidate the captured old
// complete prefix and newly captured successor. Ordinary RecordNative remains
// strict about file birth/generation. This keeps the same COW targets, common
// selection and allocated controls, and never allocates or forgives COW debt.
func (s *Store) RecordPhysicalNativeSuccessorQualified(ctx context.Context, from, to PhysicalNativeReference, end int64, verify func(PhysicalNativeReference) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.physical
	if s.closed || s.closing || p == nil || p.receipt == nil || p.failure != nil || verify == nil || from.File == to.File || from.Parent != to.Parent || from.Generation == ^uint64(0) || from.Generation+1 != to.Generation || from.Sequence != to.Sequence || from.Hash != to.Hash || to.EOF != 0 || end < to.EOF || end > 1<<50 || p.receipt.claims.Native != from {
		return ErrSealIneligible
	}
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot(), s.checkPhysicalReceiptControlsLocked(), verify(p.receipt.claims.Native)); err != nil {
		return err
	}
	for _, slot := range p.slots {
		if slot != nil && slot.prepared {
			return ErrRecovery
		}
	}
	p.receipt.claims.Native, p.receipt.claims.NativeEnd = to, end
	if err := s.savePhysicalReceiptLocked(); err != nil {
		p.failure = err
		s.poison(err)
		return err
	}
	if err := verify(from); err != nil {
		p.failure = err
		s.poison(err)
		return err
	}
	return nil
}
