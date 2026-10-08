//go:build darwin

package replication

import (
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
	"syscall"
)

func captureReceivedBirth(f *os.File) (resourcebudget.PhysicalIdentity, error) {
	if f == nil {
		return resourcebudget.PhysicalIdentity{}, ErrMemberAuthority
	}
	var s syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &s); err != nil {
		return resourcebudget.PhysicalIdentity{}, err
	}
	if s.Ino == 0 || s.Birthtimespec.Sec == 0 {
		return resourcebudget.PhysicalIdentity{}, ErrMemberAuthority
	}
	return resourcebudget.PhysicalIdentity{DeviceMajor: uint32(s.Dev), Inode: s.Ino, BornSeconds: s.Birthtimespec.Sec, BornNanos: uint32(s.Birthtimespec.Nsec)}, nil
}
