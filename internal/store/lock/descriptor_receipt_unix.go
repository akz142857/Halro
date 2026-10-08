//go:build darwin || linux

package lock

import (
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// DescriptorReceipt returns the inode of this actual live exclusion descriptor.
// A name alone never grants retirement authority after the descriptor closes.
func (l *Lock) DescriptorReceipt() (os.FileInfo, error) {
	if l == nil {
		return nil, ownedfile.ErrReplaced
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.closing || l.file == nil || l.closeErr != nil {
		return nil, errors.Join(ownedfile.ErrRetirement, l.closeErr)
	}
	actual, err := l.file.Stat()
	if err != nil {
		return nil, err
	}
	named, err := os.Lstat(l.file.Name())
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(actual, named) {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	return actual, nil
}
