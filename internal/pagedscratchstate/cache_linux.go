//go:build linux

package pagedscratchstate

import (
	"golang.org/x/sys/unix"
	"os"
)

// The actual writer has completed Sync before this bounded, captured-FD cache
// request. Advice is observable and fallible; only Linux capacity experiments
// can establish a process/cgroup peak, never the success of this syscall alone.
func retireCacheWindow(f *os.File, knownSyncedEnd int64) error {
	return unix.Fadvise(int(f.Fd()), 0, knownSyncedEnd, unix.FADV_DONTNEED)
}
