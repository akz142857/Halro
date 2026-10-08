//go:build linux || darwin

package ownedfile

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

type renameRetirementFailure struct {
	Cause        error
	parent, file *os.File
}

func (e *renameRetirementFailure) Error() string { return e.Cause.Error() }
func (e *renameRetirementFailure) Unwrap() error { return e.Cause }

func renameCapturedPinned(from, to string, o *Owner) (moved bool, resultErr error) {
	parent, info, err := openPinnedParent(from)
	if err != nil {
		return false, err
	}
	debt := &renameRetirementFailure{}
	var file *os.File
	defer func() {
		if file != nil {
			if err := CloseFile(file); err != nil {
				debt.file = file
				debt.Cause = errors.Join(debt.Cause, err)
			}
		}
		if err := CloseFile(parent); err != nil {
			debt.parent = parent
			debt.Cause = errors.Join(debt.Cause, err)
		}
		if debt.Cause != nil {
			debt.Cause = errors.Join(ErrRetirement, resultErr, debt.Cause)
			resultErr = debt
		}
	}()
	if !os.SameFile(o.parent, info) {
		return false, ErrReplaced
	}
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(from), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	file = os.NewFile(uintptr(fd), from)
	actual, err := file.Stat()
	if err != nil || !os.SameFile(actual, o.file) {
		return false, errors.Join(ErrReplaced, err)
	}
	if err := o.checkParent(); err != nil {
		return false, err
	}
	named, err := os.Lstat(from)
	if err != nil || !os.SameFile(named, actual) {
		return false, errors.Join(ErrReplaced, err)
	}
	if err := renameExclusiveAt(int(parent.Fd()), filepath.Base(from), filepath.Base(to)); err != nil {
		return false, err
	}
	moved = true
	named, err = os.Lstat(to)
	if err != nil || !os.SameFile(named, actual) {
		return true, errors.Join(ErrRetirement, ErrReplaced, err)
	}
	return true, nil
}
