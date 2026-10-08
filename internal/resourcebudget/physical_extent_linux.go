//go:build linux

package resourcebudget

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"math"
	"os"
	"unsafe"
)

type physicalExtent struct {
	Logical, Physical, Length uint64
	Reserved64                [2]uint64
	Flags                     uint32
	Reserved32                [3]uint32
}

type physicalExtentMap struct {
	Start, Length                  uint64
	Flags, Mapped, Count, Reserved uint32
	Extents                        [64]physicalExtent
}

// CheckPhysicalTail bounds actual persistent extents beyond the authenticated
// EOF by the independently recorded upper range. It detects copied old receipt
// contents that would otherwise silently lose a later KEEP_SIZE allocation.
func CheckPhysicalTail(file *os.File, eof, end int64) error {
	if file == nil || eof < 0 || end < eof || end > 1<<50 {
		return ErrInvalid
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil {
		return err
	}
	if fs.Bsize <= 0 || fs.Bsize > 1<<20 {
		return ErrPhysicalAllocation
	}
	if fs.Type == unix.TMPFS_MAGIC {
		// tmpfs has no durable FIEMAP authority. It is accepted only as an
		// ephemeral allocation fixture and cannot resume physical ownership.
		return ErrPhysicalAllocation
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC {
		return ErrPhysicalAllocation
	}
	limit := uint64((end + fs.Bsize - 1) / fs.Bsize * fs.Bsize)
	next := uint64(eof / fs.Bsize * fs.Bsize)
	for batch := 0; batch < 1024; batch++ {
		m := physicalExtentMap{Start: next, Length: math.MaxUint64 - next, Flags: 1, Count: 64}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), 0xc020660b, uintptr(unsafe.Pointer(&m)))
		if errno != 0 {
			return errors.Join(ErrPhysicalAllocation, errno)
		}
		if m.Mapped > uint32(len(m.Extents)) {
			return ErrPhysicalAllocation
		}
		if m.Mapped == 0 {
			return nil
		}
		for _, extent := range m.Extents[:m.Mapped] {
			if extent.Length == 0 || extent.Logical > math.MaxUint64-extent.Length || extent.Logical+extent.Length > limit || extent.Flags&0x6 != 0 {
				return fmt.Errorf("%w: extent logical=%d length=%d flags=%x exceeds EOF=%d recorded_end=%d block_limit=%d", ErrPhysicalAllocation, extent.Logical, extent.Length, extent.Flags, eof, end, limit)
			}
			next = extent.Logical + extent.Length
			if extent.Flags&1 != 0 {
				return nil
			}
		}
	}
	return ErrPhysicalAllocation
}

// CheckPhysicalRange requires actual allocated coverage of the entire target
// range. It does not allocate or infer ownership from a logical reservation.
func CheckPhysicalRange(file *os.File, start, end int64) error {
	if file == nil || start < 0 || end <= start || end > 1<<50 {
		return ErrInvalid
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil {
		return err
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC || fs.Bsize <= 0 || fs.Bsize > 1<<20 {
		return ErrPhysicalAllocation
	}
	missing, err := physicalMissingRangeBytes(file, start, end, fs.Bsize)
	if err != nil {
		return err
	}
	if missing != 0 {
		return ErrPhysicalAllocation
	}
	return nil
}

// physicalMissingRangeBytes counts block-aligned holes on the actual ext4 FD.
// Its finite FIEMAP buffer belongs to the caller's existing physical workspace.
// No file content, fee, or free-space value can stand in for these extents.
func physicalMissingRangeBytes(file *os.File, start, end, block int64) (int64, error) {
	next := uint64(start / block * block)
	limit := uint64((end + block - 1) / block * block)
	var missing uint64
	for batch := 0; batch < 1024; batch++ {
		m := physicalExtentMap{Start: next, Length: limit - next, Flags: 1, Count: 64}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), 0xc020660b, uintptr(unsafe.Pointer(&m)))
		if errno != 0 {
			return 0, errors.Join(ErrPhysicalAllocation, errno)
		}
		if m.Mapped > uint32(len(m.Extents)) {
			return 0, ErrPhysicalAllocation
		}
		if m.Mapped == 0 {
			return int64(missing + limit - next), nil
		}
		for _, extent := range m.Extents[:m.Mapped] {
			if extent.Length == 0 || extent.Logical > math.MaxUint64-extent.Length || extent.Flags&0x6 != 0 || extent.Logical%uint64(block) != 0 || extent.Length%uint64(block) != 0 || extent.Logical+extent.Length <= next {
				return 0, ErrPhysicalAllocation
			}
			if extent.Logical > next {
				holeEnd := min(extent.Logical, limit)
				missing += holeEnd - next
				next = holeEnd
			}
			if next >= limit {
				return int64(missing), nil
			}
			next = min(extent.Logical+extent.Length, limit)
			if next >= limit {
				return int64(missing), nil
			}
			if extent.Flags&1 != 0 {
				return int64(missing + limit - next), nil
			}
		}
	}
	return 0, ErrPhysicalAllocation
}
