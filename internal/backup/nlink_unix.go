//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package backup

import (
	"os"
	"syscall"
)

func hasMultipleLinks(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink > 1
}
