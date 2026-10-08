package authenticatedstate

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// AdoptClosedSuccessor updates a fresh publisher's deletion receipts after a
// borrowed writer has closed successfully. Both handles must refer to the SAME
// pack, namespace, scope and Manager. It neither authenticates business state nor
// transfers deletion authority to the borrower. Actual Discard still reacquires
// the namespace lock and checks the captured files before retiring disk credit.
func (s *Store) AdoptClosedSuccessor(next *Store) (resultErr error) {
	if s == nil || next == nil || s == next || s.borrowed || !next.borrowed || s.budget != next.budget {
		return ErrLimit
	}
	if !s.mu.TryLock() {
		return ErrBusy
	}
	defer s.mu.Unlock()
	if !next.mu.TryLock() {
		return ErrBusy
	}
	defer next.mu.Unlock()
	if !s.closed || !next.closed || s.closeErr != nil || next.closeErr != nil || s.discarded || next.fault() != nil || s.dir != next.dir || s.ceiling != next.ceiling || s.current.content.context != next.current.content.context || s.current.content.id != next.current.content.id || next.physicalRevision < s.physicalRevision || next.physicalRevision == s.physicalRevision && next.physicalRootDigest != s.physicalRootDigest {
		return ErrRecovery
	}
	credit, e := s.control.RetainCovering(s.budget, resourcebudget.Cost{Disk: s.ceiling + storeControlDisk})
	if e != nil {
		return e
	}
	defer func() { resultErr = errors.Join(resultErr, credit.Release()) }()
	guard, e := lock.AcquireExistingReadOnly(s.dir)
	if e != nil {
		return e
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	dir, e := os.Lstat(s.dir)
	if e != nil || !os.SameFile(s.dirInfo, dir) || !os.SameFile(next.dirInfo, dir) {
		return errors.Join(ownedfile.ErrReplaced, e)
	}
	pack, e := os.Lstat(filepath.Join(s.dir, "pages"))
	if e != nil || !os.SameFile(s.info, pack) || !os.SameFile(next.info, pack) || pack.Size() < int64(next.physicalExtent) || pack.Size() > next.ceiling {
		return errors.Join(ownedfile.ErrReplaced, e)
	}
	lockInfo, e := os.Lstat(filepath.Join(s.dir, ".halro.lock"))
	if e != nil || !os.SameFile(s.lockInfo, lockInfo) {
		return errors.Join(ownedfile.ErrReplaced, e)
	}
	raw, info, e := readFixed(filepath.Join(s.dir, "active.root"), cowRootBytes)
	if e != nil || !os.SameFile(next.rootInfo, info) || sha256.Sum256(raw) != next.physicalRootDigest {
		return errors.Join(ErrCorrupt, e)
	}
	if next.physical != nil && !next.physical.feeRetired && s.physical != nil && !s.physical.feeRetired {
		return ErrRecovery // two outstanding owners cannot be silently merged
	}
	s.current, s.rootInfo, s.rootDigest = next.current, info, next.rootDigest
	s.physicalRootDigest, s.physicalExtent, s.physicalRevision = next.physicalRootDigest, next.physicalExtent, next.physicalRevision
	if next.physical != nil && !next.physical.feeRetired {
		// Both actual descriptors have joined and the same captured namespace,
		// root and Manager were verified above. Retain the original physical
		// owner (including opaque debt) with the actual creator, never drop its
		// fee merely because the borrowed writer exited.
		s.physical, next.physical = next.physical, nil
	}
	return nil
}

// Preflight checks the entire fixed namespace before removing its first file.
// A replaced root or extra file must not leave a foreign owner without its pack.
// Interrupted deletion may leave a subset of the three captured names.
func (s *Store) checkDiscardNamespace() (resultErr error) {
	f, e := os.Open(s.dir)
	if e != nil {
		return e
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	info, e := f.Stat()
	if e != nil || !os.SameFile(s.dirInfo, info) {
		return errors.Join(ownedfile.ErrReplaced, e)
	}
	names, e := f.Readdirnames(4)
	if e != nil && !errors.Is(e, io.EOF) {
		return e
	}
	if len(names) > 3 {
		return ownedfile.ErrReplaced
	}
	for _, expected := range []struct {
		name string
		info os.FileInfo
	}{{"pages", s.info}, {"active.root", s.rootInfo}, {".halro.lock", s.lockInfo}} {
		if expected.info == nil || expected.name == "pages" && s.discardPackRetired {
			continue
		}
		current, err := os.Lstat(filepath.Join(s.dir, expected.name))
		if err != nil || !os.SameFile(expected.info, current) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	for _, name := range names {
		var captured os.FileInfo
		switch name {
		case "pages":
			captured = s.info
		case "active.root":
			captured = s.rootInfo
		case ".halro.lock":
			captured = s.lockInfo
		default:
			return ownedfile.ErrReplaced
		}
		current, e := os.Lstat(filepath.Join(s.dir, name))
		if e != nil || captured == nil || !os.SameFile(captured, current) {
			return errors.Join(ownedfile.ErrReplaced, e)
		}
		if name == "active.root" {
			raw, _, e := readFixed(filepath.Join(s.dir, name), cowRootBytes)
			if e != nil || sha256.Sum256(raw) != s.physicalRootDigest {
				return errors.Join(ErrCorrupt, e)
			}
		}
	}
	return nil
}
