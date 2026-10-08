//go:build linux

package resourcebudget

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func capturePhysicalIdentity(file *os.File) (PhysicalIdentity, error) {
	if file == nil {
		return PhysicalIdentity{}, ErrInvalid
	}
	var stat unix.Statx_t
	if err := unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_INO|unix.STATX_BTIME, &stat); err != nil {
		return PhysicalIdentity{}, errors.Join(ErrPhysicalAllocation, err)
	}
	if stat.Mask&unix.STATX_BTIME == 0 || stat.Ino == 0 || stat.Btime.Sec == 0 {
		return PhysicalIdentity{}, ErrPhysicalAllocation
	}
	return PhysicalIdentity{DeviceMajor: stat.Dev_major, DeviceMinor: stat.Dev_minor, Inode: stat.Ino, BornSeconds: stat.Btime.Sec, BornNanos: stat.Btime.Nsec}, nil
}

func physicalAllocatedBytes(file *os.File) (int64, error) {
	if file == nil {
		return 0, ErrInvalid
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, err
	}
	if stat.Blocks < 0 || stat.Blocks > (1<<50)/512 {
		return 0, ErrPhysicalAllocation
	}
	return stat.Blocks * 512, nil
}
