//go:build linux

package resourcebudget

import (
	"encoding/binary"
	"golang.org/x/sys/unix"
	"math"
)

func diskSpace(path string) (diskSignal, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return diskSignal{}, err
	}
	return filesystemSpace(st)
}

func filesystemSpace(st unix.Statfs_t) (diskSignal, error) {
	size := int64(st.Frsize)
	if size <= 0 {
		size = int64(st.Bsize)
	}
	// Some filesystems (observed btrfs) expose no fixed inode capacity. That
	// is unavailable evidence, not proof of either zero or unlimited inodes.
	if size <= 0 || uint64(st.Bavail) > uint64(math.MaxInt64)/uint64(size) || st.Files == 0 || st.Ffree > st.Files || uint64(st.Ffree) > math.MaxInt64 {
		return diskSignal{}, ErrInvalid
	}
	return diskSignal{int64(st.Bavail) * size, int64(st.Ffree)}, nil
}
func verifyCgroupFS(path string) ([16]byte, error) {
	var identity [16]byte
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return identity, err
	}
	if uint64(st.Type) != uint64(unix.CGROUP2_SUPER_MAGIC) {
		return identity, ErrInvalid
	}
	var object unix.Stat_t
	if err := unix.Stat(path, &object); err != nil {
		return identity, err
	}
	if object.Mode&unix.S_IFMT != unix.S_IFDIR {
		return identity, ErrInvalid
	}
	binary.LittleEndian.PutUint64(identity[:8], uint64(object.Dev))
	binary.LittleEndian.PutUint64(identity[8:], uint64(object.Ino))
	return identity, nil
}

// Linux samples the process's cgroup; it has no host stand-in.
func platformHostMemory() func() (hostMemory, error) { return nil }
