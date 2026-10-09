package resourcebudget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// PhysicalCandidate owns an actual future publication inode and its already
// created name. The captured name matters: creating a new pending link can need
// a directory block even when all data extents have been prepaid. It grants no
// selection, source, HA role or saved-content authority.
type PhysicalCandidate struct {
	mu                                  sync.Mutex
	file                                *os.File
	owner                               *ownedfile.Owner
	path                                string
	bytes                               int64
	disk, work                          Permit
	manager                             *Manager
	claimed, closed                     bool
	retiring, workRetired, diskReturned bool
	closeErr                            error
	published                           bool
	discarded                           bool
	ready                               bool
	preparationErr                      error
}

// PreparePhysicalCandidate requires the persistent publisher's actual disk
// owner before creating or allocating anything. A partial allocation or unknown
// Close keeps that original owner. A caller must persist its separately
// authenticated birth/name/selection receipt before accepting upstream work.
func PreparePhysicalCandidate(ctx context.Context, path string, bytes int64, manager *Manager, reservation Permit) (candidate *PhysicalCandidate, resultErr error) {
	return PreparePhysicalCandidateObserved(ctx, path, bytes, manager, reservation, nil)
}

// The observer may interrupt preparation, but grants no ownership or recovery
// permission. Every created target retains its actual fee and descriptor.
func PreparePhysicalCandidateObserved(ctx context.Context, path string, bytes int64, manager *Manager, reservation Permit, observe func(string) error) (candidate *PhysicalCandidate, resultErr error) {
	if manager == nil || bytes <= 0 || bytes > 1<<30 {
		return nil, ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	disk, err := reservation.RetainCovering(manager, Cost{Disk: bytes})
	if err != nil {
		return nil, err
	}
	// The slot's control claim outlives any caller loan (RetainIndependentWorkspace
	// keeps it), so it pays ordinary admission even inside a parent workspace.
	work, err := manager.TryAcquireContext(WithoutRAMWorkspace(ctx), Background, Cost{RAM: 4096})
	if err != nil {
		return nil, errors.Join(err, disk.Release())
	}
	c := &PhysicalCandidate{path: absolute, bytes: bytes, disk: disk, work: work, manager: manager}
	f, owner, err := ownedfile.Create(absolute, os.O_RDWR, 0600)
	c.file, c.owner = f, owner
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			c.closeErr = err
			return c, err
		}
		return nil, errors.Join(err, work.Release(), disk.Release())
	}
	candidate = c // every created target and FD retains a real owner on failure
	defer func() { c.preparationErr = resultErr; c.ready = resultErr == nil }()
	if observe != nil {
		if err := observe("physical-target-created"); err != nil {
			return c, err
		}
	}
	if err := AllocatePhysicalRange(f, 0, bytes); err != nil {
		return c, err
	}
	if observe != nil {
		if err := observe("physical-target-allocated"); err != nil {
			return c, err
		}
	}
	if err := CheckPhysicalRange(f, 0, bytes); err != nil {
		return c, err
	}
	if err := f.Sync(); err != nil {
		return c, ownedfile.CloseError(f, err)
	}
	parent, err := os.Open(filepath.Dir(absolute))
	if err != nil {
		return c, err
	}
	err = errors.Join(parent.Sync(), ownedfile.CloseFile(parent))
	return c, err
}

// Claim transfers exactly this already-prepared name and descriptor to the
// actual publisher. A candidate cannot be redirected to another path or shared
// by concurrent consumers. Neither an EOF nor logical Disk credit proves the
// actual target extent; recheck the original range before handing it out.
func (c *PhysicalCandidate) Claim(path string, bytes int64) (*os.File, *ownedfile.Owner, error) {
	return c.ClaimForWrite(path, bytes, 0)
}

func (c *PhysicalCandidate) ClaimForWrite(path string, bytes, writtenBytes int64) (*os.File, *ownedfile.Owner, error) {
	if c == nil {
		return nil, nil, ErrPhysicalAllocation
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	absolute, err := filepath.Abs(path)
	if err != nil || absolute != c.path || bytes <= 0 || bytes > c.bytes || writtenBytes < 0 || writtenBytes > bytes || c.claimed || c.closed || c.retiring || c.closeErr != nil || !c.ready || c.preparationErr != nil {
		return nil, nil, errors.Join(ErrPhysicalAllocation, err, c.closeErr)
	}
	if err := c.owner.VerifyFile(c.file); err != nil {
		return nil, nil, err
	}
	info, err := c.file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > writtenBytes {
		return nil, nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := CheckPhysicalRange(c.file, 0, c.bytes); err != nil {
		return nil, nil, err
	}
	if err := CheckPhysicalTail(c.file, 0, c.bytes); err != nil {
		return nil, nil, err
	}
	if _, err := c.file.Seek(0, 0); err != nil {
		return nil, nil, err
	}
	c.claimed = true
	return c.file, c.owner, nil
}

// CloseFile is the original descriptor's one actual Close. Every caller sees
// the same result, including uncertainty; a second Close never invents exit.
func (c *PhysicalCandidate) CloseFile() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeFileLocked()
}

func (c *PhysicalCandidate) closeFileLocked() error {
	if !c.closed {
		c.closed = true
		c.closeErr = ownedfile.CloseFile(c.file)
	}
	return c.closeErr
}
func (c *PhysicalCandidate) releaseWorkLocked() error {
	if c.workRetired {
		return nil
	}
	if err := c.work.Release(); err != nil {
		return err
	}
	c.workRetired = true
	return nil
}
func (c *PhysicalCandidate) releaseDiskLocked() error {
	if c.diskReturned {
		return nil
	}
	if err := c.disk.Release(); err != nil {
		return err
	}
	c.diskReturned = true
	return nil
}

// Published records the real publisher's successful rename and directory
// barrier. The immutable published target remains covered by the parent's
// persistent reservation; only the joined temporary work may be released.
func (c *PhysicalCandidate) Published(manager *Manager, persistent Permit) error {
	if c == nil {
		return ErrPhysicalAllocation
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr != nil || !c.claimed || !c.closed {
		return errors.Join(ownedfile.ErrRetirement, c.closeErr)
	}
	if c.published {
		return nil
	}
	witness, err := persistent.RetainCovering(manager, Cost{Disk: c.bytes})
	if err != nil || manager != c.manager {
		return errors.Join(ErrPhysicalAllocation, err)
	}
	if err := witness.Release(); err != nil {
		return err
	}
	// ReplaceCaptured updates this exact owner to the real published name.
	if err := c.owner.VerifyName(); err != nil {
		return err
	}
	if err := c.releaseWorkLocked(); err != nil {
		return err
	}
	if err := c.releaseDiskLocked(); err != nil {
		return err
	}
	c.published = true
	return nil
}

// Discard is for a definitely unpublished target after its borrower has joined.
// It refuses a claimed target: only the actual publication path may establish
// whether acceptance/rename occurred. Unknown targets retain their fee/name.
func (c *PhysicalCandidate) Discard() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.owner == nil || errors.Is(c.preparationErr, ownedfile.ErrRetirement) {
		c.mu.Unlock()
		return errors.Join(ownedfile.ErrRetirement, c.preparationErr)
	}
	if c.discarded {
		c.mu.Unlock()
		return nil
	}
	if c.claimed || c.published {
		c.mu.Unlock()
		return ErrPhysicalAllocation
	}
	c.retiring = true
	defer c.mu.Unlock()
	if err := c.closeFileLocked(); err != nil {
		return err
	}
	if c.claimed || c.published {
		return ErrPhysicalAllocation
	}
	if err := c.owner.Retire(); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(c.path))
	if err != nil {
		return err
	}
	if err := errors.Join(parent.Sync(), ownedfile.CloseFile(parent)); err != nil {
		c.closeErr = err
		return err
	}
	if err := c.releaseWorkLocked(); err != nil {
		c.closeErr = err
		return err
	}
	if err := c.releaseDiskLocked(); err != nil {
		c.closeErr = err
		return err
	}
	c.discarded = true
	return nil
}

// ValidatePrepared rechecks the exact captured pending name, actual allocated
// range, Manager and bounded shape without lending its writer descriptor.
func (c *PhysicalCandidate) ValidatePrepared(path string, bytes int64, manager *Manager) error {
	if c == nil {
		return ErrPhysicalAllocation
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	absolute, err := filepath.Abs(path)
	if err != nil || absolute != c.path || manager != c.manager || bytes != c.bytes || !c.ready || c.preparationErr != nil || c.closed || c.claimed {
		return errors.Join(ErrPhysicalAllocation, err, c.closeErr, c.preparationErr)
	}
	if err := c.owner.VerifyFile(c.file); err != nil {
		return err
	}
	info, err := c.file.Stat()
	if err != nil || info.Size() < 0 || info.Size() > c.bytes {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return errors.Join(CheckPhysicalRange(c.file, 0, c.bytes), CheckPhysicalTail(c.file, 0, c.bytes))
}

// PhysicalCandidateReference is an ownership claim, not logical source content.
// A persisted reference must be authenticated under an independent common
// selection before RestorePhysicalCandidate may consume it.
type PhysicalCandidateReference struct {
	File, Parent PhysicalIdentity
	Bytes        int64
}

func (c *PhysicalCandidate) Reference() (reference PhysicalCandidateReference, resultErr error) {
	if c == nil {
		return reference, ErrPhysicalAllocation
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.claimed || c.closed || c.closeErr != nil || c.preparationErr != nil {
		return reference, errors.Join(ErrPhysicalAllocation, c.closeErr, c.preparationErr)
	}
	if err := c.owner.VerifyFile(c.file); err != nil {
		return reference, err
	}
	reference.File, resultErr = CapturePhysicalIdentity(c.file)
	if resultErr != nil {
		return reference, resultErr
	}
	parent, err := os.Open(filepath.Dir(c.path))
	if err != nil {
		return reference, err
	}
	reference.Parent, err = CapturePhysicalIdentity(parent)
	reference.Bytes = c.bytes
	return reference, errors.Join(err, ownedfile.CloseFile(parent))
}

// RestorePhysicalCandidate borrows no new space. The caller must first verify
// the MAC ownership receipt and its independent selected envelope. Exact ext4
// birth identities, original name/parent and every allocated target block are
// checked again. A scanned ordinary header cannot supply this permission.
func RestorePhysicalCandidate(ctx context.Context, path string, expected PhysicalCandidateReference, manager *Manager, reservation Permit) (*PhysicalCandidate, error) {
	return restorePhysicalCandidate(ctx, path, expected, manager, reservation, Permit{})
}

// RestorePhysicalCandidateWithOwnedWorkspace retains the original independent
// target's finite control claim. A scoped callback loan cannot become a
// persistent descriptor owner. Inode, extent and receipt checks remain required.
func RestorePhysicalCandidateWithOwnedWorkspace(ctx context.Context, path string, expected PhysicalCandidateReference, manager *Manager, reservation, work Permit) (*PhysicalCandidate, error) {
	if work.claim == nil {
		return nil, ErrInvalid
	}
	return restorePhysicalCandidate(ctx, path, expected, manager, reservation, work)
}

func retainPhysicalCandidateWorkspace(work Permit, manager *Manager) (Permit, error) {
	if manager == nil || work.claim == nil || work.claim.manager != manager {
		return Permit{}, ErrInvalid
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !work.validLocked() || work.claim.reserved || work.claim.workspace != nil || work.claim.class != Background || work.claim.cost != (Cost{RAM: 4096}) {
		return Permit{}, ErrInvalid
	}
	return work.retainLocked()
}

// RetainIndependentWorkspace keeps the already paid target control claim for
// subsequent uses of its fixed inode slot; it conveys no source authority.
func (c *PhysicalCandidate) RetainIndependentWorkspace(manager *Manager) (Permit, error) {
	if c == nil {
		return Permit{}, ErrInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.manager != manager || c.workRetired || c.closed || !c.ready || c.preparationErr != nil {
		return Permit{}, ErrInvalid
	}
	return retainPhysicalCandidateWorkspace(c.work, manager)
}

func restorePhysicalCandidate(ctx context.Context, path string, expected PhysicalCandidateReference, manager *Manager, reservation, originalWork Permit) (candidate *PhysicalCandidate, resultErr error) {
	if manager == nil || expected.File.Inode == 0 || expected.Parent.Inode == 0 || expected.Bytes <= 0 || expected.Bytes > 1<<30 {
		return nil, ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	disk, err := reservation.RetainCovering(manager, Cost{Disk: expected.Bytes})
	if err != nil {
		return nil, err
	}
	var work Permit
	if originalWork.claim == nil {
		work, err = manager.TryAcquireContext(WithoutRAMWorkspace(ctx), Background, Cost{RAM: 4096})
	} else {
		work, err = retainPhysicalCandidateWorkspace(originalWork, manager)
	}
	if err != nil {
		return nil, errors.Join(err, disk.Release())
	}
	c := &PhysicalCandidate{path: absolute, bytes: expected.Bytes, manager: manager, disk: disk, work: work}
	defer func() {
		if resultErr != nil {
			c.preparationErr = resultErr
		} else {
			c.ready = true
		}
	}()
	file, err := os.OpenFile(absolute, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.Join(err, work.Release(), disk.Release())
	}
	c.file = file
	candidate = c // every acquired actual FD has a cleanup owner
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > c.bytes {
		return c, errors.Join(ownedfile.ErrReplaced, err)
	}
	actual, err := CapturePhysicalIdentity(file)
	if err != nil || actual != expected.File {
		return c, errors.Join(ownedfile.ErrReplaced, err)
	}
	parent, err := os.Open(filepath.Dir(absolute))
	if err != nil {
		return c, err
	}
	parentInfo, statErr := parent.Stat()
	parentID, identityErr := CapturePhysicalIdentity(parent)
	if err := errors.Join(statErr, identityErr, ownedfile.CloseFile(parent)); err != nil || parentID != expected.Parent {
		return c, errors.Join(ownedfile.ErrReplaced, err)
	}
	c.owner, err = ownedfile.RestoreAuthenticated(absolute, file, info, parentInfo)
	if err != nil {
		return c, err
	}
	if err := CheckPhysicalRange(file, 0, c.bytes); err != nil {
		return c, err
	}
	return c, CheckPhysicalTail(file, 0, c.bytes)
}

// ClosePreserving joins an unborrowed target while retaining its persistent
// charge and name for the authenticated receipt owner. A claimed, unpublished
// target has an unresolved child and cannot be converted into a clean Close.
func (c *PhysicalCandidate) ClosePreserving() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimed && !c.published {
		return errors.Join(ownedfile.ErrRetirement, c.closeErr)
	}
	if c.published {
		return nil
	}
	if err := c.closeFileLocked(); err != nil {
		return err
	}
	if c.preparationErr != nil {
		return errors.Join(ownedfile.ErrRetirement, c.preparationErr)
	}
	return c.releaseWorkLocked()
}
