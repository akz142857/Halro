package authenticatedstate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// RootIdentity is a fixed commitment receipt, not a resume or role permission.
// It describes the selected MAC-validated root, including its complete row count.
type RootIdentity struct {
	Scope, Cut, Digest [32]byte
	ID                 [16]byte
	Revision, Rows     uint64
}

func (s *Store) RootIdentity(ctx context.Context) (result RootIdentity, resultErr error) {
	if !s.mu.TryLock() {
		return result, ErrBusy
	}
	defer s.mu.Unlock()
	if s.closing || s.closed {
		return result, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := s.fault(); err != nil {
		return result, err
	}
	permit, err := s.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, permit.Release())
		if resultErr != nil {
			result = RootIdentity{}
		}
	}()
	if err := errors.Join(s.checkPack(), s.checkRoot()); err != nil {
		s.poison(err)
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return RootIdentity{Scope: s.current.content.context, Cut: s.current.cut, Digest: s.rootDigest, ID: s.current.content.id, Revision: s.current.revision, Rows: s.current.content.count}, nil
}
