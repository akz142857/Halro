package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// The original pack is slot zero. Each reused slot has an authenticated lower
// revision bound; disjoint revision intervals let a complete selected common
// seal locate its physical pack even after a crash between component and common
// publication. PackID remains the native baseline lineage, not a filename hint.
func (s *Store) slotPath(slot uint8, name string) string {
	if slot == 0 {
		return filepath.Join(s.dir, name)
	}
	switch name {
	case "pages":
		name = "pages.next"
	case "active.root":
		name = "active.next.root"
	case "root.pending":
		name = "root.next.pending"
	case "origin.root":
		name = "origin.next.root"
	case "repack.intent":
		name = "repack.next.intent"
	}
	return filepath.Join(s.dir, name)
}

func (s *Store) selectPackSlot(o StoreOptions) error {
	if o.ExpectedSealRoot == nil && o.ExpectedRoot == nil {
		if _, err := os.Lstat(s.slotPath(1, "active.root")); !errors.Is(err, os.ErrNotExist) {
			return ErrRecovery
		}
		if _, err := os.Lstat(s.slotPath(0, "origin.root")); !errors.Is(err, os.ErrNotExist) {
			return ErrRecovery
		}
		return nil
	}
	var revision uint64
	var id [16]byte
	if o.ExpectedSealRoot != nil {
		selected, _, err := selectedCOWRoot(*o.ExpectedSealRoot, *o.ExpectedSealSchema, o.Key, o.Scope, o.ExpectedCut)
		if err != nil {
			return err
		}
		revision, id = selected.revision, selected.content.id
	} else {
		revision, id = o.ExpectedRoot.Revision, o.ExpectedRoot.ID
	}
	found := false
	var refusal error
	for slot := uint8(0); slot < 2; slot++ {
		raw, _, err := readFixed(s.slotPath(slot, "active.root"), cowRootBytes)
		if err != nil {
			refusal = errors.Join(refusal, err)
			continue
		}
		var cut [32]byte
		copy(cut[:], raw[8:40])
		physical, err := decodeCOWRoot(raw, o.Key, o.Scope, cut)
		if err != nil {
			refusal = errors.Join(refusal, err)
			continue
		}
		minimum := uint64(1)
		origin, _, err := readFixed(s.slotPath(slot, "origin.root"), cowRootBytes)
		if err == nil {
			copy(cut[:], origin[8:40])
			first, err := decodeCOWRoot(origin, o.Key, o.Scope, cut)
			if err != nil || first.content.id != physical.content.id || first.revision > physical.revision {
				refusal = errors.Join(refusal, ErrRecovery, err)
				continue
			}
			minimum = first.revision
		} else if slot != 0 || !errors.Is(err, os.ErrNotExist) {
			refusal = errors.Join(refusal, ErrRecovery, err)
			continue
		}
		if physical.content.id != id || revision < minimum || revision > physical.revision {
			continue
		}
		if found {
			return ErrRecovery
		}
		s.packSlot, found = slot, true
	}
	if !found {
		return errors.Join(ErrRecovery, refusal)
	}
	return nil
}

// RepackDiskBytes is the extra fixed reservation covering the second complete
// pack and both slots' additional control files. It is a logical file ceiling;
// allocating it does not reserve filesystem blocks.
func (s *Store) RepackDiskBytes() int64 {
	bytes, _ := RepackReservationBytes(s.ceiling)
	return bytes
}

type repackState struct {
	slot                                   uint8
	file                                   *os.File
	info, rootInfo                         os.FileInfo
	root                                   cowRoot
	rootDigest, packDigest, physicalDigest [32]byte
	extent, revision                       uint64
	controls                               [5]os.FileInfo
	physicalClaims                         physicalReceiptClaims
	physicalAllocatedEnd                   int64
	physicalReceipt                        bool
}

// StoreRepack retains both actual descriptors until common selection is known.
// It never grants native/role authority. Commit checks the actual publisher;
// Rollback requires that the original common selection is still active.
type StoreRepack struct {
	store                             *Store
	publisher                         *SealCandidatePublisher
	guard                             *lock.Lock
	before                            CandidateSeal
	schema                            SealRootSchema
	old                               repackState
	packOwner, rootOwner, originOwner *ownedfile.Owner
	intentOwner                       *ownedfile.Owner
	disk, work                        resourcebudget.Permit
	done                              bool
	err                               error
	physicalIntent                    bool
}

func selectedContains(seal CandidateSeal, root SealRoot) bool {
	for i := uint8(0); i < seal.RootCount; i++ {
		if seal.Roots[i] == root {
			return true
		}
	}
	return false
}

func writeRepackControl(path string, raw []byte) (owner *ownedfile.Owner, info os.FileInfo, resultErr error) {
	f, owner, err := ownedfile.Create(path, os.O_RDWR, 0600)
	if err != nil {
		return owner, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, ownedfile.CloseFile(f)) }()
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		return owner, nil, errors.Join(err, io.ErrShortWrite)
	}
	if err := f.Sync(); err != nil {
		return owner, nil, err
	}
	info, err = f.Stat()
	return owner, info, err
}

// A fully authenticated inactive interval may be removed only after an actual
// common publication selected the other complete root. Unknown partial files
// are preserved. Each unlink still checks the captured original named inode.
func (s *Store) retireInactivePack(ctx context.Context, slot uint8) (resultErr error) {
	paths := []string{"pages", "active.root", "origin.root", "root.pending"}
	var captured [4]os.FileInfo
	for i, name := range paths {
		info, err := os.Lstat(s.slotPath(slot, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.Join(ErrRecovery, err)
		}
		captured[i] = info
	}
	intent, intentInfo, err := s.readRepackIntent(slot, captured[0])
	if errors.Is(err, ownedfile.ErrRetirement) {
		return err
	}
	if captured[1] == nil && captured[2] == nil && captured[3] == nil && (err != nil || intent == nil) {
		// Only the pre-copy shape can be preserved without authenticating it.
		// A foreign/large/control-bearing unknown candidate still refuses.
		intentInfo, statErr := os.Lstat(s.slotPath(slot, "repack.intent"))
		if errors.Is(statErr, os.ErrNotExist) {
			intentInfo = nil
		} else if statErr != nil {
			return statErr
		}
		if captured[0] != nil || intentInfo != nil {
			return s.preserveUnqualifiedRepack(ctx, slot, captured[0], intentInfo)
		}
	}
	if err != nil {
		return err
	}
	if intent != nil {
		retired, err := s.retireInterruptedRepack(ctx, slot, intent, intentInfo, captured)
		if err != nil || retired {
			return err
		}
	}
	if captured[0] == nil && captured[1] == nil && captured[2] == nil && captured[3] == nil {
		return nil
	}
	if captured[1] == nil {
		return ErrRecovery
	}
	raw, info, err := readFixed(s.slotPath(slot, "active.root"), cowRootBytes)
	if err != nil || !os.SameFile(info, captured[1]) {
		return errors.Join(ErrRecovery, err)
	}
	var cut [32]byte
	copy(cut[:], raw[8:40])
	r, err := decodeCOWRoot(raw, s.key, s.current.content.context, cut)
	orphanSuccessor := r.revision == s.physicalRevision+1 && r.cut == s.current.cut && r.content.count == s.current.content.count && captured[0] != nil && captured[2] != nil
	if err != nil || r.content.id != s.current.content.id || r.revision >= s.current.revision && !orphanSuccessor {
		return errors.Join(ErrRecovery, err)
	}
	if captured[2] != nil {
		origin, originInfo, err := readFixed(s.slotPath(slot, "origin.root"), cowRootBytes)
		if err != nil || !os.SameFile(originInfo, captured[2]) {
			return errors.Join(ErrRecovery, err)
		}
		copy(cut[:], origin[8:40])
		first, err := decodeCOWRoot(origin, s.key, s.current.content.context, cut)
		if err != nil || first.content.id != r.content.id || first.revision > r.revision {
			return errors.Join(ErrRecovery, err)
		}
		if orphanSuccessor && first != r {
			return ErrRecovery
		}
	} else if slot != 0 && captured[0] != nil {
		return ErrRecovery
	}
	if captured[3] != nil {
		pending, pendingInfo, err := readFixed(s.slotPath(slot, "root.pending"), cowRootBytes)
		if err != nil || !os.SameFile(pendingInfo, captured[3]) {
			return errors.Join(ErrRecovery, err)
		}
		copy(cut[:], pending[8:40])
		candidate, err := decodeCOWRoot(pending, s.key, s.current.content.context, cut)
		if err != nil || candidate.content.id != r.content.id || candidate.revision != r.revision+1 || candidate.revision >= s.current.revision {
			return errors.Join(ErrRecovery, err)
		}
	}
	if captured[0] == nil {
		// An older authenticated control interval may survive the first unlink
		// of a previously eligible reclamation. No data file is fabricated or
		// adopted; the complete selected successor is already held by this job.
		for _, i := range [4]int{0, 2, 3, 1} {
			if err := removeCaptured(s.slotPath(slot, paths[i]), captured[i]); err != nil {
				return err
			}
		}
		if err := removeCaptured(s.slotPath(slot, "repack.intent"), intentInfo); err != nil {
			return err
		}
		return durable.SyncDirectory(s.dir)
	}
	f, err := os.OpenFile(s.slotPath(slot, "pages"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	actual, err := f.Stat()
	var header [headerBytes]byte
	if err == nil {
		_, err = f.ReadAt(header[:], 0)
	}
	initial, decodeErr := decodeCommitment(header[:], s.key, s.current.content.context)
	if err == nil && (decodeErr != nil || !os.SameFile(captured[0], actual) || initial.id != r.content.id || actual.Size() < int64(r.content.extent) || actual.Size() > s.ceiling) {
		err = ErrRecovery
	}
	if err == nil {
		q := &query{ctx: ctx, snapshot: &Snapshot{file: f, current: r.content}, maxReads: math.MaxInt}
		var compare func([]byte, []byte) error
		if orphanSuccessor {
			compare = func(key, value []byte) error {
				actual, found, err := lookupRepackRow(&query{ctx: ctx, snapshot: &Snapshot{file: s.pack, current: s.current.content}, maxReads: math.MaxInt}, key)
				if err != nil || !found || !bytes.Equal(actual, value) {
					return errors.Join(ErrRecovery, err)
				}
				return nil
			}
		}
		err = verifyRepackTree(q, descriptor{count: r.content.count, height: r.content.height, ref: r.content.root}, compare)
	}
	if closeErr := ownedfile.CloseFile(f); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, i := range [4]int{0, 2, 3, 1} {
		if err := removeCaptured(s.slotPath(slot, paths[i]), captured[i]); err != nil {
			return err
		}
	}
	if err := removeCaptured(s.slotPath(slot, "repack.intent"), intentInfo); err != nil {
		return err
	}
	return durable.SyncDirectory(s.dir)
}

func verifyRepackTree(q *query, d descriptor, compare func([]byte, []byte) error) error {
	if d.count == 0 {
		return nil
	}
	n, err := q.load(d)
	if err != nil {
		return err
	}
	if n.height == 1 {
		value, err := q.value(n.value)
		if err == nil && compare != nil {
			err = compare(n.key, value)
		}
		return err
	}
	for _, child := range n.children {
		if err := verifyRepackTree(q, child, compare); err != nil {
			return err
		}
	}
	return nil
}

func lookupRepackRow(q *query, key []byte) ([]byte, bool, error) {
	content := q.snapshot.current
	if content.count == 0 {
		return nil, false, nil
	}
	d := descriptor{count: content.count, height: content.height, ref: content.root}
	for {
		n, err := q.load(d)
		if err != nil {
			return nil, false, err
		}
		if n.height == 1 {
			if !bytes.Equal(n.key, key) {
				return nil, false, nil
			}
			value, err := q.value(n.value)
			return value, true, err
		}
		found := false
		for _, child := range n.children {
			if bytes.Compare(key, child.first) >= 0 && bytes.Compare(key, child.last) <= 0 {
				d, found = child, true
				break
			}
		}
		if !found {
			return nil, false, nil
		}
	}
}

// PrepareRepack verifies and copies only reachable nodes/blobs into the inactive
// fixed slot. RAM is bounded by tree height and one value, independent of rows.
// The complete freshly selected root must match before old-slot reclamation.
// Business owners must stay paused through Commit/Rollback and common Publish.
func (s *Store) PrepareRepack(ctx context.Context, publisher *SealCandidatePublisher, guard *lock.Lock, selected CandidateSeal, schema SealRootSchema, reservation resourcebudget.Permit) (*StoreRepack, error) {
	return s.prepareRepack(ctx, publisher, guard, selected, schema, reservation, false)
}

// PrepareRepackForPhysicalHeadroom additionally permits an earlier copy when
// the original physical owner's observed terminal demand plus one ordinary
// update cannot fit its unchanged ceiling. The caller holds the same selected
// maintenance lane and complete NoDebt publication as PrepareRepack.
func (s *Store) PrepareRepackForPhysicalHeadroom(ctx context.Context, publisher *SealCandidatePublisher, guard *lock.Lock, selected CandidateSeal, schema SealRootSchema, reservation resourcebudget.Permit) (*StoreRepack, error) {
	return s.prepareRepack(ctx, publisher, guard, selected, schema, reservation, true)
}

func (s *Store) prepareRepack(ctx context.Context, publisher *SealCandidatePublisher, guard *lock.Lock, selected CandidateSeal, schema SealRootSchema, reservation resourcebudget.Permit, physicalHeadroom bool) (result *StoreRepack, resultErr error) {
	if s == nil || !s.mu.TryLock() {
		return nil, ErrBusy
	}
	defer s.mu.Unlock()
	if s.closed || s.closing || s.readOnly || !s.borrowed || s.repack != nil || s.physicalRevision == math.MaxUint64 || s.physical != nil && s.physical.debt != 0 {
		return nil, ErrRecovery
	}
	if err := errors.Join(ctx.Err(), s.fault(), s.checkPack(), s.checkRoot()); err != nil {
		return nil, err
	}
	if s.physicalExtent < uint64(s.ceiling/2) && (!physicalHeadroom || !s.physicalHeadroomExhaustedLocked()) {
		return nil, nil
	}
	for i := range s.pins {
		if s.pins[i].Load() != nil {
			return nil, ErrBusy
		}
	}
	if err := publisher.ValidateOwner(guard, s.budget); err != nil {
		return nil, err
	}
	actual, found, err := publisher.CurrentContext(ctx)
	if err != nil || !found || actual != selected || !selectedContains(selected, sealRootFor(s.current, s.rootDigest, schema)) {
		return nil, errors.Join(ErrSealIneligible, err)
	}
	// No captured allocation is retired until the actual publisher independently
	// selects this exact current root. An early demand signal grants no authority.
	if err := s.retirePhysicalPreparationKeepingControlsLocked(); err != nil {
		return nil, err
	}
	reservationBytes, err := RepackReservationBytes(s.ceiling)
	if err != nil {
		return nil, err
	}
	disk, err := reservation.RetainCovering(s.budget, resourcebudget.Cost{Disk: reservationBytes})
	if err != nil {
		return nil, err
	}
	work, err := s.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: BuildRAMBytes})
	if err != nil {
		return nil, errors.Join(err, disk.Release())
	}
	c := &StoreRepack{store: s, publisher: publisher, guard: guard, before: selected, schema: schema, disk: disk, work: work}
	var file *os.File
	installed := false
	defer func() {
		if installed {
			return
		}
		closeErr := ownedfile.CloseFile(file)
		resultErr = errors.Join(resultErr, closeErr)
		if c.physicalIntent && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, c.rollbackPhysicalReceiptLocked())
		}
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			for _, owner := range []*ownedfile.Owner{c.rootOwner, c.originOwner, c.packOwner, c.intentOwner} {
				if owner != nil {
					resultErr = errors.Join(resultErr, owner.Retire())
				}
			}
			resultErr = errors.Join(resultErr, durable.SyncDirectory(s.dir))
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			c.err, s.retirementErr, s.repack = resultErr, resultErr, c
			s.poison(resultErr)
			result = c
		} else {
			resultErr = errors.Join(resultErr, work.Release(), disk.Release())
		}
	}()
	slot := 1 - s.packSlot
	if err := s.retireInactivePack(ctx, slot); err != nil {
		return nil, err
	}
	if s.physical != nil && s.physical.receipt != nil && s.physical.receipt.claims.RepackTargetPack.Inode != 0 {
		// The independent source remained selected and the actual inactive
		// candidate has joined retirement above. Supersede its obsolete To.
		clearPhysicalRepack(&s.physical.receipt.claims)
		if err := s.savePhysicalReceiptLocked(); err != nil {
			return nil, err
		}
	}
	file, c.packOwner, err = ownedfile.Create(s.slotPath(slot, "pages"), os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := s.phase("repack-pages-created"); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	c.intentOwner, err = s.createRepackIntent(slot, c.packOwner.Identity())
	if err != nil {
		return nil, err
	}
	if err := s.phase("repack-intent-synced"); err != nil {
		return nil, err
	}
	if _, err := file.Write(make([]byte, headerBytes)); err != nil {
		return nil, err
	}
	content := s.current.content
	content.extent = headerBytes
	write := func(raw []byte) (ref, error) {
		if err := ctx.Err(); err != nil {
			return ref{}, err
		}
		if uint64(len(raw)) > uint64(s.ceiling)-content.extent {
			return ref{}, ErrLimit
		}
		r := ref{offset: content.extent, length: uint32(len(raw)), digest: sha256.Sum256(raw)}
		n, err := file.Write(raw)
		if err != nil || n != len(raw) {
			return ref{}, errors.Join(err, io.ErrShortWrite)
		}
		content.extent += uint64(n)
		return r, nil
	}
	q := &query{ctx: ctx, snapshot: &Snapshot{file: s.pack, current: s.current.content}, maxReads: math.MaxInt}
	var copyTree func(descriptor) (descriptor, error)
	copyTree = func(d descriptor) (descriptor, error) {
		n, err := q.load(d)
		if err != nil {
			return descriptor{}, err
		}
		if n.height == 1 {
			raw, err := q.read(n.value, MaxValueBytes+1)
			if err != nil || len(raw) == 0 || raw[0] != 0 {
				return descriptor{}, errors.Join(ErrCorrupt, err)
			}
			n.value, err = write(raw)
			if err != nil {
				return descriptor{}, err
			}
		} else {
			for i, child := range n.children {
				n.children[i], err = copyTree(child)
				if err != nil {
					return descriptor{}, err
				}
			}
		}
		raw, err := encodeNode(n, content.extent)
		if err != nil {
			return descriptor{}, err
		}
		r, err := write(raw)
		if err != nil {
			return descriptor{}, err
		}
		first, last := n.bounds()
		return descriptor{first: first, last: last, count: n.count, height: n.height, ref: r}, nil
	}
	if content.count != 0 {
		d, err := copyTree(descriptor{count: content.count, height: content.height, ref: content.root})
		if err != nil {
			return nil, err
		}
		content.root = d.ref
	}
	// A growing live set cannot be reclaimed. Leave the current root unchanged
	// when the authenticated copy saves less than one eighth of physical bytes.
	if content.extent > s.physicalExtent-s.physicalExtent/8 {
		return nil, nil
	}
	header, err := content.encode(s.key)
	if err != nil {
		return nil, err
	}
	if n, err := file.WriteAt(header, 0); err != nil || n != len(header) {
		return nil, errors.Join(err, io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := s.phase("repack-pages-synced"); err != nil {
		return nil, err
	}
	next := cowRoot{cut: s.current.cut, revision: s.physicalRevision + 1, content: content}
	raw, err := next.encode(s.key)
	if err != nil {
		return nil, err
	}
	c.originOwner, _, err = writeRepackControl(s.slotPath(slot, "origin.root"), raw)
	if err != nil {
		return nil, err
	}
	var rootInfo os.FileInfo
	c.rootOwner, rootInfo, err = writeRepackControl(s.slotPath(slot, "active.root"), raw)
	if err != nil {
		return nil, err
	}
	if err := durable.SyncDirectory(s.dir); err != nil {
		return nil, err
	}
	if err := s.phase("repack-roots-synced"); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := c.packOwner.VerifyFile(file); err != nil {
		return nil, err
	}
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot()); err != nil {
		return nil, err
	}
	if err := work.ReleasePart(resourcebudget.Cost{RAM: BuildRAMBytes - 4096}); err != nil {
		return nil, err
	}
	retirement, err := s.captureRepackRetirement(ctx)
	if err != nil {
		return nil, err
	}
	c.old = repackState{controls: retirement, slot: s.packSlot, file: s.pack, info: retirement[0], rootInfo: s.rootInfo, root: s.current, rootDigest: s.rootDigest, packDigest: s.packDigest, physicalDigest: s.physicalRootDigest, extent: s.physicalExtent, revision: s.physicalRevision}
	if s.physical != nil {
		c.old.physicalAllocatedEnd = s.physical.allocatedEnd
		if s.physical.receipt != nil {
			c.old.physicalClaims, c.old.physicalReceipt = s.physical.receipt.claims, true
		}
	}
	if err := c.preparePhysicalReceiptLocked(file, slot, next, raw, rootInfo); err != nil {
		return nil, err
	}
	s.packSlot, s.pack, s.info, s.rootInfo, s.current = slot, file, info, rootInfo, next
	s.rootDigest, s.physicalRootDigest, s.packDigest = sha256.Sum256(raw), sha256.Sum256(raw), sha256.Sum256(header)
	s.physicalExtent, s.physicalRevision, s.repack = content.extent, next.revision, c
	if s.physical != nil {
		s.physical.allocatedEnd = int64(content.extent)
	}
	installed = true
	return c, nil
}

func (c *StoreRepack) finish(ctx context.Context, committed bool) (resultErr error) {
	if c == nil {
		return nil
	}
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.done {
		return c.err
	}
	if c.err != nil || s.repack != c {
		return errors.Join(ErrRecovery, c.err)
	}
	selected, found, err := c.publisher.CurrentContext(ctx)
	if err != nil || !found {
		return errors.Join(ErrRecovery, err)
	}
	if committed {
		if selected == c.before || !selectedContains(selected, sealRootFor(s.current, s.rootDigest, c.schema)) {
			return ErrSealIneligible
		}
		if err := s.phase("repack-common-selected"); err != nil {
			return err
		}
		if err := c.commitPhysicalReceiptLocked(); err != nil {
			c.err, s.retirementErr = err, err
			s.poison(err)
			return err
		}
		if err := ownedfile.CloseFile(c.old.file); err != nil {
			c.err = err
			s.retirementErr = err
			s.poison(err)
			return err
		}
		c.old.file = nil
		// The common seal now selects the compact pack and every old pin has
		// joined. Retire the old inode here: waiting for the next repack would
		// retain dead pages indefinitely when the new pack stays small.
		if err := c.retireCapturedSource(); err != nil {
			c.err, s.retirementErr = err, err
			s.poison(err)
			return err
		}
		if err := s.phase("repack-old-pack-retired"); err != nil {
			return err
		}
	} else {
		if selected != c.before {
			return ErrSealIneligible
		}
		if err := ownedfile.CloseFile(s.pack); err != nil {
			c.err = err
			s.retirementErr = err
			s.poison(err)
			return err
		}
		old := c.old
		s.packSlot, s.pack, s.info, s.rootInfo, s.current = old.slot, old.file, old.info, old.rootInfo, old.root
		s.rootDigest, s.packDigest, s.physicalRootDigest = old.rootDigest, old.packDigest, old.physicalDigest
		s.physicalExtent, s.physicalRevision = old.extent, old.revision
		if s.physical != nil {
			s.physical.allocatedEnd = old.physicalAllocatedEnd
			if err := c.rollbackPhysicalReceiptLocked(); err != nil {
				c.err, s.retirementErr = err, err
				s.poison(err)
				return err
			}
		}
		for _, owner := range []*ownedfile.Owner{c.rootOwner, c.originOwner, c.packOwner, c.intentOwner} {
			if owner != nil {
				resultErr = errors.Join(resultErr, owner.Retire())
			}
		}
		resultErr = errors.Join(resultErr, durable.SyncDirectory(s.dir))
	}
	if resultErr != nil {
		c.err = resultErr
		s.poison(resultErr)
		return resultErr
	}
	resultErr = errors.Join(c.work.Release(), c.disk.Release())
	if resultErr != nil {
		c.err = resultErr
		s.poison(resultErr)
		return resultErr
	}
	c.done, s.repack = true, nil
	return nil
}

func (c *StoreRepack) Commit() error   { return c.CommitContext(context.Background()) }
func (c *StoreRepack) Rollback() error { return c.RollbackContext(context.Background()) }

// Context variants retain the original maintenance/completion workspace for
// actual selection reads; they do not change the independent common gate.
func (c *StoreRepack) CommitContext(ctx context.Context) error   { return c.finish(ctx, true) }
func (c *StoreRepack) RollbackContext(ctx context.Context) error { return c.finish(ctx, false) }
