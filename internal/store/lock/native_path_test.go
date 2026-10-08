package lock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativePathUsesSameLiveLockAndRejectsForeignAndSymlinkAncestors(t *testing.T) {
	root := t.TempDir()
	guard, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	child := filepath.Join(root, "audit", "archive")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "ledger.wal"), filepath.Join(root, "audit", "audit.log"), filepath.Join(child, "history.log")} {
		if !guard.CoversNativePath(path) {
			t.Fatal("actual guarded native descendant refused", path)
		}
	}
	if guard.CoversDataDirectory(child) {
		t.Fatal("native path qualification fabricated another directory lock")
	}
	foreign := t.TempDir()
	if err := os.Symlink(foreign, filepath.Join(root, "redirected")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(foreign, "audit.log"), filepath.Join(root, "..", "foreign", "audit.log"), filepath.Join(root, "redirected", "audit.log"), filepath.Join(root, "missing", "audit.log")} {
		if guard.CoversNativePath(path) {
			t.Fatal("unqualified native path admitted", path)
		}
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if guard.CoversNativePath(filepath.Join(child, "history.log")) {
		t.Fatal("closed guard retained native authority")
	}
}
