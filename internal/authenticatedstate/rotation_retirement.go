package authenticatedstate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type RotationRootRetirementFailure struct {
	source, successor *Store
	reader            *os.File
	work, disk        resourcebudget.Permit
	cause             error
}

func (e *RotationRootRetirementFailure) Error() string { return e.cause.Error() }
func (e *RotationRootRetirementFailure) Unwrap() error { return e.cause }

// CaptureRotationRetirementLock must run while the actual source lock is live.
// Reopened borrowed stores otherwise have no permission to unlink that lock.
func (s *Store) CaptureRotationRetirementLock() error {
	if s == nil {
		return ErrRecovery
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.guard == nil || s.fault() != nil {
		return ErrRecovery
	}
	actual, err := s.guard.DescriptorReceipt()
	if err != nil {
		return err
	}
	s.lockInfo = actual
	return nil
}

// RetireClosedRotationRoot grants only an actual superseded Metadata pack
// owner retirement. It leaves native journals to their separate archive owner.
// Both roots must originate in this same Manager and actual successor owner;
// a caller seal or a borrowed Discard alone grants no deletion permission.
func (s *Store) RetireClosedRotationRoot(ctx context.Context, successor *Store, previous SealRoot, publisher *SealCandidatePublisher, guard *lock.Lock, authority SealEligibility, reservation resourcebudget.Permit) (resultErr error) {
	return s.retireClosedRotationRoot(ctx, successor, previous, publisher, guard, authority, reservation, func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	})
}

// Tests alter only the newly opened actual retirement reader descriptor.
func (s *Store) retireClosedRotationRoot(ctx context.Context, successor *Store, previous SealRoot, publisher *SealCandidatePublisher, guard *lock.Lock, authority SealEligibility, reservation resourcebudget.Permit, open func(string) (*os.File, error)) (resultErr error) {
	if s == nil || successor == nil || s == successor || publisher == nil || guard == nil || s.budget != successor.budget || previous.Component != SealComponentMetadata || previous.Kind != SealRootState {
		return ErrSealIneligible
	}
	if err := publisher.ValidateOwner(guard, s.budget); err != nil {
		return err
	}
	successor.mu.Lock()
	defer successor.mu.Unlock()
	if successor.closed || successor.closing || successor.fault() != nil {
		return ErrRecovery
	}
	if err := errors.Join(successor.checkPack(), successor.checkRoot()); err != nil {
		return err
	}
	schema := SealRootSchema{Kind: previous.Kind, Component: previous.Component, IndexID: previous.IndexID, Codec: previous.Codec}
	next := sealRootFor(successor.current, successor.rootDigest, schema)
	if next == previous || next.State.Scope != previous.State.Scope {
		return fmt.Errorf("old/new scopes differ or root equal: %w", ErrSealIneligible)
	}
	selected, found, err := publisher.Select(authority)
	if err != nil || !found || !selectedContains(selected, next) || selectedContains(selected, previous) {
		return fmt.Errorf("actual successor not selected: %w (next selected=%v old selected=%v)", errors.Join(ErrSealIneligible, err), selectedContains(selected, next), selectedContains(selected, previous))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed || s.closeErr != nil || s.pack != nil || s.retirementErr != nil || s.discarded || sealRootFor(s.current, s.rootDigest, schema) != previous {
		return errors.Join(ErrRecovery, s.closeErr, s.retirementErr)
	}
	if s.repack != nil && s.repack.old.file != nil {
		return ErrRecovery
	}
	relative, err := filepath.Rel(publisher.dir, s.dir)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == "../" {
		return ErrSealIneligible
	}
	credit, err := reservation.RetainCovering(s.budget, resourcebudget.Cost{Disk: s.ceiling + storeControlDisk})
	if err != nil {
		return err
	}
	work, err := s.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 10})
	if err != nil {
		return errors.Join(err, credit.Release())
	}
	var reader *os.File
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				s.retirementErr = errors.Join(s.retirementErr, resultErr)
				s.closeErr = s.retirementErr
			}
			resultErr = &RotationRootRetirementFailure{source: s, successor: successor, reader: reader, work: work, disk: credit, cause: resultErr}
			return
		}
		resultErr = errors.Join(work.Release(), credit.Release())
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(directory, s.dirInfo) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	expected := []struct {
		name string
		info *os.FileInfo
	}{{s.slotPath(s.packSlot, "pages"), &s.info}, {s.slotPath(s.packSlot, "active.root"), &s.rootInfo}, {filepath.Join(s.dir, ".halro.lock"), &s.lockInfo}}
	for index, entry := range expected {
		current, err := os.Lstat(entry.name)
		if err != nil || *entry.info == nil || !os.SameFile(current, *entry.info) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		if index == 2 {
			continue
		}
		f, err := open(entry.name)
		if err != nil {
			return err
		}
		reader = f
		actual, readErr := f.Stat()
		if readErr == nil && !os.SameFile(actual, *entry.info) {
			readErr = ownedfile.ErrReplaced
		}
		size := headerBytes
		if index == 1 {
			size = cowRootBytes
		}
		raw := make([]byte, size)
		if readErr == nil {
			_, readErr = io.ReadFull(f, raw)
		}
		wanted := s.packDigest
		if index == 1 {
			wanted = s.physicalRootDigest
		}
		if readErr == nil && sha256.Sum256(raw) != wanted {
			readErr = ErrCorrupt
		}
		clear(raw)
		closeErr := ownedfile.CloseFile(f)
		if closeErr != nil {
			return errors.Join(readErr, closeErr)
		}
		reader = nil
		if readErr != nil {
			return readErr
		}
	}
	f, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	reader = f
	actual, readErr := f.Stat()
	if readErr == nil && !os.SameFile(actual, s.dirInfo) {
		readErr = ownedfile.ErrReplaced
	}
	names, scanErr := f.Readdirnames(4)
	if scanErr != nil && !errors.Is(scanErr, io.EOF) {
		readErr = errors.Join(readErr, scanErr)
	}
	if len(names) != 3 {
		readErr = errors.Join(readErr, ownedfile.ErrReplaced)
	}
	for _, name := range names {
		known := false
		for _, entry := range expected {
			if name == filepath.Base(entry.name) {
				known = true
			}
		}
		if !known {
			readErr = errors.Join(readErr, ownedfile.ErrReplaced)
		}
	}
	closeErr := ownedfile.CloseFile(f)
	if closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	reader = nil
	if readErr != nil {
		return readErr
	}
	// Foreign or incomplete namespaces have been rejected before the first unlink.
	for _, entry := range expected {
		if err := retireCaptured(entry.name, entry.info); err != nil {
			return err
		}
	}
	if err := os.Remove(s.dir); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(s.dir)); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	if err := s.discardPhysicalFeeLocked(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	s.discarded = true
	return nil
}
