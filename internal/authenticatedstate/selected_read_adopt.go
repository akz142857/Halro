package authenticatedstate

import (
	"context"
	"errors"
)

// AdoptClosedSelectedReadRoot resumes an independently selected logical root
// on its actual original writer. This is a descriptor handoff, not a physical
// rollback or a grant to append native facts. Every registered child and pin
// must already be joined. The real flock, physical header, extent and revision
// remain owned by the original writer; its next update appends after the full
// physical extent with a strictly newer physical revision.
func (s *Store) AdoptClosedSelectedReadRoot(ctx context.Context, child *Store, selected SealRoot, schema SealRootSchema, cut [32]byte) error {
	if s == nil || child == nil || s == child || child.readBorrowParent != s {
		return ErrRecovery
	}
	if !s.mu.TryLock() {
		return ErrBusy
	}
	defer s.mu.Unlock()
	if s.closed || s.closing || s.readOnly || s.guard == nil || s.readBorrowParent != nil || !s.guard.CoversDataDirectory(s.dir) {
		return ErrRecovery
	}
	if err := errors.Join(ctx.Err(), s.fault(), s.checkPack(), s.checkRoot()); err != nil {
		return err
	}
	for i := range s.pins {
		if s.pins[i].Load() != nil {
			return ErrBusy
		}
	}
	found := false
	for i := uint8(0); i < s.readBorrowCount; i++ {
		loan := s.readBorrowChildren[i]
		if loan == nil {
			return ErrRecovery
		}
		loan.mu.Lock()
		joined := loan.closed && loan.pack == nil && loan.closeErr == nil && loan.retirementErr == nil && loan.fault() == nil

		if loan.readBorrowComponentCleanup && !loan.readBorrowComponentJoined {
			joined = false
		}
		for i := range loan.pins {
			if loan.pins[i].Load() != nil {
				joined = false
			}
		}
		if loan == child {
			found = true
		}
		loan.mu.Unlock()
		if !joined {
			return ErrBusy
		}
	}
	if !found {
		return ErrRecovery
	}
	child.mu.Lock()
	defer child.mu.Unlock()
	if !child.readOnly || child.info == nil || child.physicalRootDigest != s.physicalRootDigest || child.physicalExtent != s.physicalExtent || child.physicalRevision != s.physicalRevision || child.packSlot != s.packSlot || child.packDigest != s.packDigest || child.dir != s.dir || child.budget != s.budget {
		return ErrRecovery
	}
	candidate, digest, err := selectedCOWRoot(selected, schema, s.key, s.current.content.context, cut)
	if err != nil {
		return err
	}
	if candidate != child.current || digest != child.rootDigest || candidate.content.id != s.current.content.id || candidate.revision > s.physicalRevision || candidate.content.extent > s.physicalExtent {
		return ErrRecovery
	}
	s.current, s.rootDigest = candidate, digest
	return ctx.Err()
}
