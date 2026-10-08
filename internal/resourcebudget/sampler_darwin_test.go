//go:build darwin

package resourcebudget

import (
	"context"
	"errors"
	"math"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinFilesystemHeadroomRefusesUnavailableAndOverflow(t *testing.T) {
	valid := unix.Statfs_t{Bsize: 4096, Bavail: 100, Files: 1000, Ffree: 900}
	signal, err := filesystemSpace(valid)
	if err != nil || signal.available != 409600 || signal.inodes != 900 {
		t.Fatalf("headroom = %+v %v", signal, err)
	}
	for _, name := range []string{"block-size", "bytes-overflow", "unknown-inodes", "invalid-inodes", "inode-overflow"} {
		t.Run(name, func(t *testing.T) {
			st := valid
			switch name {
			case "block-size":
				st.Bsize = 0
			case "bytes-overflow":
				st.Bavail = math.MaxUint64
			case "unknown-inodes":
				st.Files = 0
			case "invalid-inodes":
				st.Ffree = st.Files + 1
			case "inode-overflow":
				st.Files = math.MaxUint64
				st.Ffree = math.MaxUint64
			}
			if _, err := filesystemSpace(st); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unreliable signal accepted: %v", err)
			}
		})
	}
}

func TestDarwinDiskHeadroomSamplesRealFilesystemWithoutCgroupCredit(t *testing.T) {
	dir := t.TempDir()
	sampler, err := NewSampler(dir)
	if err != nil {
		t.Fatal(err)
	}
	available, inodes, err := sampler.DiskHeadroom(context.Background(), dir)
	if err != nil || available < 0 || inodes < 0 {
		t.Fatalf("real filesystem headroom: %d %d %v", available, inodes, err)
	}
	if _, err := verifyCgroupFS(dir); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-Linux directory credited as cgroup: %v", err)
	}
}
