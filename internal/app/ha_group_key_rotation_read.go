package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
)

type haGroupRotationReadFailure struct {
	Cause error
	files [2]*os.File
	raw   []byte
}

func (e *haGroupRotationReadFailure) Error() string { return e.Cause.Error() }
func (e *haGroupRotationReadFailure) Unwrap() error { return e.Cause }

// The caller owns the finite intent workspace for the whole read and decoder.
// No bytes escape until both original descriptors join and the named file and
// parent still identify the same actual namespace and exact length.
func readHAGroupRotationBytes(path string) ([]byte, os.FileInfo, error) {
	return readHAGroupRotationBytesWithClose(path, ownedfile.CloseFile)
}
func readHAGroupRotationBytesWithClose(path string, closeFD func(*os.File) error) (raw []byte, captured os.FileInfo, resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > haGroupRotationIntentLimit {
		return nil, nil, authenticatedstate.ErrCorrupt
	}
	owner := &haGroupRotationReadFailure{}
	defer func() {
		for n := 1; n >= 0; n-- {
			if owner.files[n] == nil {
				continue
			}
			if err := closeFD(owner.files[n]); err != nil {
				owner.Cause = errors.Join(owner.Cause, ownedfile.ErrRetirement, err)
			} else {
				owner.files[n] = nil
			}
		}
		if owner.Cause != nil {
			resultErr = &haGroupRotationReadFailure{Cause: errors.Join(resultErr, owner.Cause), files: owner.files, raw: owner.raw}
		}
		if resultErr != nil {
			raw, captured = nil, nil
			if owner.Cause == nil {
				clear(owner.raw)
			}
		}
	}()
	owner.files[0], err = os.OpenFile(filepath.Dir(path), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	parent, err := owner.files[0].Stat()
	if err != nil || !parent.IsDir() {
		return nil, nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	owner.files[1], err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	actual, err := owner.files[1].Stat()
	if err != nil || !sameHAGroupReadFile(info, actual) {
		return nil, nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	owner.raw = make([]byte, int(info.Size()))
	if _, err := io.ReadFull(owner.files[1], owner.raw); err != nil {
		return nil, nil, err
	}
	var extra [1]byte
	if n, err := owner.files[1].Read(extra[:]); n != 0 || err != io.EOF {
		return nil, nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	actual, err = owner.files[1].Stat()
	named, nameErr := os.Lstat(path)
	parentActual, parentErr := owner.files[0].Stat()
	parentNamed, parentNameErr := os.Lstat(filepath.Dir(path))
	if err != nil || nameErr != nil || parentErr != nil || parentNameErr != nil || !sameHAGroupReadFile(info, actual) || !sameHAGroupReadFile(info, named) || !os.SameFile(parent, parentActual) || !os.SameFile(parent, parentNamed) || !parentNamed.IsDir() || parentNamed.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.Join(ownedfile.ErrReplaced, err, nameErr, parentErr, parentNameErr)
	}
	return owner.raw, actual, nil
}
func sameHAGroupReadFile(before, after os.FileInfo) bool {
	return before != nil && after != nil && after.Mode().IsRegular() && after.Mode().Perm() == before.Mode().Perm() && after.Mode().Perm()&0077 == 0 && os.SameFile(before, after) && after.Size() == before.Size() && after.ModTime().Equal(before.ModTime())
}
