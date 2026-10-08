package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
)

// RestoreAuthenticated captures an existing derivative only after the caller
// has independently authenticated its persisted ownership receipt. This check
// proves actual descriptor/name/parent identity, never source or deletion
// authority by itself. The caller retains directory exclusion and the FD.
func RestoreAuthenticated(path string, file *os.File, expected, parent os.FileInfo) (*Owner, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || file == nil || expected == nil || parent == nil {
		return nil, errors.Join(ErrReplaced, err)
	}
	actual, statErr := file.Stat()
	named, nameErr := os.Lstat(absolute)
	directory, dirErr := os.Lstat(filepath.Dir(absolute))
	if statErr != nil || nameErr != nil || dirErr != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 || !os.SameFile(actual, expected) || !os.SameFile(actual, named) || actual.Size() != expected.Size() || actual.Size() != named.Size() || !os.SameFile(parent, directory) {
		return nil, errors.Join(ErrReplaced, statErr, nameErr, dirErr)
	}
	return &Owner{path: absolute, file: actual, parent: directory}, nil
}
