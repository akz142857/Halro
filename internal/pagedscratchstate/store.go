// Package scratchstate owns an exclusive, unpublished disk reducer. There is
// deliberately no Open API: saved scratch bytes never choose a trusted state.
package pagedscratchstate

import (
	"context"
	"crypto/rand"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const (
	// Includes the fixed block bank, metadata and verification buffer below;
	// the remainder pays bounded key/value, tree and iterator workspaces.
	WorkspaceRAMBytes int64 = 64 << 20
	MaxKeyBytes             = 32769
	MaxValueBytes           = 3 << 20
	maxHeight               = 24
	pageBytes               = 4096
	cacheWindowBytes        = 16 << 20
)

var (
	ErrCorrupt  = errors.New("scratch page or tree relationship is invalid")
	ErrLimit    = errors.New("scratch workspace or disk limit exhausted")
	ErrPoisoned = errors.New("unpublished scratch candidate is poisoned")
)

type Options struct {
	Budget       *resourcebudget.Manager
	MaxDiskBytes int64
	Reservation  resourcebudget.Permit
	// Private experiment switches; zero values preserve the frozen paged prototype.
	CompactBlobs        bool
	SharedOriginals     bool
	ReuseAuthentication bool
}

type link struct {
	id   uint64
	hash [32]byte
}
type blobRef struct {
	link link
	size uint32
}

// Store keeps only actual file owners, finite tree/free-list roots and a fresh
// independent MAC key. Neither rows nor a growing page map is retained in RAM.
type Store struct {
	mu              sync.Mutex
	dir             string
	dirInfo         os.FileInfo
	parentInfo      os.FileInfo
	directory       *os.File
	dirRemoved      bool
	files           [2]*os.File
	owners          [2]*ownedfile.Owner
	retired         [2]bool
	key             [32]byte
	mac             hash.Hash // fixed owner-local HMAC workspace; guarded by mu
	macInput        [MaxKeyBytes + 512]byte
	macOutput       [32]byte
	root            link
	free            [2]link
	next            [2]uint64
	freeCount       [2]uint64
	rows            uint64
	operation       uint64 // private mutation lifetime; no receipt crosses a Put
	compactBlobs    bool
	sharedOriginals bool
	ceiling         int64
	work, disk      resourcebudget.Permit
	unsynced        int64
	readBytes       int64
	cacheErr        error
	blockBank       *transactionBlockBank
	nodeReceipts    *nodeAuthenticationReceipts
	fault           error
	closed          bool
	closeErr        error
	discarded       bool
}

type Failure struct {
	Owner *Store
	Cause error
}

func (e *Failure) Error() string { return e.Cause.Error() }
func (e *Failure) Unwrap() error { return e.Cause }

func Create(ctx context.Context, dir string, o Options) (result *Store, resultErr error) {
	if o.Budget == nil || o.MaxDiskBytes < int64(pageBytes+blobPageSize(o.CompactBlobs)) {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: WorkspaceRAMBytes})
	if err != nil {
		return nil, err
	}
	var disk resourcebudget.Permit
	if o.Reservation != (resourcebudget.Permit{}) {
		disk, err = o.Reservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	} else {
		disk, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	}
	if err != nil {
		return nil, errors.Join(err, work.Release())
	}
	s := &Store{compactBlobs: o.CompactBlobs, sharedOriginals: o.SharedOriginals, dir: filepath.Clean(dir), ceiling: o.MaxDiskBytes, work: work, disk: disk, blockBank: new(transactionBlockBank), next: [2]uint64{1, 1}}
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, errors.Join(err, disk.Release(), work.Release())
	}
	if o.ReuseAuthentication {
		s.nodeReceipts = new(nodeAuthenticationReceipts)
	}
	s.initializeMAC()
	s.parentInfo, err = os.Stat(filepath.Dir(s.dir))
	if err != nil || !s.parentInfo.IsDir() {
		s.mac = nil
		clear(s.key[:])
		return nil, errors.Join(ownedfile.ErrReplaced, err, disk.Release(), work.Release())
	}
	if err := os.Mkdir(s.dir, 0700); err != nil {
		s.mac = nil
		clear(s.key[:])
		return nil, errors.Join(err, disk.Release(), work.Release())
	}
	s.directory, err = os.Open(s.dir)
	if err != nil {
		return s, &Failure{s, errors.Join(ownedfile.ErrRetirement, err)}
	}
	s.dirInfo, err = s.directory.Stat()
	if err != nil || !s.dirInfo.IsDir() {
		s.closeErr = errors.Join(ownedfile.ErrRetirement, err)
		return s, &Failure{s, s.closeErr}
	}
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				s.closeErr = errors.Join(s.closeErr, resultErr)
				result, resultErr = s, &Failure{s, resultErr}
				return
			}
			if cleanup := s.Discard(); cleanup != nil {
				resultErr = &Failure{s, errors.Join(resultErr, cleanup)}
				result = s
			}
		}
	}()
	for i, name := range []string{"nodes", "blobs"} {
		s.files[i], s.owners[i], err = ownedfile.Create(filepath.Join(s.dir, name), os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if _, err := s.files[i].Write(make([]byte, s.pageSize(i))); err != nil {
			return nil, err
		}
	}
	if err := s.syncParent(); err != nil {
		return nil, err
	}
	return s, nil
}

type Transaction struct {
	store    *Store
	ctx      context.Context
	failure  error
	readOnly bool
	active   bool
}

// Update does not claim a rollback of unpublished writes. Any callback, page,
// allocation or I/O error poisons the entire candidate. Its builder must discard
// it; subsequent reads and mutations refuse until actual retirement finishes.
func (s *Store) Update(ctx context.Context, fn func(*Transaction) error) error {
	return s.transaction(ctx, false, fn)
}
func (s *Store) View(ctx context.Context, fn func(*Transaction) error) error {
	return s.transaction(ctx, true, fn)
}
func (s *Store) transaction(ctx context.Context, readOnly bool, fn func(*Transaction) error) error {
	if s == nil || fn == nil {
		return ErrPoisoned
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.fault != nil {
		return errors.Join(ErrPoisoned, s.fault, s.closeErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.verifyTransactionFiles(); err != nil {
		s.fault = err
		return err
	}
	s.resetNodeReceipts()
	s.blockBank.begin()
	t := &Transaction{store: s, ctx: ctx, readOnly: readOnly, active: true}
	completed := false
	defer func() {
		t.active = false
		s.blockBank.end()
		s.resetNodeReceipts()
		if !completed {
			s.fault = ErrPoisoned
		}
	}()
	err := errors.Join(fn(t), t.failure, ctx.Err())
	if err == nil && !readOnly {
		err = s.flushDirtyBank()
		if err == nil {
			err = s.syncBankTransaction()
		}
		if err == nil {
			err = s.flushCache(false)
		}
	}
	// Callback success is not a namespace or descriptor receipt. Join both
	// actual files and the captured directory again before any successful exit.
	err = errors.Join(err, s.verifyTransactionFiles(), s.verifyBlockBank(), ctx.Err())
	if err != nil {
		s.fault = err
	}
	completed = true
	return err
}
func (t *Transaction) fail(err error) error {
	if err != nil {
		t.failure = errors.Join(t.failure, err)
	}
	return err
}
func (t *Transaction) valid() error {
	if t == nil || t.store == nil || !t.active || t.failure != nil {
		return ErrPoisoned
	}
	return t.ctx.Err()
}
func (s *Store) verifyNames() error {
	parent, parentErr := os.Lstat(filepath.Dir(s.dir))
	if parentErr != nil || s.parentInfo == nil || !parent.IsDir() || !os.SameFile(parent, s.parentInfo) {
		return errors.Join(ownedfile.ErrReplaced, parentErr)
	}
	info, err := os.Lstat(s.dir)
	if err != nil || !info.IsDir() || s.dirInfo == nil || !os.SameFile(info, s.dirInfo) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	for i, owner := range s.owners {
		if s.retired[i] {
			continue
		}
		if owner != nil {
			if err := owner.VerifyName(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) verifyTransactionFiles() error {
	if _, ok := s.allocatedBytes(); !ok {
		return ErrCorrupt
	}
	if err := s.verifyNames(); err != nil {
		return err
	}
	actual, err := s.directory.Stat()
	if err != nil || !actual.IsDir() || !os.SameFile(actual, s.dirInfo) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	for kind, file := range s.files {
		if s.retired[kind] || s.owners[kind] == nil || file == nil {
			return ownedfile.ErrReplaced
		}
		if err := s.owners[kind].VerifyFile(file); err != nil {
			return err
		}
		info, err := file.Stat()
		// next includes the initial reserved page. A foreign append, truncate
		// or sparse extension cannot become extra uncharged scratch contents.
		if err != nil || !info.Mode().IsRegular() || s.next[kind] == 0 || s.next[kind] > uint64(s.ceiling/int64(s.pageSize(kind))) || info.Size() != int64(s.next[kind])*int64(s.pageSize(kind)) {
			return errors.Join(ErrCorrupt, err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	for _, f := range []*os.File{s.files[0], s.files[1], s.directory} {
		if f != nil {
			s.closeErr = errors.Join(s.closeErr, ownedfile.CloseFile(f))
		}
	}
	if s.closeErr == nil {
		s.mac = nil
		clear(s.key[:])
		s.blockBank = nil
		s.nodeReceipts = nil
	}
	return s.closeErr
}
func (s *Store) Discard() error {
	if s == nil {
		return nil
	}
	if err := s.Close(); err != nil {
		return &Failure{s, err}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.discarded {
		return nil
	}
	if !s.dirRemoved {
		if err := s.verifyNames(); err != nil {
			return &Failure{s, err}
		}
	}
	for i, owner := range s.owners {
		if s.retired[i] {
			continue
		}
		if owner != nil {
			if err := owner.Retire(); err != nil {
				return &Failure{s, errors.Join(ownedfile.ErrRetirement, err)}
			}
			s.retired[i] = true
		}
	}
	if !s.dirRemoved {
		if err := os.Remove(s.dir); err != nil {
			return &Failure{s, errors.Join(ownedfile.ErrRetirement, err)}
		}
		s.dirRemoved = true
	}
	if err := s.syncParent(); err != nil {
		return &Failure{s, errors.Join(ownedfile.ErrRetirement, err)}
	}
	for _, permit := range []*resourcebudget.Permit{&s.disk, &s.work} {
		if *permit != (resourcebudget.Permit{}) {
			if err := permit.Release(); err != nil {
				return &Failure{s, errors.Join(ownedfile.ErrRetirement, err)}
			}
			*permit = resourcebudget.Permit{}
		}
	}
	s.discarded = true
	return nil
}

// The actual parent descriptor and its captured identity determine the sync.
// Unknown descriptor Close is permanently retained by the Store and error owner.
func (s *Store) syncParent() (resultErr error) {
	f, err := os.Open(filepath.Dir(s.dir))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := ownedfile.CloseFile(f); closeErr != nil {
			s.closeErr = errors.Join(s.closeErr, closeErr)
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	actual, err := f.Stat()
	named, nameErr := os.Lstat(filepath.Dir(s.dir))
	if err != nil || nameErr != nil || !actual.IsDir() || !named.IsDir() || !os.SameFile(s.parentInfo, actual) || !os.SameFile(s.parentInfo, named) {
		return errors.Join(ownedfile.ErrReplaced, err, nameErr)
	}
	return f.Sync()
}
