package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// F1 (board §28, §40.16): a killed process leaves private archive
// temporaries that nothing else reclaims, and the archive inventory charges
// them against the ceiling. The next lock-holding Exporter removes exactly
// those names and keeps resumable state and foreign names.
func TestExporterReclaimsCrashTemporaries(t *testing.T) {
	leftovers := []string{
		".archive-entries-123.tmp",
		".archive-root-9.tmp",
		".archive-legacy-77.tmp",
		".archive-date-obligation-5.tmp",
		".archive-date-state-6.tmp",
		".archive-date-reclaim-spool-8",
		"2026/04/.archive-partition-41.tmp",
		"2026/04/.archive-column-42.tmp",
		"2026/.archive-catalog-43.tmp",
		".archive-verify-1/index.bin",
		".archive-date-index-2/index.bin",
		".archive-date-ref-index-3/index.bin",
		".archive-date-orphan-page-index-4/index.bin",
	}
	kept := []string{
		".archive-date-build-v1.json",
		".archive-date-verify-v1.json",
		"2026/04/usage.parquet",
		".archive-root.tmp",
		".archive-root-x.tmp",
		"foreign.tmp",
	}
	setup := func(t *testing.T) string {
		root := t.TempDir()
		for _, name := range append(append([]string{}, leftovers...), kept...) {
			path := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("left"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// A directory under a file temporary's name is not one.
		if err := os.MkdirAll(filepath.Join(root, ".archive-entries-55.tmp", "data"), 0o700); err != nil {
			t.Fatal(err)
		}
		return root
	}
	exists := func(t *testing.T, path string) bool {
		_, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return err == nil
	}
	check := func(t *testing.T, root string, reclaimed bool) {
		for _, name := range leftovers {
			if exists(t, filepath.Join(root, name)) == reclaimed {
				t.Fatal("crash temporary reclaimed", !reclaimed, name)
			}
		}
		for _, name := range append(kept, ".archive-entries-55.tmp/data") {
			if !exists(t, filepath.Join(root, name)) {
				t.Fatal("reclaim removed a name it does not own", name)
			}
		}
	}
	open := func(t *testing.T, root string, reclaim bool) *Exporter {
		e, err := NewExporterWithContext(context.Background(), root, Options{Format: FormatNDJSON, ArchiveDiskBytes: 1 << 20, ReclaimCrashTemporaries: reclaim})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	t.Run("reclaimed", func(t *testing.T) {
		root := setup(t)
		e := open(t, root, true)
		check(t, root, true)
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("not-requested", func(t *testing.T) {
		root := setup(t)
		e := open(t, root, false)
		check(t, root, false)
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("live-exporter-in-process", func(t *testing.T) {
		root := setup(t)
		live := open(t, root, false)
		e := open(t, root, true)
		check(t, root, false)
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if err := live.Close(); err != nil {
			t.Fatal(err)
		}
		again := open(t, root, true)
		check(t, root, true)
		if err := again.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
