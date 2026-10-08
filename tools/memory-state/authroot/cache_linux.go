//go:build linux

package authroot

import (
	"golang.org/x/sys/unix"
	"os"
)

func configurePackAccess(f *os.File) error {
	// RANDOM disables readahead for this FD only; no system-wide cache changes.
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_RANDOM)
}
func discardPackCache(f *os.File, offset, length int64) error {
	return unix.Fadvise(int(f.Fd()), offset, length, unix.FADV_DONTNEED)
}
