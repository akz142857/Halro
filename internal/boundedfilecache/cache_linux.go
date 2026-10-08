//go:build linux

package boundedfilecache

import (
	"golang.org/x/sys/unix"
	"os"
)

func advise(file *os.File, offset, length int64) error {
	return unix.Fadvise(int(file.Fd()), offset, length, unix.FADV_DONTNEED)
}
