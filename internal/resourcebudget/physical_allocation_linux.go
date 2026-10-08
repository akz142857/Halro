//go:build linux

package resourcebudget

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func allocatePhysicalRange(file *os.File, start, bytes int64) error {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil {
		return err
	}
	// tmpfs is useful for real ENOSPC fixtures, but not durable production
	// acceptance. No silent fallback on CoW/overlay/unsupported filesystems.
	if (fs.Type != unix.EXT4_SUPER_MAGIC && fs.Type != unix.TMPFS_MAGIC) || fs.Bsize <= 0 || fs.Bsize > 1<<20 {
		return ErrPhysicalAllocation
	}
	// Free space is only a negative preflight. Count the actual holes, including
	// block alignment, rather than charging existing extents again. Sufficient
	// space never grants anything: the syscall can still fail partially below.
	if fs.Type == unix.EXT4_SUPER_MAGIC {
		missing, err := physicalMissingRangeBytes(file, start, start+bytes, fs.Bsize)
		if err != nil {
			return err
		}
		if uint64(missing/fs.Bsize) > fs.Bavail {
			return errors.Join(ErrPhysicalAllocationNoWrite, unix.ENOSPC)
		}
	}
	var before, after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return err
	}
	err := unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_KEEP_SIZE, start, bytes)
	if err != nil {
		statErr := unix.Fstat(int(file.Fd()), &after)
		if statErr != nil || before.Blocks != after.Blocks || before.Size != after.Size {
			return errors.Join(ErrPhysicalAllocationUncertain, err, statErr)
		}
	}
	return err
}

func retirePhysicalTail(file *os.File, eof, allocatedEnd int64) error {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &fs); err != nil {
		return err
	}
	if (fs.Type != unix.EXT4_SUPER_MAGIC && fs.Type != unix.TMPFS_MAGIC) || fs.Bsize <= 0 || fs.Bsize > 1<<20 {
		return ErrPhysicalAllocation
	}
	// Allocation necessarily owns the final block intersecting its range. Round
	// only its end; the prefix before EOF remains intact, including a partial block.
	end := (allocatedEnd + fs.Bsize - 1) / fs.Bsize * fs.Bsize
	if fs.Type == unix.EXT4_SUPER_MAGIC {
		// ext4 clamps hole punching to i_size. KEEP_SIZE unwritten extents
		// beyond EOF therefore survive a successful PUNCH_HOLE syscall. Prove
		// that every actual tail extent belongs to this recorded upper range,
		// then use same-EOF truncate; never expose a temporary larger EOF that
		// could become unauthenticated native bytes after SIGKILL.
		if err := CheckPhysicalTail(file, eof, allocatedEnd); err != nil {
			return err
		}
		if err := file.Truncate(eof); err != nil {
			return errors.Join(ErrPhysicalAllocationUncertain, err)
		}
		if err := file.Sync(); err != nil {
			return errors.Join(ErrPhysicalAllocationUncertain, err)
		}
		if err := CheckPhysicalTail(file, eof, eof); err != nil {
			return errors.Join(ErrPhysicalAllocationUncertain, err)
		}
		return nil
	}
	return unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, eof, end-eof)
}
