package governance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

const StartupCachePersistRAMBytes int64 = governancelimits.StartupCachePersistRAMBytes

var ErrStartupCachePersistence = errors.New("governance startup cache persistence is uncertain")

type StartupCachePersistOptions struct {
	Guard             *lock.Lock
	Budget            *resourcebudget.Manager
	DiskReservation   resourcebudget.Permit
	Identity          [32]byte
	Head              Summary
	PhysicalCandidate *resourcebudget.PhysicalCandidate
}

// StartupCacheFile is the original creation receipt for one immutable unique
// candidate file. It owns its disk charge until Discard; reopening a capsule
// through ReadStartupCache grants no deletion authority. Persistence does not
// activate this candidate: common seal selection must still bind its exact head.
type StartupCacheFile struct {
	mu                  sync.Mutex
	path                string
	identity            [32]byte
	head                Summary
	owner               *ownedfile.Owner
	file, directory     *os.File
	disk, work          resourcebudget.Permit
	err                 error
	discarded           bool
	closed              bool
	reservationReturned bool
	ctx                 context.Context
	cancel              context.CancelFunc
	budget              *resourcebudget.Manager
	guard               *lock.Lock
	digest              [32]byte
	retainedCache       *StartupCache
	readOnly            bool
}

func (f *StartupCacheFile) Path() string {
	if f == nil {
		return ""
	}
	return f.path
}
func (f *StartupCacheFile) Head() Summary {
	if f == nil {
		return Summary{}
	}
	return f.head
}
func (f *StartupCacheFile) Identity() [32]byte {
	if f == nil {
		return [32]byte{}
	}
	return f.identity
}
func (f *StartupCacheFile) Close() error {
	if f == nil {
		return nil
	}
	if f.cancel != nil {
		f.cancel()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.readOnly && f.err == nil && !f.reservationReturned {
		if err := f.disk.Release(); err != nil {
			f.err = err
			return err
		}
		f.reservationReturned = true
	}
	return f.err
}

// CloseWithPersistentReservation joins this candidate's loans and returns its
// retained reservation to the same caller-owned fixed slot budget. The parent
// must remain live while any file survives; closing a reader alone never refunds
// persistent bytes. A failed physical retirement retains the original loan.
func (f *StartupCacheFile) CloseWithPersistentReservation(parent resourcebudget.Permit) error {
	if f == nil {
		return nil
	}
	witness, err := parent.RetainCovering(f.budget, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		return err
	}
	if err := witness.Release(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reservationReturned {
		return nil
	}
	if err := f.disk.Release(); err != nil {
		f.err = err
		return err
	}
	f.reservationReturned = true
	return nil
}

func (f *StartupCacheFile) Discard() error {
	if f == nil {
		return nil
	}
	if f.readOnly {
		return ErrStartupCache
	}
	if f.cancel != nil {
		f.cancel()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.discarded {
		return nil
	}
	if err := f.owner.Retire(); err != nil {
		f.err = err
		return err
	}
	if err := durable.SyncDirectory(filepath.Dir(f.path)); err != nil {
		f.err = errors.Join(ErrStartupCachePersistence, err)
		return f.err
	}
	if !f.reservationReturned {
		if err := f.disk.Release(); err != nil {
			f.err = err
			return err
		}
		f.reservationReturned = true
	}
	f.discarded = true
	return nil
}

// PersistNew writes only a never-before-existing candidate path, Syncs the
// actual owned file and parent directory, and closes both descriptors before
// acknowledging persistence. The same Manager must preallocate the complete
// maximum disk extent. Known rejection cleans only the actual created inode;
// unknown FD/barrier retirement retains the receipt, descriptors and whole IO
// fee. It never overwrites a current capsule or deletes a reader's source.
func (c *StartupCache) PersistNew(ctx context.Context, path string, o StartupCachePersistOptions) (*StartupCacheFile, error) {
	return c.persistNew(ctx, path, o, nil)
}

func (c *StartupCache) persistNew(ctx context.Context, path string, o StartupCachePersistOptions, phase func(string, *os.File) error) (result *StartupCacheFile, resultErr error) {
	if c == nil || o.Guard == nil || o.Budget == nil || o.Budget != c.budget || o.Identity == ([32]byte{}) || o.Identity != c.identity || o.Head != c.head || path == "" || !c.mu.TryLock() {
		return nil, ErrStartupCache
	}
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil || sha256.Sum256(c.raw) != c.encodedDigest {
		return nil, errors.Join(ErrStartupCache, c.err, c.ctx.Err())
	}
	jobCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	work, err := o.Budget.TryAcquireContext(jobCtx, resourcebudget.IO, resourcebudget.Cost{RAM: StartupCachePersistRAMBytes})
	if err != nil {
		return nil, err
	}
	disk, err := o.DiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		return nil, errors.Join(err, work.Release())
	}
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	result = &StartupCacheFile{identity: c.identity, head: c.head, disk: disk, work: work, ctx: ownerCtx, cancel: ownerCancel, budget: o.Budget, guard: o.Guard, digest: c.encodedDigest}
	defer func() {
		var closeErr error
		if result.file != nil {
			closeFile := func() error { return ownedfile.CloseFile(result.file) }
			if o.PhysicalCandidate != nil {
				closeFile = o.PhysicalCandidate.CloseFile
			}
			if err := closeFile(); err != nil {
				closeErr = errors.Join(closeErr, err)
			} else {
				result.file = nil
			}
		}
		if result.directory != nil {
			if err := ownedfile.CloseFile(result.directory); err != nil {
				closeErr = errors.Join(closeErr, err)
			} else {
				result.directory = nil
			}
		}
		resultErr = errors.Join(resultErr, closeErr)
		if o.PhysicalCandidate != nil {
			if resultErr == nil {
				resultErr = o.PhysicalCandidate.Published(o.Budget, result.disk)
			}
			if resultErr != nil {
				result.err = errors.Join(ErrStartupCachePersistence, resultErr)
				resultErr = result.err
				return
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrStartupCachePersistence) {
			result.err = resultErr
			return
		}
		if resultErr != nil && result.owner != nil {
			if err := result.owner.Retire(); err != nil {
				resultErr = errors.Join(resultErr, err)
				result.err = resultErr
				return
			}
			if err := durable.SyncDirectory(filepath.Dir(result.path)); err != nil {
				resultErr = errors.Join(resultErr, ErrStartupCachePersistence, err)
				result.err = resultErr
				return
			}
		}
		if err := work.Release(); err != nil {
			resultErr = errors.Join(resultErr, err)
			result.err = resultErr
			return
		}
		result.work = resourcebudget.Permit{}
		if resultErr != nil {
			if err := disk.Release(); err != nil {
				resultErr = errors.Join(resultErr, err)
				result.err = resultErr
				return
			}
			result = nil
		}
	}()
	absolute, err := filepath.Abs(path)
	if err != nil {
		return result, err
	}
	result.path = absolute
	parent := filepath.Dir(absolute)
	if !o.Guard.CoversDataDirectory(parent) {
		return result, ErrStartupCache
	}
	result.directory, err = os.OpenFile(parent, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	parentInfo, err := result.directory.Stat()
	if err != nil {
		return result, err
	}
	namedParent, err := os.Lstat(parent)
	if err != nil || !namedParent.IsDir() || !os.SameFile(parentInfo, namedParent) {
		return result, errors.Join(ownedfile.ErrReplaced, err)
	}
	if o.PhysicalCandidate != nil {
		if err := o.PhysicalCandidate.ValidatePrepared(absolute, MaxStartupCacheBytes, o.Budget); err != nil {
			return result, err
		}
		result.file, result.owner, err = o.PhysicalCandidate.ClaimForWrite(absolute, MaxStartupCacheBytes, int64(len(c.raw)))
	} else {
		result.file, result.owner, err = ownedfile.Create(absolute, os.O_RDWR, 0600)
	}
	if err != nil {
		return result, err
	}
	if err := writeFull(startupContextWriter{jobCtx, result.file}, c.raw); err != nil {
		return result, err
	}
	if phase != nil {
		if err := phase("before-file-sync", result.file); err != nil {
			return result, err
		}
	}
	if err := result.file.Sync(); err != nil {
		return result, errors.Join(ErrStartupCachePersistence, err)
	}
	if err := result.owner.VerifyFile(result.file); err != nil {
		return result, err
	}
	info, err := result.file.Stat()
	if err != nil || info.Size() != int64(len(c.raw)) {
		return result, errors.Join(ErrStartupCache, err)
	}
	reader := io.NewSectionReader(result.file, 0, int64(len(c.raw)))
	buffer := make([]byte, 64<<10)
	for offset := 0; offset < len(c.raw); {
		if err := jobCtx.Err(); err != nil {
			return result, err
		}
		n := len(c.raw) - offset
		if n > len(buffer) {
			n = len(buffer)
		}
		if _, err := io.ReadFull(reader, buffer[:n]); err != nil {
			return result, err
		}
		if !bytes.Equal(buffer[:n], c.raw[offset:offset+n]) {
			return result, ErrStartupCache
		}
		offset += n
	}
	if phase != nil {
		if err := phase("before-directory-sync", result.directory); err != nil {
			return result, err
		}
	}
	if err := result.directory.Sync(); err != nil {
		return result, errors.Join(ErrStartupCachePersistence, err)
	}
	if err := result.owner.VerifyName(); err != nil {
		return result, err
	}
	currentParent, err := os.Lstat(parent)
	if err != nil || !currentParent.IsDir() || !os.SameFile(parentInfo, currentParent) || !o.Guard.CoversDataDirectory(parent) {
		return result, errors.Join(ownedfile.ErrReplaced, err)
	}
	if phase != nil {
		if err := phase("before-close", result.file); err != nil {
			return result, err
		}
	}
	return result, nil
}

// ReplacePendingCandidate moves only this exclusively created pending inode
// over a separately authenticated inactive capsule. The caller must first
// qualify that destination against the independently selected common seal.
// This primitive does not select, activate or grant ownership of the old file.
func (f *StartupCacheFile) ReplacePendingCandidate(ctx context.Context, destination string, previous os.FileInfo) error {
	if f == nil || previous == nil {
		return ErrStartupCache
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.readOnly || f.closed || f.discarded || f.err != nil || f.owner == nil || f.path != destination+".pending" || !f.guard.CoversDataDirectory(filepath.Dir(destination)) {
		return ErrStartupCache
	}
	if err := f.owner.ReplaceCaptured(destination, previous); err != nil {
		return err
	}
	f.path = destination
	if err := durable.SyncDirectory(filepath.Dir(destination)); err != nil {
		f.err = errors.Join(ErrStartupCachePersistence, err)
		return f.err
	}
	return f.owner.VerifyName()
}
