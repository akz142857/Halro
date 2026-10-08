//go:build darwin

package resourcebudget

import (
	"math"

	"golang.org/x/sys/unix"
)

// macOS can provide filesystem headroom for offline backup/restore even though
// Linux cgroup pressure sampling remains unavailable. Unknown inode capacity
// is still refused; no synthetic unlimited capacity is substituted.
func diskSpace(path string) (diskSignal, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return diskSignal{}, err
	}
	return filesystemSpace(st)
}

func filesystemSpace(st unix.Statfs_t) (diskSignal, error) {
	size := int64(st.Bsize)
	if size <= 0 || uint64(st.Bavail) > uint64(math.MaxInt64)/uint64(size) ||
		st.Files == 0 || st.Ffree > st.Files || uint64(st.Ffree) > math.MaxInt64 {
		return diskSignal{}, ErrInvalid
	}
	return diskSignal{available: int64(st.Bavail) * size, inodes: int64(st.Ffree)}, nil
}

func verifyCgroupFS(string) ([16]byte, error) { return [16]byte{}, ErrInvalid }
