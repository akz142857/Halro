package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/akz142857/Halro/internal/boundedfilecache"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Context is an independently expected digest of the domain format, identity,
// complete scope and native cut. It is not an identity obtained from this file.
type Options struct {
	Context      [32]byte
	Key          []byte
	MaxDiskBytes int64
	Budget       *resourcebudget.Manager
}
type commitment struct {
	context       [32]byte
	id            [16]byte
	extent, count uint64
	height        uint16
	root          ref
}

func (c commitment) valid() bool {
	if c.context == ([32]byte{}) || c.id == ([16]byte{}) || c.extent < headerBytes || c.extent > math.MaxInt64 {
		return false
	}
	if c.count == 0 {
		return c.height == 0 && c.root == (ref{digest: emptyDigest(c.context)})
	}
	return c.height > 0 && c.height <= maxHeight && c.root.valid(c.extent, maxNodeBytes)
}
func (c commitment) encode(key []byte) ([]byte, error) {
	if len(key) != 32 || !c.valid() {
		return nil, ErrCorrupt
	}
	b := make([]byte, headerBytes)
	copy(b, []byte("HLSTATE1"))
	copy(b[8:40], c.context[:])
	copy(b[40:56], c.id[:])
	binary.BigEndian.PutUint64(b[56:64], c.extent)
	binary.BigEndian.PutUint64(b[64:72], c.count)
	binary.BigEndian.PutUint16(b[72:74], c.height)
	copy(b[74:118], putRef(nil, c.root))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:authenticated-state:commitment:v1\x00"))
	mac.Write(b[:128])
	copy(b[128:], mac.Sum(nil))
	return b, nil
}
func decodeCommitment(b, key []byte, expected [32]byte) (commitment, error) {
	var c commitment
	if len(key) != 32 || len(b) != headerBytes || !bytes.Equal(b[:8], []byte("HLSTATE1")) || !bytes.Equal(b[118:128], make([]byte, 10)) {
		return c, ErrCorrupt
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:authenticated-state:commitment:v1\x00"))
	mac.Write(b[:128])
	if !hmac.Equal(mac.Sum(nil), b[128:]) {
		return c, ErrCorrupt
	}
	copy(c.context[:], b[8:40])
	copy(c.id[:], b[40:56])
	c.extent = binary.BigEndian.Uint64(b[56:])
	c.count = binary.BigEndian.Uint64(b[64:])
	c.height = binary.BigEndian.Uint16(b[72:])
	c.root = getRef(b[74:118])
	if c.context != expected || !c.valid() {
		return commitment{}, ErrCorrupt
	}
	return c, nil
}
func validateOptions(o Options) error {
	if len(o.Key) != 32 || o.Context == ([32]byte{}) || o.MaxDiskBytes < headerBytes || o.Budget == nil {
		return ErrLimit
	}
	return nil
}

// Build consumes a strictly ordered, caller-authenticated complete row stream.
// It never grants trust to an arbitrary DB. Output is exclusive and is not an
// active runtime seal. Failure removes only the captured output inode after IO
// exits; a replacement survives. The returned owner retains the disk charge.
func Build(ctx context.Context, path string, o Options, next func() (Row, bool, error)) (*Snapshot, error) {
	return buildSnapshot(ctx, path, o, next, false)
}
func buildSnapshot(ctx context.Context, path string, o Options, next func() (Row, bool, error), retainCeiling bool) (result *Snapshot, resultErr error) {
	return buildSnapshotWithCreate(ctx, path, o, next, retainCeiling, ownedfile.Create)
}

// The seam changes only exclusive construction, permitting tests to exercise
// actual FD failures before an output receipt can reach its admitted owner.
func buildSnapshotWithCreate(ctx context.Context, path string, o Options, next func() (Row, bool, error), retainCeiling bool, create func(string, int, os.FileMode) (*os.File, *ownedfile.Owner, error)) (result *Snapshot, resultErr error) {
	if e := validateOptions(o); e != nil {
		return nil, e
	}
	o.Key = bytes.Clone(o.Key)
	defer clear(o.Key)
	if next == nil {
		return nil, ErrLimit
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	permit, e := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: BuildRAMBytes + 4096, Disk: o.MaxDiskBytes})
	if e != nil {
		return nil, e
	}
	ramCharge := BuildRAMBytes + 4096
	var f *os.File
	var owner *ownedfile.Owner
	failed := true
	defer func() {
		if failed {
			// A failed exclusive constructor may already have created an inode
			// whose descriptor or parent could not be retired. No nil receipt
			// can prove that the original admitted work has ended.
			var closeErr error
			if f != nil {
				closeErr = ownedfile.CloseFile(f)
			}
			if closeErr != nil {
				// An uncertain physical Close is not retirement evidence.
				resultErr = errors.Join(resultErr, closeErr)
				return
			}
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				return
			}
			clean := owner.Retire()
			resultErr = errors.Join(resultErr, clean)
			if clean == nil {
				resultErr = errors.Join(resultErr, permit.Release())
			} else if !errors.Is(clean, ownedfile.ErrRetirement) {
				resultErr = errors.Join(resultErr, permit.ReleasePart(resourcebudget.Cost{RAM: ramCharge}))
			}
		}
	}()
	f, owner, e = create(path, os.O_RDWR, 0o600)
	if e != nil {
		return nil, e
	}
	if _, e = f.Write(make([]byte, headerBytes)); e != nil {
		return nil, e
	}
	cacheWriter := boundedfilecache.Writer{File: f}
	c := commitment{context: o.Context, extent: headerBytes, root: ref{digest: emptyDigest(o.Context)}}
	if _, e = rand.Read(c.id[:]); e != nil {
		return nil, e
	}
	write := func(raw []byte) (ref, error) {
		if e := ctx.Err(); e != nil {
			return ref{}, e
		}
		if uint64(len(raw)) > uint64(o.MaxDiskBytes)-c.extent {
			return ref{}, ErrLimit
		}
		r := ref{offset: c.extent, length: uint32(len(raw)), digest: sha256.Sum256(raw)}
		n, e := f.Write(raw)
		if e != nil {
			return ref{}, e
		}
		if n != len(raw) {
			return ref{}, io.ErrShortWrite
		}
		c.extent += uint64(n)
		if e := cacheWriter.Advance(int64(c.extent), false); e != nil {
			return ref{}, e
		}
		return r, nil
	}
	writeNode := func(n node) (descriptor, error) {
		raw, e := encodeNode(n, c.extent)
		if e != nil {
			return descriptor{}, e
		}
		r, e := write(raw)
		if e != nil {
			return descriptor{}, e
		}
		lo, hi := n.bounds()
		return descriptor{bytes.Clone(lo), bytes.Clone(hi), n.count, n.height, r}, nil
	}
	var levels [maxHeight][fanout]descriptor
	var sizes [maxHeight]int
	parent := func(children []descriptor) (descriptor, error) {
		var count uint64
		var height uint16
		for _, v := range children {
			if v.count > math.MaxUint64-count {
				return descriptor{}, ErrLimit
			}
			count += v.count
			if v.height > height {
				height = v.height
			}
		}
		return writeNode(node{children: children, count: count, height: height + 1})
	}
	var push func(int, descriptor) error
	push = func(level int, d descriptor) error {
		if level >= maxHeight {
			return ErrLimit
		}
		levels[level][sizes[level]] = d
		sizes[level]++
		if sizes[level] < fanout {
			return nil
		}
		p, e := parent(levels[level][:])
		if e != nil {
			return e
		}
		clear(levels[level][:])
		sizes[level] = 0
		return push(level+1, p)
	}
	var previous []byte
	for {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		row, ok, e := next()
		if e != nil {
			return nil, e
		}
		if !ok {
			break
		}
		if !validKey(row.Key) || len(row.Value) > MaxValueBytes || previous != nil && bytes.Compare(previous, row.Key) >= 0 || c.count == math.MaxUint64 {
			return nil, ErrLimit
		}
		previous = bytes.Clone(row.Key)
		// A leading byte distinguishes the empty value from an absent physical blob.
		blob := make([]byte, len(row.Value)+1)
		copy(blob[1:], row.Value)
		v, e := write(blob)
		if e != nil {
			return nil, e
		}
		d, e := writeNode(node{key: row.Key, value: v, count: 1, height: 1})
		if e != nil {
			return nil, e
		}
		if e = push(0, d); e != nil {
			return nil, e
		}
		c.count++
	}
	for level := 0; level < maxHeight; level++ {
		if sizes[level] == 0 {
			continue
		}
		higher := false
		for i := level + 1; i < maxHeight; i++ {
			higher = higher || sizes[i] != 0
		}
		d := levels[level][0]
		if sizes[level] > 1 {
			d, e = parent(levels[level][:sizes[level]])
			if e != nil {
				return nil, e
			}
		}
		clear(levels[level][:])
		sizes[level] = 0
		if !higher {
			if d.count != c.count {
				return nil, ErrCorrupt
			}
			c.root = d.ref
			c.height = d.height
			break
		}
		if e = push(level+1, d); e != nil {
			return nil, e
		}
	}
	raw, e := c.encode(o.Key)
	if e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	if n, e := f.WriteAt(raw, 0); e != nil || n != len(raw) {
		return nil, errors.Join(e, io.ErrShortWrite)
	}
	if e = cacheWriter.Advance(int64(c.extent), true); e != nil {
		return nil, e
	}
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if e = ownedfile.CloseFile(f); e != nil {
		f = nil
		return nil, e
	}
	f = nil
	if e = durable.SyncDirectory(filepath.Dir(path)); e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	// Admit/reopen the reader before retiring the builder RAM. The disk owner is
	// transferred to the same pinned reader and never refunded by Close alone.
	opened, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	current, e := opened.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if e != nil || pathErr != nil || !os.SameFile(info, current) || !os.SameFile(info, pathInfo) || current.Size() != int64(c.extent) {
		return nil, errors.Join(e, pathErr, ownedfile.CloseFile(opened), ErrCorrupt)
	}
	unusedDisk := o.MaxDiskBytes - int64(c.extent)
	if retainCeiling {
		unusedDisk = 0
	}
	if e = permit.ReleasePart(resourcebudget.Cost{RAM: BuildRAMBytes, Disk: unusedDisk}); e != nil {
		return nil, errors.Join(e, ownedfile.CloseFile(opened))
	}
	ramCharge = 4096
	result = &Snapshot{file: opened, info: info, path: path, current: c, headerDigest: sha256.Sum256(raw), key: bytes.Clone(o.Key), budget: o.Budget, owner: owner, disk: permit}
	failed = false
	return result, nil
}

type Snapshot struct {
	mu                   sync.Mutex
	file                 *os.File
	info                 os.FileInfo
	path                 string
	current              commitment
	headerDigest         [32]byte
	key                  []byte
	budget               *resourcebudget.Manager
	owner                *ownedfile.Owner
	disk                 resourcebudget.Permit
	closeErr, errorState error
	readerRetired        bool
	externalDiskOwner    bool
	discarded            bool
	packHeaderDigest     [32]byte
	packCeiling          int64
	onClose              func() error
	onCorrupt            func(error)
	fault                func() error
}

// Open verifies a fixed authenticated commitment and independently supplied
// context before returning a reader. It borrows immutable persistent bytes;
// their disk charge belongs to the enclosing publisher, not this read handle.
func Open(ctx context.Context, path string, o Options) (s *Snapshot, resultErr error) {
	if e := validateOptions(o); e != nil {
		return nil, e
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	o.Key = bytes.Clone(o.Key)
	defer clear(o.Key)
	permit, e := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: 4096})
	if e != nil {
		return nil, e
	}
	failed := true
	var f *os.File
	defer func() {
		if failed {
			resultErr = errors.Join(resultErr, ownedfile.CloseFile(f))
			if !errors.Is(resultErr, ownedfile.ErrRetirement) {
				resultErr = errors.Join(resultErr, permit.Release())
			}
		}
	}()
	f, e = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() < headerBytes || info.Size() > o.MaxDiskBytes {
		return nil, ErrCorrupt
	}
	var raw [headerBytes]byte
	if _, e = f.ReadAt(raw[:], 0); e != nil {
		return nil, errors.Join(e, ErrCorrupt)
	}
	c, e := decodeCommitment(raw[:], o.Key, o.Context)
	if e != nil || c.extent != uint64(info.Size()) {
		return nil, ErrCorrupt
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	s = &Snapshot{file: f, info: info, path: path, current: c, headerDigest: sha256.Sum256(raw[:]), budget: o.Budget, disk: permit}
	if e = s.checkLocked(); e != nil {
		return nil, e
	}
	s.key = bytes.Clone(o.Key)
	failed = false
	return s, nil
}
func (s *Snapshot) checkLocked() error {
	if s.closeErr != nil {
		return s.closeErr
	}
	if s.file == nil {
		return ErrClosed
	}
	if s.errorState != nil {
		return s.errorState
	}
	if s.fault != nil {
		if e := s.fault(); e != nil {
			return e
		}
	}
	current, e := s.file.Stat()
	path, e2 := os.Lstat(s.path)
	if e != nil || e2 != nil || !current.Mode().IsRegular() || !os.SameFile(s.info, current) || !os.SameFile(s.info, path) || (s.packCeiling == 0 && current.Size() != int64(s.current.extent) || s.packCeiling > 0 && (current.Size() < int64(s.current.extent) || current.Size() > s.packCeiling)) {
		return errors.Join(ErrCorrupt, e, e2)
	}
	var b [headerBytes]byte
	if _, e = s.file.ReadAt(b[:], 0); e != nil || sha256.Sum256(b[:]) != s.physicalHeaderDigest() {
		return errors.Join(ErrCorrupt, e)
	}
	return nil
}
func (s *Snapshot) physicalHeaderDigest() [32]byte {
	if s.packCeiling > 0 {
		return s.packHeaderDigest
	}
	return s.headerDigest
}
func (s *Snapshot) closeLocked() error {
	if s.closeErr != nil {
		return s.closeErr
	}
	if s.file != nil {
		if err := ownedfile.CloseFile(s.file); err != nil {
			s.closeErr = err
			if s.onCorrupt != nil {
				s.onCorrupt(err)
			}
			return s.closeErr
		}
		s.file = nil
		clear(s.key)
	}
	if !s.readerRetired {
		var e error
		if s.owner == nil {
			e = s.disk.Release()
		} else {
			e = s.disk.ReleasePart(resourcebudget.Cost{RAM: 4096})
		}
		s.closeErr = errors.Join(s.closeErr, e)
		if e == nil {
			s.readerRetired = true
			if s.onClose != nil {
				s.closeErr = errors.Join(s.closeErr, s.onClose())
				s.onClose = nil
			}
		}
	}
	return s.closeErr
}
func (s *Snapshot) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.closeLocked() }

// Discard is restricted to the fresh file created by Build, after the caller
// retires every independently reopened reader/root pin. It joins this handle's
// IO only; cross-handle/root pin authority belongs to the enclosing publisher.
// Reopened snapshots have no unlink authority.
func (s *Snapshot) Discard() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == nil {
		return ErrLimit
	}
	if e := s.closeLocked(); e != nil {
		return e
	}
	if s.discarded {
		return nil
	}
	if e := s.owner.Retire(); e != nil {
		return e
	}
	if !s.externalDiskOwner {
		if e := s.disk.Release(); e != nil {
			return e
		}
	}
	s.discarded = true
	return nil
}
