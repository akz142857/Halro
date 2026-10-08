package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

const cowRootBytes = 240
const storeControlDisk int64 = 2*cowRootBytes + 128
const maxStorePins = 4
const UpdateRAMBytes int64 = 64 << 20

// StoreResidentRAMBytes remains charged until an opened store and its readers
// have joined. It is separate from each query/update workspace and disk owner.
const StoreResidentRAMBytes int64 = 4096 + maxNodeBytes

var ErrRecovery = errors.New("authenticated store requires reopen and native eligibility check")

type storeFault struct{ err error }
type cowRoot struct {
	cut      [32]byte
	revision uint64
	content  commitment
}

func (r cowRoot) encode(key []byte) ([]byte, error) {
	if r.cut == ([32]byte{}) || r.revision == 0 {
		return nil, ErrCorrupt
	}
	content, e := r.content.encode(key)
	if e != nil {
		return nil, e
	}
	b := append([]byte("HLCOWR01"), r.cut[:]...)
	b = binary.BigEndian.AppendUint64(b, r.revision)
	b = append(b, content...)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:authenticated-state:cow-root:v1\x00"))
	mac.Write(b)
	return mac.Sum(b), nil
}
func decodeCOWRoot(b, key []byte, scope, cut [32]byte) (cowRoot, error) {
	var r cowRoot
	if len(key) != 32 || len(b) != cowRootBytes || !bytes.Equal(b[:8], []byte("HLCOWR01")) {
		return r, ErrCorrupt
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:authenticated-state:cow-root:v1\x00"))
	mac.Write(b[:cowRootBytes-32])
	if !hmac.Equal(mac.Sum(nil), b[cowRootBytes-32:]) {
		return r, ErrCorrupt
	}
	copy(r.cut[:], b[8:40])
	r.revision = binary.BigEndian.Uint64(b[40:48])
	if r.cut != cut || r.cut == ([32]byte{}) || r.revision == 0 {
		return cowRoot{}, ErrCorrupt
	}
	c, e := decodeCommitment(b[48:208], key, scope)
	if e != nil {
		return cowRoot{}, e
	}
	r.content = c
	return r, nil
}

type StoreOptions struct {
	ReadOnly           bool     // refuses updates and all orphan/tail repair on existing state
	PhysicalRecovery   bool     // defers orphan repair until independent physical receipt/source qualification; grants no write permission
	Scope, ExpectedCut [32]byte // independently established domain scope/native cut
	Key                []byte
	MaxDiskBytes       int64 // includes the bounded root-control files, not FS block overhead
	MaxAppendBytes     int64 // maximum pages/blobs appended by one transaction
	Budget             *resourcebudget.Manager
	DiskReservation    resourcebudget.Permit // required on reopen; publisher keeps its own owner
	ExpectedRoot       *RootIdentity         // optional independent full identity; checked before orphan repair
	ExpectedSealRoot   *SealRoot             // optional selected logical root; checked before orphan repair
	ExpectedSealSchema *SealRootSchema       // independent versioned schema required with ExpectedSealRoot
}

func (o StoreOptions) valid() bool {
	if o.Scope == ([32]byte{}) || o.ExpectedCut == ([32]byte{}) || len(o.Key) != 32 || o.MaxDiskBytes <= storeControlDisk+headerBytes || o.MaxAppendBytes <= 0 || o.MaxAppendBytes > o.MaxDiskBytes-storeControlDisk || o.Budget == nil {
		return false
	}
	if o.ExpectedSealRoot == nil {
		return o.ExpectedSealSchema == nil
	}
	return o.ExpectedSealSchema != nil &&
		o.ExpectedSealSchema.Kind == SealRootState && o.ExpectedSealRoot.Kind == SealRootState &&
		o.ExpectedSealSchema.Component >= SealComponentLedger && o.ExpectedSealSchema.Component <= SealComponentLogDirectory &&
		o.ExpectedSealSchema.IndexID != 0 && o.ExpectedSealSchema.Codec != 0
}

type Store struct {
	readOnly                              bool
	physicalRecoveryPending               bool
	packSlot                              uint8
	physical                              *physicalStoreOwner
	repack                                *StoreRepack
	mu                                    sync.Mutex
	pack                                  *os.File
	info, dirInfo, rootInfo, lockInfo     os.FileInfo
	dir                                   string
	guard                                 *lock.Lock
	readBorrowParent                      *Store
	readBorrowCount                       uint8 // fixed four lifetime loans; guarded by parent mu
	readBorrowChildren                    [4]*Store
	readBorrowComponentCleanup            bool         // guarded by parent mu
	readBorrowComponentJoined             bool         // guarded by child mu
	readBorrowCleanup                     func() error // immutable after registration
	key                                   []byte
	current                               cowRoot
	rootDigest, packDigest                [32]byte
	physicalRootDigest                    [32]byte
	physicalExtent, physicalRevision      uint64
	ceiling, appendLimit                  int64
	budget                                *resourcebudget.Manager
	control                               resourcebudget.Permit
	resident                              resourcebudget.Permit
	base                                  *Snapshot // captured fresh pack owner, if created by this handle
	borrowed                              bool
	pins                                  [maxStorePins]atomic.Pointer[Snapshot]
	failure                               atomic.Pointer[storeFault]
	closing, closed, discarded            bool
	discardPackRetired, discardDirRetired bool
	closeErr                              error
	retirementErr                         error
	retiredUpdate                         resourcebudget.Permit
	closeRootFile                         func(*os.File) error // private actual-FD close seam
	syncRetiredDirectory                  func(string) error   // private actual directory exit seam
	closeDone                             chan struct{}
	auditCancel                           context.CancelFunc // one complete fixed-root audit job
	hook                                  func(string) error // fault injection; nil in production
}

func (s *Store) fault() error {
	if f := s.failure.Load(); f != nil {
		return errors.Join(ErrRecovery, f.err)
	}
	return nil
}
func (s *Store) poison(e error) {
	if e != nil {
		s.failure.CompareAndSwap(nil, &storeFault{e})
	}
}
func (s *Store) phase(name string) error {
	if s.hook != nil {
		return s.hook(name)
	}
	return nil
}
func syncDir(path string) error { return durable.SyncDirectory(path) }
func readFixed(path string, size int) ([]byte, os.FileInfo, error) {
	pathInfo, e := os.Lstat(path)
	if e != nil || !pathInfo.Mode().IsRegular() || pathInfo.Size() != int64(size) {
		return nil, nil, errors.Join(ErrCorrupt, e)
	}
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, nil, e
	}
	info, e := f.Stat()
	if e != nil || !os.SameFile(pathInfo, info) {
		return nil, nil, errors.Join(ErrCorrupt, e, ownedfile.CloseFile(f))
	}
	b := make([]byte, size)
	_, e = io.ReadFull(f, b)
	e = errors.Join(e, ownedfile.CloseFile(f))
	if e != nil {
		return nil, nil, errors.Join(ErrCorrupt, e)
	}
	current, e := os.Lstat(path)
	if e != nil || !os.SameFile(info, current) {
		return nil, nil, errors.Join(ErrCorrupt, e)
	}
	return b, info, nil
}

// CreateStore imports an already authenticated complete stream. The source
// authenticity and initial native cut are caller obligations, exactly as for
// Build. No current ordinary DB is automatically accepted by this API.
func CreateStore(ctx context.Context, dir string, o StoreOptions, next func() (Row, bool, error)) (result *Store, resultErr error) {
	if !o.valid() || o.ReadOnly || next == nil {
		return nil, ErrLimit
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	permit, e := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: 4096, Disk: storeControlDisk})
	if e != nil {
		return nil, e
	}
	if e = os.Mkdir(dir, 0o700); e != nil {
		return nil, errors.Join(e, permit.Release())
	}
	info, e := os.Lstat(dir)
	if e != nil {
		return nil, errors.Join(e, permit.Release())
	}
	s := &Store{dir: dir, dirInfo: info, key: bytes.Clone(o.Key), ceiling: o.MaxDiskBytes - storeControlDisk, appendLimit: o.MaxAppendBytes, budget: o.Budget, control: permit}
	failed := true
	defer func() {
		if failed {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				s.retirementErr = errors.Join(s.retirementErr, resultErr)
			}
			if cleanupErr := s.Discard(); cleanupErr != nil {
				result, resultErr = s, errors.Join(resultErr, cleanupErr)
			}
		}
	}()
	if e = syncDir(filepath.Dir(dir)); e != nil {
		return nil, e
	}
	s.guard, e = lock.Acquire(dir)
	if e != nil {
		return nil, e
	}
	s.lockInfo, e = os.Lstat(filepath.Join(dir, ".halro.lock"))
	if e != nil {
		return nil, e
	}
	base, e := buildSnapshot(ctx, filepath.Join(dir, "pages"), Options{Context: o.Scope, Key: o.Key, MaxDiskBytes: s.ceiling, Budget: o.Budget}, next, true)
	if e != nil {
		return nil, e
	}
	s.base = base
	s.current = cowRoot{o.ExpectedCut, 1, base.current}
	s.physicalExtent, s.physicalRevision = base.current.extent, 1
	s.packDigest = base.headerDigest
	s.info = base.info
	if e = base.Close(); e != nil {
		return nil, e
	}
	merged, e := s.control.Merge(base.disk)
	if e != nil {
		return nil, e
	}
	s.control = merged
	base.externalDiskOwner = true
	s.pack, e = os.OpenFile(filepath.Join(dir, "pages"), os.O_RDWR, 0)
	if e != nil {
		return nil, e
	}
	if e = s.checkPack(); e != nil {
		return nil, e
	}
	if e = s.publish(ctx, s.current); e != nil {
		return nil, e
	}
	failed = false
	return s, nil
}

// OpenStore verifies the independently eligible cut before accepting the root.
// Its existing persistent bytes remain the external publisher's disk obligation.
// A complete selected seal can name an older root while preserving the newer
// physical publication. A valid unpublished staging root is discarded; malformed/foreign staging is
// preserved and fails closed. Orphan pack tail is trimmed under the writer lock.
func OpenStore(ctx context.Context, dir string, o StoreOptions) (*Store, error) {
	return openStore(ctx, dir, o, nil)
}

// readParent is set only under that live actual owner mutex, never from paths
// or candidate fields. Its existing lock owns the child through joined cleanup.
func openStore(ctx context.Context, dir string, o StoreOptions, readParent *Store) (result *Store, resultErr error) {
	if !o.valid() {
		return nil, ErrLimit
	}
	var expectedRoot RootIdentity
	if o.ExpectedRoot != nil {
		expectedRoot = *o.ExpectedRoot
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	credit, e := o.DiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if e != nil {
		return nil, e
	}
	permit, e := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: StoreResidentRAMBytes})
	if e != nil {
		return nil, errors.Join(e, credit.Release())
	}
	s := &Store{dir: dir, key: bytes.Clone(o.Key), ceiling: o.MaxDiskBytes - storeControlDisk, appendLimit: o.MaxAppendBytes, budget: o.Budget, control: credit, resident: permit, borrowed: true, readOnly: o.ReadOnly, physicalRecoveryPending: o.PhysicalRecovery}
	failed := true
	defer func() {
		if failed {
			cleanupErr := s.Close()
			resultErr = errors.Join(resultErr, cleanupErr)
			if cleanupErr != nil {
				result = s
			}
		}
	}()
	s.dirInfo, e = os.Lstat(dir)
	if e != nil || !s.dirInfo.IsDir() {
		return nil, errors.Join(ErrCorrupt, e)
	}
	if readParent == nil {
		s.guard, e = lock.AcquireExistingReadOnly(dir)
		if e != nil {
			return nil, e
		}
	} else {
		s.readBorrowParent = readParent
	}
	if e = s.selectPackSlot(o); e != nil {
		return nil, e
	}
	raw, info, e := readFixed(s.slotPath(s.packSlot, "active.root"), cowRootBytes)
	if e != nil {
		return nil, e
	}
	// Authenticate the physical publisher first, then select the logical root.
	// Historical selection never rolls back/truncates a committed newer pack.
	var physicalCut [32]byte
	copy(physicalCut[:], raw[8:40])
	physical, e := decodeCOWRoot(raw, o.Key, o.Scope, physicalCut)
	if e != nil {
		return nil, e
	}
	r, selectedDigest := physical, sha256.Sum256(raw)
	if o.ExpectedSealRoot != nil {
		r, selectedDigest, e = selectedCOWRoot(*o.ExpectedSealRoot, *o.ExpectedSealSchema, o.Key, o.Scope, o.ExpectedCut)
		if e != nil {
			return nil, e
		}
		if r.content.id != physical.content.id || r.revision > physical.revision || r.content.extent > physical.content.extent || r.revision == physical.revision && selectedDigest != sha256.Sum256(raw) {
			return nil, ErrRecovery
		}
	} else if r.cut != o.ExpectedCut {
		return nil, ErrCorrupt
	}
	actualRoot := RootIdentity{Scope: r.content.context, Cut: r.cut, Digest: selectedDigest, ID: r.content.id, Revision: r.revision, Rows: r.content.count}
	if o.ExpectedRoot != nil && actualRoot != expectedRoot {
		return nil, ErrRecovery
	}
	s.current = r
	s.rootInfo = info
	s.rootDigest = selectedDigest
	s.physicalRootDigest = sha256.Sum256(raw)
	s.physicalExtent, s.physicalRevision = physical.content.extent, physical.revision
	path := s.slotPath(s.packSlot, "pages")
	pathInfo, e := os.Lstat(path)
	if e != nil || !pathInfo.Mode().IsRegular() {
		return nil, errors.Join(ErrCorrupt, e)
	}
	flags := os.O_RDWR
	if o.ReadOnly {
		flags = os.O_RDONLY
	}
	s.pack, e = os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	s.info, e = s.pack.Stat()
	if e != nil || !os.SameFile(pathInfo, s.info) {
		return nil, errors.Join(ErrCorrupt, e)
	}
	var header [headerBytes]byte
	if _, e = s.pack.ReadAt(header[:], 0); e != nil {
		return nil, errors.Join(ErrCorrupt, e)
	}
	initial, e := decodeCommitment(header[:], o.Key, o.Scope)
	if e != nil || initial.id != r.content.id || r.content.extent < initial.extent {
		return nil, ErrCorrupt
	}
	s.packDigest = sha256.Sum256(header[:])
	if e = s.checkPack(); e != nil {
		return nil, e
	}
	// Prove the selected page before making any cleanup mutation.
	if r.content.count != 0 {
		page := make([]byte, r.content.root.length)
		if _, e = s.pack.ReadAt(page, int64(r.content.root.offset)); e != nil || sha256.Sum256(page) != r.content.root.digest {
			return nil, errors.Join(ErrRecovery, e)
		}
		n, e := decodeNode(page, r.content.extent)
		if e != nil || n.count != r.content.count || n.height != r.content.height {
			return nil, errors.Join(ErrRecovery, e)
		}
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	pending := s.slotPath(s.packSlot, "root.pending")
	if _, e = os.Lstat(pending); e == nil && !o.PhysicalRecovery {
		if o.ReadOnly {
			return nil, ErrRecovery
		}
		b, pendingInfo, e := readFixed(pending, cowRootBytes)
		if e != nil {
			return nil, errors.Join(ErrRecovery, e)
		}
		var cut [32]byte
		copy(cut[:], b[8:40])
		candidate, e := decodeCOWRoot(b, o.Key, o.Scope, cut)
		if e != nil || candidate.content.id != physical.content.id || candidate.revision != physical.revision+1 || candidate.content.extent < physical.content.extent || candidate.content.extent > uint64(s.info.Size()) {
			return nil, ErrRecovery
		}
		if e = removeCaptured(pending, pendingInfo); e != nil {
			return nil, e
		}
		if e = syncDir(dir); e != nil {
			return nil, e
		}
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	if s.info.Size() > int64(physical.content.extent) && !o.PhysicalRecovery {
		if o.ReadOnly {
			return nil, ErrRecovery
		}
		if e = s.pack.Truncate(int64(physical.content.extent)); e != nil {
			return nil, e
		}
		if e = s.pack.Sync(); e != nil {
			return nil, e
		}
	}
	if e = s.checkRoot(); e != nil {
		return nil, e
	}
	failed = false
	return s, nil
}
func (s *Store) checkPack() error {
	if s.pack == nil {
		return ErrClosed
	}
	info, e := s.pack.Stat()
	path, e2 := os.Lstat(s.slotPath(s.packSlot, "pages"))
	dir, e3 := os.Lstat(s.dir)
	if e != nil || e2 != nil || e3 != nil || !os.SameFile(s.info, info) || !os.SameFile(s.info, path) || !os.SameFile(s.dirInfo, dir) || info.Size() < int64(s.physicalExtent) || info.Size() > s.ceiling {
		return errors.Join(ErrCorrupt, e, e2, e3)
	}
	var b [headerBytes]byte
	if _, e = s.pack.ReadAt(b[:], 0); e != nil || sha256.Sum256(b[:]) != s.packDigest {
		return errors.Join(ErrCorrupt, e)
	}
	return nil
}
func (s *Store) checkRoot() error {
	if s.rootInfo == nil {
		if _, e := os.Lstat(s.slotPath(s.packSlot, "active.root")); !errors.Is(e, os.ErrNotExist) {
			return errors.Join(ownedfile.ErrReplaced, e)
		}
		return nil
	}
	raw, info, e := readFixed(s.slotPath(s.packSlot, "active.root"), cowRootBytes)
	if e != nil || !os.SameFile(s.rootInfo, info) || sha256.Sum256(raw) != s.physicalRootDigest {
		return errors.Join(ErrCorrupt, e)
	}
	return nil
}
func (s *Store) publish(ctx context.Context, next cowRoot) (resultErr error) {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := s.checkPack(); e != nil {
		return e
	}
	if e := s.checkRoot(); e != nil {
		return e
	}
	b, e := next.encode(s.key)
	if e != nil {
		return e
	}
	if e = s.pack.Sync(); e != nil {
		return e
	}
	if e = s.phase("pages-synced"); e != nil {
		return e
	}
	path := s.slotPath(s.packSlot, "root.pending")
	if u := physicalUpdate(ctx, s); u != nil && u.slot.pendingMove && s.physical.receipt != nil {
		copy(s.physical.receipt.claims.PendingRoot[:], b)
		if e := s.savePhysicalReceiptLocked(); e != nil {
			return e
		}
		if e := s.phase("physical-pending-root-intent-synced"); e != nil {
			return e
		}
	}
	f, owner, e := s.physicalPending(ctx, path)
	if e != nil {
		return e
	}
	closed, published, uncertain := false, false, false
	closeFile := s.closeRootFile
	if closeFile == nil {
		closeFile = func(file *os.File) error { return file.Close() }
	}
	defer func() {
		if !closed {
			closed = true
			err := closeFile(f)
			recordPhysicalClose(ctx, s, err)
			if err != nil {
				uncertain = true
				resultErr = errors.Join(resultErr, ownedfile.CloseError(f, err))
			}
		}
		if uncertain {
			s.retirementErr = errors.Join(s.retirementErr, resultErr)
			return
		}
		if !published {
			if err := owner.Retire(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
				s.retirementErr = errors.Join(s.retirementErr, resultErr)
			}
		}
	}()

	n, e := f.Write(b)
	if e != nil || n != len(b) {
		return errors.Join(e, io.ErrShortWrite)
	}
	if e = s.phase("root-written"); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	info, e := f.Stat()
	if e != nil {
		return e
	}
	e = closeFile(f)
	recordPhysicalClose(ctx, s, e)
	closed = true
	if e != nil {
		uncertain = true
		return ownedfile.CloseError(f, e)
	}
	if e = s.phase("root-synced"); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	current, e := os.Lstat(path)
	if e != nil || !os.SameFile(info, current) {
		return errors.Join(ownedfile.ErrReplaced, e)
	}
	if e = s.checkRoot(); e != nil {
		return e
	}
	if e = os.Rename(path, s.slotPath(s.packSlot, "active.root")); e != nil {
		return e
	}
	published = true // The pending name no longer owns an unlink obligation.
	if u := physicalUpdate(ctx, s); u != nil {
		u.slot.published = true
	}
	// Capture deletion identity immediately after rename, independently of
	// whether the domain/publication barrier later succeeds. A failed private
	// publisher may retire its OWN root, while foreign replacements still refuse.
	s.rootInfo, s.physicalRootDigest = info, sha256.Sum256(b)
	s.physicalExtent, s.physicalRevision = next.content.extent, next.revision
	if e = s.phase("root-renamed"); e != nil {
		return e
	}
	if e = syncDir(s.dir); e != nil {
		return e
	}
	if e = s.phase("directory-synced"); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	s.current, s.rootDigest = next, s.physicalRootDigest
	return nil
}

// Snapshot pins a specific authenticated root in one of four fixed slots. New
// revisions leave its referenced prefix untouched. A Store integrity/publication
// fault rejects further reads on every pin; Close joins and retires all pins.
func (s *Store) Snapshot(ctx context.Context) (*Snapshot, error) {
	if !s.mu.TryLock() {
		return nil, ErrBusy
	}
	defer s.mu.Unlock()
	return s.snapshotLocked(ctx)
}

func (s *Store) snapshotLocked(ctx context.Context) (*Snapshot, error) {
	if s.closing || s.closed {
		return nil, ErrClosed
	}
	if e := s.fault(); e != nil {
		return nil, e
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := errors.Join(s.checkPack(), s.checkRoot()); e != nil {
		s.poison(e)
		return nil, e
	}
	slot := -1
	for i := range s.pins {
		if s.pins[i].Load() == nil {
			slot = i
			break
		}
	}
	if slot < 0 {
		return nil, ErrBusy
	}
	permit, e := s.budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: 4096})
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(s.slotPath(s.packSlot, "pages"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, errors.Join(e, permit.Release())
	}
	info, e := f.Stat()
	if e != nil || !os.SameFile(s.info, info) {
		closeErr := ownedfile.CloseFile(f)
		if errors.Is(closeErr, ownedfile.ErrRetirement) {
			s.retirementErr = errors.Join(s.retirementErr, closeErr)
			s.poison(closeErr)
			return nil, errors.Join(ErrCorrupt, e, closeErr)
		}
		return nil, errors.Join(ErrCorrupt, e, closeErr, permit.Release())
	}
	pin := &Snapshot{file: f, info: info, path: s.slotPath(s.packSlot, "pages"), current: s.current.content, headerDigest: s.rootDigest, key: bytes.Clone(s.key), budget: s.budget, disk: permit, packHeaderDigest: s.packDigest, packCeiling: s.ceiling, fault: s.fault, onCorrupt: s.poison}
	pin.onClose = func() error { s.pins[slot].CompareAndSwap(pin, nil); return nil }
	s.pins[slot].Store(pin)
	return pin, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		e := s.closeErr
		s.mu.Unlock()
		return e
	}
	if s.closing {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		e := s.closeErr
		s.mu.Unlock()
		return e
	}
	s.closeDone = make(chan struct{})
	s.closing = true
	if s.auditCancel != nil {
		s.auditCancel()
	}
	var pins [maxStorePins]*Snapshot
	for i := range s.pins {
		pins[i] = s.pins[i].Load()
	}
	s.mu.Unlock()
	var e error
	for _, pin := range pins {
		if pin != nil {
			e = errors.Join(e, pin.Close())
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e = errors.Join(e, s.retirementErr)
	e = errors.Join(e, s.closePhysicalLocked())
	if failure := s.failure.Load(); failure != nil && errors.Is(failure.err, ownedfile.ErrRetirement) {
		e = errors.Join(e, failure.err)
	}
	if s.pack != nil {
		if closeErr := ownedfile.CloseFile(s.pack); closeErr != nil {
			e = errors.Join(e, closeErr)
		} else {
			s.pack = nil
		}
	}
	if s.repack != nil && s.repack.old.file != nil && s.repack.old.file != s.pack {
		if closeErr := ownedfile.CloseFile(s.repack.old.file); closeErr != nil {
			e = errors.Join(e, closeErr)
		} else {
			s.repack.old.file = nil
		}
	}
	e = errors.Join(e, s.guard.Close())
	if !errors.Is(e, ownedfile.ErrRetirement) && !errors.Is(e, ErrRecovery) {
		clear(s.key)
	}
	if e == nil {
		if s.borrowed {
			e = errors.Join(e, s.resident.Release())
			e = errors.Join(e, s.control.Release())
		}
		// A creator keeps its fixed cleanup/control RAM with the persistent
		// receipt until Discard physically retires the namespace.
	}
	s.closed = true
	s.closeErr = e
	close(s.closeDone)
	return e
}

// DiskReservation returns a retained disk owner after this writer is closed.
// The publisher must keep that owner until its persistent namespace is retired;
// OpenStore retains its own reference for all append/reader work.
func (s *Store) DiskReservation() (resourcebudget.Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed || s.closeErr != nil || s.discarded {
		return resourcebudget.Permit{}, ErrBusy
	}
	return s.control.RetainCovering(s.budget, resourcebudget.Cost{Disk: s.ceiling + storeControlDisk})
}
func removeCaptured(path string, info os.FileInfo) error {
	if info == nil {
		return nil
	}
	current, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !os.SameFile(info, current) {
		return ownedfile.ErrReplaced
	}
	return os.Remove(path)
}

// Discard is only for a newly created Store. It joins every pin before removing
// its captured pack/root/lock/directory. Reopened stores borrow persistent bytes
// and have no deletion authority. Foreign files cause refusal, never RemoveAll.
func (s *Store) Discard() (resultErr error) {
	if s.borrowed {
		return ErrLimit
	}
	if e := s.Close(); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			s.retirementErr = errors.Join(s.retirementErr, resultErr)
			s.closeErr = s.retirementErr
		}
	}()
	syncParent := s.syncRetiredDirectory
	if syncParent == nil {
		syncParent = syncDir
	}
	if s.discarded {
		return nil
	}
	if s.discardDirRetired {
		if e := syncParent(filepath.Dir(s.dir)); e != nil {
			return e
		}
		if e := s.control.Release(); e != nil {
			return e
		}
		if e := s.discardPhysicalFeeLocked(); e != nil {
			return e
		}
		s.discarded = true
		return nil
	}
	current, e := os.Lstat(s.dir)
	if e != nil || !os.SameFile(s.dirInfo, current) {
		return errors.Join(ownedfile.ErrReplaced, e)
	}
	var cleanupGuard *lock.Lock
	lockPath := filepath.Join(s.dir, ".halro.lock")
	if current, e := os.Lstat(lockPath); e == nil {
		if !os.SameFile(s.lockInfo, current) {
			return ownedfile.ErrReplaced
		}
		cleanupGuard, e = lock.AcquireExistingReadOnly(s.dir)
		if e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) || s.lockInfo != nil {
		return errors.Join(ownedfile.ErrReplaced, e)
	}
	guardClosed := false
	defer func() {
		if cleanupGuard != nil && !guardClosed {
			resultErr = errors.Join(resultErr, cleanupGuard.Close())
		}
	}()
	if e = s.checkDiscardNamespace(); e != nil {
		return e
	}
	if s.base != nil {
		if e = s.base.Discard(); e != nil {
			return e
		}
		s.discardPackRetired = true
	}
	if e = retireCaptured(s.slotPath(s.packSlot, "active.root"), &s.rootInfo); e != nil {
		return e
	}
	if e = retireCaptured(lockPath, &s.lockInfo); e != nil {
		return e
	}
	if e = os.Remove(s.dir); e != nil {
		return e
	}
	s.discardDirRetired = true
	if cleanupGuard != nil {
		e = cleanupGuard.Close()
		guardClosed = true
		if e != nil {
			return e
		}
	}
	if e = syncParent(filepath.Dir(s.dir)); e != nil {
		return e
	}
	if e = s.control.Release(); e != nil {
		return e
	}
	if e = s.discardPhysicalFeeLocked(); e != nil {
		return e
	}
	s.discarded = true
	return nil
}

// A nil receipt means this handle already unlinked it, or never created it.
// Missing names without that transition may conceal renamed persistent bytes.
func retireCaptured(path string, receipt *os.FileInfo) error {
	if *receipt == nil {
		return nil
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(*receipt, current) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	*receipt = nil
	return nil
}
