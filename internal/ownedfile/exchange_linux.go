//go:build linux

package ownedfile

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func exchangeCapturedPinned(from, to string, owner *Owner, previous os.FileInfo) (other *Owner, moved bool, resultErr error) {
	parent, parentInfo, err := openPinnedParent(from)
	if err != nil {
		return nil, false, err
	}
	var source, destination *os.File
	defer func() {
		resultErr = errors.Join(resultErr, CloseFile(source), CloseFile(destination), CloseFile(parent))
	}()
	if !os.SameFile(parentInfo, owner.parent) {
		return nil, false, ErrReplaced
	}
	open := func(path string) (*os.File, error) {
		fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	source, err = open(from)
	if err != nil {
		return nil, false, err
	}
	sourceInfo, err := source.Stat()
	if err != nil || !sourceInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, owner.file) {
		return nil, false, errors.Join(ErrReplaced, err)
	}
	destination, err = open(to)
	if err != nil {
		return nil, false, err
	}
	destinationInfo, err := destination.Stat()
	if err != nil || !destinationInfo.Mode().IsRegular() || !os.SameFile(destinationInfo, previous) || os.SameFile(sourceInfo, destinationInfo) {
		return nil, false, errors.Join(ErrReplaced, err)
	}
	namedSource, err := os.Lstat(from)
	if err != nil || !os.SameFile(namedSource, sourceInfo) {
		return nil, false, errors.Join(ErrReplaced, err)
	}
	namedDestination, err := os.Lstat(to)
	if err != nil || !os.SameFile(namedDestination, destinationInfo) {
		return nil, false, errors.Join(ErrReplaced, err)
	}
	if err := owner.checkParent(); err != nil {
		return nil, false, err
	}
	if err := unix.Renameat2(int(parent.Fd()), filepath.Base(from), int(parent.Fd()), filepath.Base(to), unix.RENAME_EXCHANGE); err != nil {
		return nil, false, err
	}
	moved = true
	other = &Owner{path: from, file: destinationInfo, parent: parentInfo}
	namedSource, err = os.Lstat(from)
	if err != nil || !os.SameFile(namedSource, destinationInfo) {
		return other, true, errors.Join(ErrRetirement, ErrReplaced, err)
	}
	namedDestination, err = os.Lstat(to)
	if err != nil || !os.SameFile(namedDestination, sourceInfo) {
		return other, true, errors.Join(ErrRetirement, ErrReplaced, err)
	}
	if err := owner.checkParent(); err != nil {
		return other, true, errors.Join(ErrRetirement, err)
	}
	return other, true, nil
}
