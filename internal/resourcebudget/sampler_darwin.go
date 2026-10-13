//go:build darwin

package resourcebudget

import (
	"crypto/sha256"
	"encoding/binary"
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

// macOS has no cgroup. Its kernel reports the share of memory still available
// (kern.memorystatus_level, a percentage) because free pages alone are mostly
// cache; that share of hw.memsize is what the host can still give the process.
func platformHostMemory() func() (hostMemory, error) { return darwinHostMemory }

func darwinHostMemory() (hostMemory, error) {
	level, err := unix.SysctlUint32("kern.memorystatus_level")
	if err != nil {
		return hostMemory{}, err
	}
	memsize, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return hostMemory{}, err
	}
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return hostMemory{}, err
	}
	available, err := hostAvailable(level, memsize)
	if err != nil {
		return hostMemory{}, err
	}
	return hostMemory{available: available, identity: darwinHostIdentity(memsize, boot.Sec, int64(boot.Usec))}, nil
}

func hostAvailable(level uint32, memsize uint64) (int64, error) {
	if level > 100 || memsize == 0 || memsize > math.MaxInt64 {
		return 0, ErrInvalid
	}
	// memsize/100*level keeps the product inside int64 for any valid memsize.
	return int64(memsize/100*uint64(level) + memsize%100*uint64(level)/100), nil
}

// The identity changes when the host reboots or its memory changes, which the
// pressure controller reports as SourceChanged, as for a different cgroup.
func darwinHostIdentity(memsize uint64, bootSec, bootUsec int64) [32]byte {
	var b [24]byte
	binary.BigEndian.PutUint64(b[0:], memsize)
	binary.BigEndian.PutUint64(b[8:], uint64(bootSec))
	binary.BigEndian.PutUint64(b[16:], uint64(bootUsec))
	return sha256.Sum256(append([]byte("halro:darwin-host:v1\x00"), b[:]...))
}
