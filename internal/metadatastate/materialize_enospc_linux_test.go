//go:build linux

package metadatastate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// E1 point 4 component (board §40.14): the selected backup's Metadata image is
// a bbolt file. Real ENOSPC while bbolt writes its first pages inside Open, or
// while a later transaction writes rows, is an ordinary failure: no image, no
// retained fee, no retirement debt, and the same call succeeds once space
// returns. Pressure stays Healthy here; the command test covers the sampler.
func TestMetadataImageAtActualENOSPCRemovesImageAndFees(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated Linux ext4 filesystem <=512 MiB")
	}
	var statfs unix.Statfs_t
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" || unix.Statfs(t.TempDir(), &statfs) != nil || statfs.Type != unix.EXT4_SUPER_MAGIC || uint64(statfs.Blocks)*uint64(statfs.Bsize) > 512<<20 {
		t.Fatal("refusing to exhaust filesystem outside explicit dedicated guard", statfs)
	}
	block := int64(statfs.Bsize)
	filler := filepath.Join(root, "metadata-image-filler")
	t.Cleanup(func() { _ = os.Remove(filler) })
	// fill leaves exactly headroom blocks free (as root, reserved blocks too).
	fill := func(headroom int64) {
		f, err := os.OpenFile(filler, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		size := info.Size()
		for chunk := int64(64 << 20); chunk >= block; {
			if err := unix.Fallocate(int(f.Fd()), 0, size, chunk); err != nil {
				if !errors.Is(err, syscall.ENOSPC) {
					t.Fatal(err)
				}
				chunk /= 2
				continue
			}
			size += chunk
		}
		if err := f.Truncate(max(0, size-headroom*block)); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, opened bool, headroom int64) bool {
		b, o, _ := linkedDeltaCandidate(t)
		ctx := context.Background()
		selected, err := b.SealReference(ctx)
		if err != nil {
			t.Fatal(err)
		}
		credit, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
		if err != nil {
			t.Fatal(err)
		}
		defer credit.Release()
		output := privateSource(t)
		before := o.Budget.Snapshot().Used
		reached := false
		if opened {
			materializePhase = func(at string) {
				if at == "opened" {
					reached = true
					fill(headroom)
				}
			}
			defer func() { materializePhase = nil }()
		} else {
			fill(headroom)
		}
		image, err := b.MaterializeMetadata(ctx, output, selected, 16<<20, credit)
		materializePhase = nil
		after := o.Budget.Snapshot().Used
		if err := os.Remove(filler); err != nil {
			t.Fatal(err)
		}
		t.Log("opened", opened, "headroom", headroom, "reached", reached, "enospc", errors.Is(err, syscall.ENOSPC), err)
		met := false
		if err != nil {
			if image != nil || errors.Is(err, ownedfile.ErrRetirement) {
				t.Fatal("ENOSPC image left retirement debt", image != nil, err)
			}
			if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
				t.Fatal("failed image file survived", statErr)
			}
			if after != before {
				t.Fatal("failed image kept fees", before, after)
			}
			if opened && !reached {
				t.Fatal("image failed before it was opened", err)
			}
			met = errors.Is(err, syscall.ENOSPC)
		} else if err := image.Discard(); err != nil {
			t.Fatal(err)
		}
		retry, err := b.MaterializeMetadata(ctx, output, selected, 16<<20, credit)
		if err != nil {
			t.Fatal("image after space returned", err)
		}
		if err := retry.Discard(); err != nil {
			t.Fatal(err)
		}
		if got := o.Budget.Snapshot().Used; got != before {
			t.Fatal("discarded image kept fees", before, got)
		}
		return met
	}
	t.Run("before-open", func(t *testing.T) {
		if !run(t, false, 0) {
			t.Fatal("bbolt Open did not meet ENOSPC with the filesystem full")
		}
	})
	t.Run("after-open", func(t *testing.T) {
		met := 0
		for _, headroom := range []int64{0, 1, 2, 4, 8, 16} {
			t.Run(fmt.Sprint(headroom), func(t *testing.T) {
				if run(t, true, headroom) {
					met++
				}
			})
		}
		if met == 0 {
			t.Fatal("no headroom met ENOSPC writing the opened image")
		}
		t.Log("E1-P4C headrooms that met ENOSPC after Open", met)
	})
}
