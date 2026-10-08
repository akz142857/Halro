package audit

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

// WithFrozenOwner verifies the persisted creation receipt against an already
// frozen actual native owner. The caller's callback remains inside this file
// loan; Close cancels and joins it, and Discard cannot retire its inode early.
// It does not enter the native freeze again or grant the reader deletion rights.
func (f *StartupCacheFile) WithFrozenOwner(ctx context.Context, native *Log, budget *resourcebudget.Manager, identity [32]byte, head Summary, callback func(context.Context) error) error {
	return f.withFrozenOwner(ctx, native, budget, identity, head, callback, nil)
}

func (f *StartupCacheFile) withFrozenOwner(ctx context.Context, native *Log, budget *resourcebudget.Manager, identity [32]byte, head Summary, callback func(context.Context) error, phase func(*os.File)) (resultErr error) {
	if f == nil || native == nil || callback == nil || budget == nil {
		return ErrStartupCache
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.discarded || f.err != nil || f.ctx.Err() != nil || f.budget != budget || f.identity != identity || f.head != head || f.owner == nil || !f.guard.CoversDataDirectory(filepath.Dir(f.path)) {
		return errors.Join(ErrStartupCache, f.err)
	}
	jobCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	work, err := budget.TryAcquireContext(jobCtx, resourcebudget.IO, resourcebudget.Cost{RAM: StartupCachePersistRAMBytes})
	if err != nil {
		return err
	}
	var file *os.File
	var capsule *StartupCache
	defer func() {
		if file != nil {
			resultErr = errors.Join(resultErr, ownedfile.CloseFile(file))
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			f.file, f.retainedCache, f.work, f.err = file, capsule, work, resultErr
			return
		}
		if capsule != nil {
			resultErr = errors.Join(resultErr, capsule.Close())
		}
		if err := work.Release(); err != nil {
			f.work, f.err = work, errors.Join(resultErr, err)
			resultErr = f.err
		}
	}()
	var key [32]byte
	native.mu.Lock()
	valid := native.freezeCancel != nil && !native.closing && native.file != nil && native.terminalErr == nil && native.cacheBudget == budget && native.cacheOwned && (Summary{Records: native.sequence, Bytes: native.offset, LastHash: native.lastHash}) == head && len(native.key) == len(key)
	if valid {
		copy(key[:], native.key)
	}
	native.mu.Unlock()
	defer clear(key[:])
	if !valid {
		return ErrStartupCache
	}
	if err := f.owner.VerifyName(); err != nil {
		return err
	}
	file, err = os.OpenFile(f.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	if err := f.owner.VerifyFile(file); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	capsule, err = ReadStartupCache(jobCtx, io.NewSectionReader(file, 0, info.Size()), info.Size(), key[:], identity, budget)
	if err != nil {
		return err
	}
	if capsule.encodedDigest != f.digest {
		return ErrStartupCache
	}
	if err := capsule.ValidateFrozenOwner(jobCtx, native, budget, identity, head); err != nil {
		return err
	}
	if err := callback(jobCtx); err != nil {
		return err
	}
	if err := jobCtx.Err(); err != nil {
		return err
	}
	// Validate actual bytes again after publication, without a second capsule
	// allocation; a callback cannot turn an earlier file check into stale proof.
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	r := io.NewSectionReader(file, 0, info.Size())
	for remaining := info.Size(); remaining > 0; {
		if err := jobCtx.Err(); err != nil {
			return err
		}
		n := int64(len(buffer))
		if remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(r, buffer[:n]); err != nil {
			return err
		}
		_, _ = h.Write(buffer[:n])
		remaining -= n
	}
	if string(h.Sum(nil)) != string(f.digest[:]) {
		return ErrStartupCache
	}
	current, err := file.Stat()
	if err != nil || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return errors.Join(ErrStartupCache, err)
	}
	if err := f.owner.VerifyFile(file); err != nil {
		return err
	}
	if err := f.owner.VerifyName(); err != nil {
		return err
	}
	if err := capsule.ValidateFrozenOwner(jobCtx, native, budget, identity, head); err != nil {
		return err
	}
	if phase != nil {
		phase(file)
	}
	return nil
}
