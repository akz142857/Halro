//go:build linux || darwin

package ownedfile

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openPinnedParent(path string) (*os.File, os.FileInfo, error) {
	directory := filepath.Dir(path)
	named, err := os.Lstat(directory)
	if err != nil || !named.IsDir() {
		return nil, nil, errors.Join(ErrReplaced, err)
	}
	parent, err := os.OpenFile(directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	actual, err := parent.Stat()
	if err != nil || !os.SameFile(named, actual) {
		closeErr := CloseFile(parent)
		if closeErr != nil {
			closeErr = errors.Join(ErrRetirement, closeErr)
		}
		return nil, nil, errors.Join(ErrReplaced, err, closeErr)
	}
	return parent, actual, nil
}

// Actual creation is relative to the captured parent FD, so a swapped ancestor
// cannot redirect the writer. The transient parent descriptor is closed before
// returning ownership; it never becomes a per-file retained FD registry.
func createPinned(path string, flags int, mode os.FileMode, phase func(*os.File, *os.File) error) (result *os.File, receipt *Owner, resultErr error) {
	parent, identity, err := openPinnedParent(path)
	if err != nil {
		return nil, nil, err
	}
	var file *os.File
	var owner *Owner
	defer func() {
		if err := CloseFile(parent); err != nil {
			resultErr = errors.Join(resultErr, ErrRetirement, err)
		}
		if resultErr != nil && file != nil {
			closeErr := CloseFile(file)
			resultErr = errors.Join(resultErr, closeErr)
			if closeErr != nil {
				resultErr = errors.Join(resultErr, ErrRetirement)
			} else if owner != nil {
				if err := owner.Retire(); err != nil {
					resultErr = errors.Join(resultErr, ErrRetirement, err)
				}
			}
			result, receipt = nil, nil
		}
	}()
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), flags|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, nil, err
	}
	file = os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(ErrRetirement, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, ErrReplaced
	}
	owner = &Owner{path: path, file: info, parent: identity}
	if phase != nil {
		if err := phase(parent, file); err != nil {
			return nil, nil, err
		}
	}
	if err := owner.VerifyFile(file); err != nil {
		return nil, nil, err
	}
	return file, owner, nil
}

// The caller supplies directory exclusion and joins every other physical IO
// owner first. The final comparison and unlink use one captured directory FD.
// This does not create a portable atomic inode-conditional unlink against an
// uncooperating writer; that remains the directory exclusion contract.
func retirePinned(path string, expected, parentInfo os.FileInfo, phase func(*os.File, *os.File) error) (retired bool, resultErr error) {
	if expected == nil || parentInfo == nil || !expected.Mode().IsRegular() {
		return false, ErrReplaced
	}
	parent, actualParent, err := openPinnedParent(path)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := CloseFile(parent); err != nil {
			resultErr = errors.Join(resultErr, ErrRetirement, err)
		}
	}()
	if !os.SameFile(parentInfo, actualParent) {
		return false, ErrReplaced
	}
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, errors.Join(ErrReplaced, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if err := CloseFile(file); err != nil {
			resultErr = errors.Join(resultErr, ErrRetirement, err)
		}
	}()
	actual, err := file.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		return false, errors.Join(ErrReplaced, err)
	}
	if phase != nil {
		if err := phase(parent, file); err != nil {
			return false, err
		}
	}
	var named, descriptor unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), filepath.Base(path), &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, errors.Join(ErrReplaced, err)
	}
	if err := unix.Fstat(int(file.Fd()), &descriptor); err != nil {
		return false, err
	}
	if named.Mode&unix.S_IFMT != unix.S_IFREG || named.Dev != descriptor.Dev || named.Ino != descriptor.Ino {
		return false, ErrReplaced
	}
	currentParent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		return false, errors.Join(ErrReplaced, err)
	}
	if err := unix.Unlinkat(int(parent.Fd()), filepath.Base(path), 0); err != nil {
		return false, err
	}
	retired = true
	if err := parent.Sync(); err != nil {
		return retired, errors.Join(ErrRetirement, err)
	}
	currentParent, err = os.Lstat(filepath.Dir(path))
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		return retired, errors.Join(ErrRetirement, ErrReplaced, err)
	}
	return retired, nil
}
