package durable

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestSyncDirectoryActualCloseDebtIsNotSuccess(t *testing.T) {
	err := syncDirectory(t.TempDir(), func(path string) (*os.File, error) {
		f, err := os.Open(path)
		if err == nil {
			err = f.Close()
		}
		return f, err
	})
	if !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, os.ErrClosed) {
		t.Fatal("actual directory Close failure must survive Sync failure", err)
	}
}

func TestSyncDirectoryRefusesSymlinkAndRegularFile(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, file} {
		if err := SyncDirectory(path); err == nil {
			t.Fatal("not an actual directory", path)
		}
	}
	if err := SyncDirectory(dir); err != nil {
		t.Fatal(err)
	}
}
