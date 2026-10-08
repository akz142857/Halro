package governance

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ReuseFrozenFile lends an existing immutable capsule only after its complete
// authenticated bytes match this cache and this actual frozen native owner.
// The resulting reader cannot discard or replace the file. Its retained Disk
// loan returns to the original fixed-slot parent on a fully joined Close.
func (c *StartupCache) ReuseFrozenFile(ctx context.Context, native *Log, path string, o StartupCachePersistOptions) (result *StartupCacheFile, resultErr error) {
	if c == nil || o.Guard == nil || o.Budget == nil || o.PhysicalCandidate != nil {
		return nil, ErrStartupCache
	}
	if err := c.ValidateFrozenOwner(ctx, native, o.Budget, o.Identity, o.Head); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil || c.err != nil || ctx.Err() != nil {
		return nil, ErrStartupCache
	}
	absolute, err := filepath.Abs(path)
	if err != nil || !o.Guard.CoversDataDirectory(filepath.Dir(absolute)) {
		return nil, errors.Join(ErrStartupCache, err)
	}
	disk, err := o.DiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		return nil, err
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: StartupCachePersistRAMBytes})
	if err != nil {
		return nil, errors.Join(err, disk.Release())
	}
	ownerCtx, cancel := context.WithCancel(context.Background())
	result = &StartupCacheFile{path: absolute, identity: c.identity, head: c.head, disk: disk, work: work, ctx: ownerCtx, cancel: cancel, budget: o.Budget, guard: o.Guard, digest: c.encodedDigest, readOnly: true}
	defer func() {
		var closeErr error
		if result.file != nil {
			if e := ownedfile.CloseFile(result.file); e != nil {
				closeErr = errors.Join(closeErr, e)
			} else {
				result.file = nil
			}
		}
		if result.directory != nil {
			if e := ownedfile.CloseFile(result.directory); e != nil {
				closeErr = errors.Join(closeErr, e)
			} else {
				result.directory = nil
			}
		}
		resultErr = errors.Join(resultErr, closeErr)
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			result.retainedCache, result.err = c, resultErr
			return
		}
		if e := work.Release(); e != nil {
			result.err = errors.Join(resultErr, e)
			resultErr = result.err
			return
		}
		result.work = resourcebudget.Permit{}
		if resultErr != nil {
			cancel()
			if e := disk.Release(); e != nil {
				result.err = errors.Join(resultErr, e)
				resultErr = result.err
				return
			}
			result = nil
		}
	}()
	named, err := os.Lstat(absolute)
	if err != nil || !named.Mode().IsRegular() || named.Mode().Perm()&0077 != 0 || named.Size() != int64(len(c.raw)) {
		return result, errors.Join(ErrStartupCache, err)
	}
	result.directory, err = os.OpenFile(filepath.Dir(absolute), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	parent, err := result.directory.Stat()
	if err != nil {
		return result, err
	}
	result.file, err = os.OpenFile(absolute, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	actual, err := result.file.Stat()
	if err != nil || !os.SameFile(named, actual) || actual.Size() != named.Size() {
		return result, errors.Join(ownedfile.ErrReplaced, err)
	}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	reader := io.NewSectionReader(result.file, 0, actual.Size())
	for remaining := actual.Size(); remaining > 0; {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n := int64(len(buffer))
		if remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(reader, buffer[:n]); err != nil {
			return result, err
		}
		_, _ = h.Write(buffer[:n])
		remaining -= n
	}
	if string(h.Sum(nil)) != string(c.encodedDigest[:]) {
		return result, ErrStartupCache
	}
	current, err := result.file.Stat()
	if err != nil || current.Size() != actual.Size() || !current.ModTime().Equal(actual.ModTime()) {
		return result, errors.Join(ownedfile.ErrReplaced, err)
	}
	result.owner, err = ownedfile.RestoreAuthenticated(absolute, result.file, actual, parent)
	if err != nil {
		return result, err
	}
	return result, errors.Join(ctx.Err(), c.ctx.Err())
}
