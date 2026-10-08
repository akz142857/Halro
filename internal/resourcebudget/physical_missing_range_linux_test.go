//go:build linux

package resourcebudget

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPhysicalMissingRangeCountsActualAlignedExtentsAndHoles(t *testing.T) {
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HALRO_EXT4_ROOT") == "" || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("isolated ext4 fixture required")
	}
	file, err := os.OpenFile(filepath.Join(dir, "actual-target"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	b := fs.Bsize
	for _, start := range []int64{0, 2 * b} {
		if err := AllocatePhysicalRange(file, start, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ start, end, missing int64 }{
		{1, b - 1, 0}, {1, 3*b - 1, b}, {1, 3*b + 1, 2 * b},
		{b, 2 * b, b}, {2*b + 1, 3*b - 1, 0}, {3 * b, 4 * b, b},
	} {
		missing, err := physicalMissingRangeBytes(file, c.start, c.end, b)
		if err != nil || missing != c.missing {
			t.Fatalf("actual [%d,%d): missing=%d expected=%d err=%v", c.start, c.end, missing, c.missing, err)
		}
	}
	if info, err := file.Stat(); err != nil || info.Size() != 0 {
		t.Fatal("extent qualification changed EOF", err)
	}
}
