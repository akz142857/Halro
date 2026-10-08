package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type haCheckpointCacheCloseKey struct{}

// An uncertain actual reader/directory Close retains both descriptors, its IO
// permit and the original foundation's source/key owners. It is not a receipt
// granting replacement or a claim that an inactive file was retired.
type haCheckpointCacheInspectionFailure struct {
	files [2]*os.File
	work  resourcebudget.Permit
	cause error
}

func (e *haCheckpointCacheInspectionFailure) Error() string { return e.cause.Error() }
func (e *haCheckpointCacheInspectionFailure) Unwrap() error { return e.cause }

func (c *HASelectedCheckpoint) inspectInactiveCache(ctx context.Context, path string, visit func(*os.File, int64) error) (info os.FileInfo, resultErr error) {
	f := c.owner
	work, err := f.owners.Components.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 64 << 10})
	if err != nil {
		return nil, err
	}
	debt := &haCheckpointCacheInspectionFailure{work: work}
	closeFile := ownedfile.CloseFile
	if closeActual, ok := ctx.Value(haCheckpointCacheCloseKey{}).(func(*os.File) error); ok {
		closeFile = func(file *os.File) error { return ownedfile.CloseError(file, closeActual(file)) }
	}
	defer func() {
		for i, file := range debt.files {
			if file != nil {
				if e := closeFile(file); e != nil {
					resultErr = errors.Join(resultErr, e)
				} else {
					debt.files[i] = nil
				}
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			debt.cause = resultErr
			f.mu.Lock()
			f.closeErr = errors.Join(f.closeErr, debt)
			f.mu.Unlock()
			info = nil
			resultErr = debt
			return
		}
		if e := work.Release(); e != nil {
			debt.cause = errors.Join(resultErr, e)
			f.mu.Lock()
			f.closeErr = errors.Join(f.closeErr, debt)
			f.mu.Unlock()
			info = nil
			resultErr = debt
		}
	}()
	named, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() || named.Mode().Perm()&0077 != 0 {
		return nil, ownedfile.ErrReplaced
	}
	debt.files[1], err = os.OpenFile(filepath.Dir(path), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	parent, err := debt.files[1].Stat()
	if err != nil {
		return nil, err
	}
	debt.files[0], err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	actual, err := debt.files[0].Stat()
	if err != nil || !os.SameFile(named, actual) || actual.Size() != named.Size() {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := visit(debt.files[0], actual.Size()); err != nil {
		return nil, err
	}
	after, err := debt.files[0].Stat()
	if err != nil {
		return nil, err
	}
	name, nameErr := os.Lstat(path)
	parentName, parentErr := os.Lstat(filepath.Dir(path))
	parentFD, fdErr := debt.files[1].Stat()
	if err := errors.Join(nameErr, parentErr, fdErr); err != nil {
		return nil, err
	}
	if !os.SameFile(actual, after) || !os.SameFile(actual, name) || after.Size() != actual.Size() || name.Size() != actual.Size() || !after.ModTime().Equal(actual.ModTime()) || !name.ModTime().Equal(actual.ModTime()) || !os.SameFile(parent, parentName) || !os.SameFile(parent, parentFD) {
		return nil, ownedfile.ErrReplaced
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return actual, nil
}
