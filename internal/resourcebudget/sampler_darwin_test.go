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

func TestDarwinHostAvailableIsTheKernelShareOfMemory(t *testing.T) {
	for _, tc := range []struct {
		level   uint32
		memsize uint64
		want    int64
	}{{40, 24 << 30, 10307921510}, {0, 8 << 30, 0}, {100, 8 << 30, 8 << 30}, {33, 101, 33}, {100, math.MaxInt64, math.MaxInt64}} {
		got, err := hostAvailable(tc.level, tc.memsize)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, tc := range []struct {
		level   uint32
		memsize uint64
	}{{101, 8 << 30}, {40, 0}, {40, math.MaxInt64 + 1}} {
		if _, err := hostAvailable(tc.level, tc.memsize); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid host memory accepted", tc, err)
		}
	}
	a, b := darwinHostIdentity(24<<30, 100, 5), darwinHostIdentity(24<<30, 101, 5)
	if a == b || a == ([32]byte{}) || a != darwinHostIdentity(24<<30, 100, 5) {
		t.Fatal("host identity does not follow the boot", a, b)
	}
}

func TestDarwinRealSamplerReportsAHostGroup(t *testing.T) {
	s, err := NewSampler(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sample := s.Sample(context.Background())
	if sample.Issues&(CgroupUnavailable|DiskUnavailable|RuntimeUnavailable) != 0 || sample.GroupCount != 1 || !sample.Groups[0].MaxLimited || sample.Groups[0].Max <= sample.Groups[0].Current {
		t.Fatal("real macOS sample", sample.Issues, sample.GroupCount, sample.Groups[0])
	}
}
