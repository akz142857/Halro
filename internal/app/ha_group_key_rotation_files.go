package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// These actual creators remain reachable in the group command owner, including
// an uncertain write/Sync/Close. Names are fixed intent-slot children; neither
// a restart's familiar filename nor an intent checksum can mint an unlink owner.
type haGroupRotationFile struct {
	file, parent             *os.File
	creator                  *ownedfile.Owner
	parentInfo               os.FileInfo
	work, disk               resourcebudget.Permit
	debt                     error
	fileJoined, parentJoined bool
	closeFD                  func(*os.File) error
}
type haGroupRotationFileFailure struct {
	Owner *haGroupRotationFile
	Cause error
}

func (e *haGroupRotationFileFailure) Error() string { return e.Cause.Error() }
func (e *haGroupRotationFileFailure) Unwrap() error { return e.Cause }

func createHAGroupRotationFile(ctx context.Context, path string, raw []byte, manager *resourcebudget.Manager) (*haGroupRotationFile, error) {
	return createHAGroupRotationFileWithClose(ctx, path, raw, manager, ownedfile.CloseFile)
}
func createHAGroupRotationFileWithClose(ctx context.Context, path string, raw []byte, manager *resourcebudget.Manager, closeFD func(*os.File) error) (result *haGroupRotationFile, resultErr error) {
	return createHAGroupRotationFileWithDisk(ctx, path, raw, manager, closeFD, resourcebudget.Permit{})
}

func createHAGroupRotationFileWithDisk(ctx context.Context, path string, raw []byte, manager *resourcebudget.Manager, closeFD func(*os.File) error, disk resourcebudget.Permit) (result *haGroupRotationFile, resultErr error) {
	if manager == nil || len(raw) == 0 || len(raw) > haGroupRotationIntentLimit {
		return nil, authenticatedstate.ErrLimit
	}
	x := &haGroupRotationFile{closeFD: closeFD, disk: disk}
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				x.debt = errors.Join(x.debt, resultErr)
			}
			resultErr = &haGroupRotationFileFailure{Owner: x, Cause: errors.Join(resultErr, x.Close())}
			result = x
		}
	}()
	var err error
	if x.disk == (resourcebudget.Permit{}) {
		x.disk, err = manager.TryAcquireRecovery(resourcebudget.Cost{Disk: haGroupRotationIntentLimit})
		if err != nil {
			return x, err
		}
	}
	x.work, err = haGroupRotationIntentWorkspace(ctx, manager)
	if err != nil {
		return x, err
	}
	x.parent, err = os.OpenFile(filepath.Dir(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return x, err
	}
	x.parentInfo, err = x.parent.Stat()
	if err != nil || !x.parentInfo.IsDir() {
		return x, errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := ctx.Err(); err != nil {
		return x, err
	}
	x.file, x.creator, err = ownedfile.Create(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return x, err
	}
	if n, err := x.file.Write(raw); err != nil || n != len(raw) {
		x.debt = errors.Join(ownedfile.ErrRetirement, err, io.ErrShortWrite)
		return x, x.debt
	}
	if err := x.file.Sync(); err != nil {
		x.debt = errors.Join(ownedfile.ErrRetirement, err)
		return x, x.debt
	}
	if err := x.creator.VerifyFile(x.file); err != nil {
		return x, err
	}
	if info, err := x.file.Stat(); err != nil || info.Size() != int64(len(raw)) {
		return x, errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := x.parent.Sync(); err != nil {
		x.debt = errors.Join(ownedfile.ErrRetirement, err)
		return x, x.debt
	}
	if err := x.Close(); err != nil {
		return x, err
	}
	return x, nil
}

func (x *haGroupRotationFile) Close() error {
	if x == nil {
		return nil
	}
	if !x.fileJoined && x.file != nil {
		x.fileJoined = true
		if err := x.closeActual(x.file); err != nil {
			x.debt = errors.Join(x.debt, ownedfile.ErrRetirement, err)
		} else {
			x.file = nil
		}
	}
	if !x.parentJoined && x.parent != nil {
		x.parentJoined = true
		if err := x.closeActual(x.parent); err != nil {
			x.debt = errors.Join(x.debt, ownedfile.ErrRetirement, err)
		} else {
			x.parent = nil
		}
	}
	if x.debt != nil {
		return x.debt
	}
	if x.work != (resourcebudget.Permit{}) {
		if err := x.work.Release(); err != nil {
			x.debt = errors.Join(x.debt, err)
			return x.debt
		}
		x.work = resourcebudget.Permit{}
	}
	if x.creator == nil && x.disk != (resourcebudget.Permit{}) {
		if err := x.disk.Release(); err != nil {
			x.debt = errors.Join(x.debt, err)
			return x.debt
		}
		x.disk = resourcebudget.Permit{}
	}
	// Persistent or partially created bytes remain. Their declared ceiling is
	// retained in the SAME Manager; a successful Close is not Disk retirement.
	return nil
}

func (x *haGroupRotationFile) publishCaptured(ctx context.Context, destination string, before os.FileInfo) (resultErr error) {
	if x == nil {
		return ownedfile.ErrRetirement
	}
	if x.debt != nil || x.creator == nil || !x.fileJoined || !x.parentJoined || x.file != nil || x.parent != nil || before == nil {
		return errors.Join(ownedfile.ErrRetirement, x.debt)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := os.OpenFile(filepath.Dir(destination), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	x.parent = parent
	x.parentJoined = false
	defer func() { resultErr = errors.Join(resultErr, x.Close()) }()
	actual, err := parent.Stat()
	named, nameErr := os.Lstat(filepath.Dir(destination))
	if err != nil || nameErr != nil || !os.SameFile(x.parentInfo, actual) || !os.SameFile(x.parentInfo, named) {
		return errors.Join(ownedfile.ErrReplaced, err, nameErr)
	}
	if err := x.creator.ReplaceCaptured(destination, before); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		x.debt = errors.Join(ownedfile.ErrRetirement, err)
		return x.debt
	}
	return x.creator.VerifyName()
}

func (x *haGroupRotationFile) closeActual(f *os.File) error {
	if x.closeFD != nil {
		return x.closeFD(f)
	}
	return ownedfile.CloseFile(f)
}
