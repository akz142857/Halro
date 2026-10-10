//go:build linux

package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// An export publishes one partition file per date run and then the catalog
// and manifest. Actual ENOSPC can land in any of those steps. Free space is
// raised one block at a time from a full ext4, so successive attempts fail at
// successively later points, partitions included. Every failed attempt must
// return the ENOSPC, publish no frontier, leave no scratch file, and leave
// only complete partitions identical to an unhindered export. Once space
// suffices, the same exporter must reuse those partitions and end with exactly
// the unhindered export's position and files.
func TestDiskExportMultiFilePublicationAtActualENOSPC(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires owned isolated ext4 fixture")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil || st.Type != unix.EXT4_SUPER_MAGIC || st.Bsize != 4096 {
		t.Fatal("actual ext4 with 4 KiB blocks required", err)
	}
	t.Setenv("TMPDIR", root)
	for _, format := range []string{FormatParquet, FormatNDJSON} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			f := diskExportFixture(t, 30)
			limits := DefaultDiskExportLimits()
			dir := t.TempDir()
			reference, err := NewExporterWithOptions(filepath.Join(dir, "reference"), Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reference.ExportDisk(ctx, f.p, limits); err != nil {
				t.Fatal("unhindered export", err)
			}
			want, err := reference.ReadArchivePosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wantFiles := archivePartitionDigests(t, reference.root)
			if len(wantFiles) < 4 {
				t.Fatal("fixture does not publish several partitions", len(wantFiles))
			}
			e, err := NewExporterWithOptions(filepath.Join(dir, "full"), Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			filler, err := os.OpenFile(filepath.Join(root, "usage-archive-filler-"+format), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = filler.Close(); _ = os.Remove(filler.Name()) }()
			var size int64
			for ; ; size += 4096 {
				err := unix.Fallocate(int(filler.Fd()), 0, size, 4096)
				if errors.Is(err, syscall.ENOSPC) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			failures, withOrphans := 0, 0
			for freed := int64(0); ; freed++ {
				if freed > 4096 {
					t.Fatal("export never completed as space returned")
				}
				if err := filler.Truncate(size - freed*4096); err != nil {
					t.Fatal(err)
				}
				if err := filler.Sync(); err != nil {
					t.Fatal(err)
				}
				cut, err := e.ExportDisk(ctx, f.p, limits)
				if err == nil {
					break
				}
				failures++
				if !errors.Is(err, syscall.ENOSPC) || cut != (DiskArchiveCut{}) {
					t.Fatal("export at actual ENOSPC", freed, cut, err)
				}
				if _, err := e.ReadArchivePosition(ctx); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed export published a frontier", freed, err)
				}
				assertNoArchiveScratch(t, e.root)
				got := archivePartitionDigests(t, e.root)
				for path, digest := range got {
					if wantFiles[path] != digest {
						t.Fatal("failed export left a partition the unhindered export does not have", freed, path)
					}
				}
				if len(got) > 0 {
					withOrphans++
				}
			}
			if failures < 3 || withOrphans == 0 || withOrphans == failures {
				t.Fatal("ENOSPC did not land both before and after partitions were published", failures, withOrphans)
			}
			position, err := e.ReadArchivePosition(ctx)
			if err != nil || position.Files != want.Files || position.LastSequence != want.LastSequence || position.CompletePrefix != want.CompletePrefix {
				t.Fatal("completed export differs from the unhindered one", position, want, err)
			}
			if got := archivePartitionDigests(t, e.root); !reflect.DeepEqual(got, wantFiles) {
				t.Fatal("completed export files differ from the unhindered ones", len(got), len(wantFiles))
			}
			assertNoArchiveScratch(t, e.root)
			if err := e.VerifyArchiveFileChecksums(ctx); err != nil {
				t.Fatal(err)
			}
			t.Log(format, "failed attempts", failures, "with published partitions", withOrphans)
		})
	}
}

// archivePartitionDigests maps each published partition, relative to root, to
// its SHA-256.
func archivePartitionDigests(t *testing.T, root string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "date=*", "usage-bounded-v1-*"))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = hex.EncodeToString(sum[:])
	}
	return out
}

func assertNoArchiveScratch(t *testing.T, root string) {
	t.Helper()
	// ENOSPC can refuse the archive directory itself; then nothing was left.
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return
	}
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".archive-") || strings.HasSuffix(d.Name(), ".tmp") {
			t.Error("scratch survived", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if t.Failed() {
		t.FailNow()
	}
}
