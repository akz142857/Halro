package ledger

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ownedfile"
	"golang.org/x/sys/unix"
)

// This entry is used only after a fully verified compressed representation is
// durably selected by the native manifest, under Log mutation exclusion. File
// identity alone is not authority to delete an authoritative representation.
func retireCompactedNative(path string, expected, parentInfo os.FileInfo) (bool, error) {
	return retireCompactedNativeWithPhase(path, expected, parentInfo, nil)
}
func retireCompactedNativeWithPhase(path string, expected, parentInfo os.FileInfo, phase func(*os.File, *os.File) error) (retired bool, resultErr error) {
	if expected == nil || parentInfo == nil || !expected.Mode().IsRegular() || !parentInfo.IsDir() {
		return false, ownedfile.ErrReplaced
	}
	directory := filepath.Dir(path)
	name := filepath.Base(path)
	parent, err := os.OpenFile(directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := parent.Close(); err != nil {
			resultErr = errors.Join(resultErr, ErrSegmentRetirement, err)
		}
	}()
	actualParent, err := parent.Stat()
	if err != nil || !os.SameFile(parentInfo, actualParent) {
		return false, errors.Join(err, ownedfile.ErrReplaced)
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, ErrSegmentRetirement, err)
		}
	}()
	if err := unchangedSegmentDescriptor(file, expected); err != nil {
		return false, err
	}
	if phase != nil {
		if err := phase(parent, file); err != nil {
			return false, err
		}
	}
	// Reinspect through the same pinned directory after any operation boundary.
	var named unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, err
	}
	var actual unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &actual); err != nil {
		return false, err
	}
	if named.Mode&unix.S_IFMT != unix.S_IFREG || named.Dev != actual.Dev || named.Ino != actual.Ino || named.Size != actual.Size {
		return false, ownedfile.ErrReplaced
	}
	if err := unchangedSegmentDescriptor(file, expected); err != nil {
		return false, err
	}
	currentParent, err := os.Lstat(directory)
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		return false, errors.Join(err, ownedfile.ErrReplaced)
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
		return false, err
	}
	retired = true // Unlink already happened, even if Sync or actual Close fails.
	if err := parent.Sync(); err != nil {
		return retired, errors.Join(ErrSegmentRetirement, err)
	}
	currentParent, err = os.Lstat(directory)
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		return retired, errors.Join(ErrSegmentRetirement, ownedfile.ErrReplaced, err)
	}
	return retired, nil
}
