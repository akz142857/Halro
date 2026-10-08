package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// BorrowSelectedReadStore lends actual read-only descriptors while the original
// writer retains its real directory flock. Selection still comes from the
// caller's independently authenticated common root. It performs OpenStore's
// complete pack/root/name/inode checks without reacquiring that same lock.
// Four compound component loans fit the lock's existing eight cleanup slots.
// Close of the actual parent joins every child before releasing its flock.
func (s *Store) BorrowSelectedReadStore(ctx context.Context, o StoreOptions) (child *Store, resultErr error) {
	if s == nil || !o.ReadOnly || o.ExpectedSealRoot == nil || o.ExpectedSealSchema == nil || o.ExpectedRoot == nil {
		return nil, ErrRecovery
	}
	if !s.mu.TryLock() {
		return nil, ErrBusy
	}
	defer s.mu.Unlock()
	if s.closed || s.closing || s.guard == nil || s.readBorrowCount >= 4 || s.readBorrowParent != nil || s.budget != o.Budget || !bytes.Equal(s.key, o.Key) || s.current.content.context != o.Scope || s.ceiling != o.MaxDiskBytes-storeControlDisk || !s.guard.CoversDataDirectory(s.dir) {
		return nil, ErrRecovery
	}
	if err := errors.Join(ctx.Err(), s.fault(), s.checkPack(), s.checkRoot()); err != nil {
		return nil, err
	}
	child, resultErr = openStore(ctx, filepath.Clean(s.dir), o, s)
	if child != nil {
		s.readBorrowChildren[s.readBorrowCount] = child
		s.readBorrowCount++
		// Failed actual descriptor retirement also remains retained by this guard.
		if err := s.guard.RetainCleanup(child.Close); err != nil {
			cleanup := child.Close()
			resultErr = errors.Join(resultErr, err, cleanup)
			if cleanup != nil {
				s.retirementErr = errors.Join(s.retirementErr, ownedfile.ErrRetirement, resultErr)
				s.poison(s.retirementErr)
			} else {
				child = nil
			}
		}
	}
	return child, resultErr
}

// RetainBorrowedReadCleanup extends a registered actual Store loan to its
// component's native FD and jobs. It grants neither deletion nor mutation.
func (s *Store) RetainBorrowedReadCleanup(child *Store, cleanup func() error) error {
	if s == nil || child == nil || child.readBorrowParent != s || cleanup == nil {
		return ErrRecovery
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.guard == nil || child.readBorrowComponentCleanup {
		return ErrClosed
	}
	child.readBorrowCleanup = cleanup
	if err := s.guard.RetainCleanup(child.joinBorrowedReadComponent); err != nil {
		child.readBorrowCleanup = nil
		return err
	}
	child.readBorrowComponentCleanup = true
	return nil
}

// JoinBorrowedReadComponent invokes the previously registered actual compound
// owner, including its native descriptor and callbacks. A Store-only Close
// cannot substitute for this join when handing the selected root to a writer.
func (s *Store) JoinBorrowedReadComponent(child *Store) error {
	if s == nil || child == nil || child.readBorrowParent != s {
		return ErrRecovery
	}
	s.mu.Lock()
	if s.closed || s.closing || !child.readBorrowComponentCleanup {
		s.mu.Unlock()
		return ErrRecovery
	}
	s.mu.Unlock()
	return child.joinBorrowedReadComponent()
}
func (s *Store) joinBorrowedReadComponent() error {
	if s.readBorrowCleanup == nil {
		return ErrRecovery
	}
	err := s.readBorrowCleanup()
	s.mu.Lock()
	if err == nil {
		s.readBorrowComponentJoined = true
	}
	s.mu.Unlock()
	return err
}
