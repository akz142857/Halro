package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerPreservesReplacementAndSymlink(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "symlink"}[symlink], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "derived.db")
			file, owner, err := Create(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			moved := path + ".moved"
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if symlink {
				err = os.Symlink(moved, path)
			} else {
				err = os.WriteFile(path, []byte("foreign authority"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := owner.Remove(); !errors.Is(err, ErrReplaced) {
					t.Fatal(err)
				}
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("replacement removed", err)
			}
			if _, err := os.Lstat(moved); err != nil {
				t.Fatal("moved original removed", err)
			}
		})
	}
}

func TestExclusiveCreateAndOwnedRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "derived.db")
	f, owner, err := Create(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Create(path, os.O_RDWR, 0600); !errors.Is(err, os.ErrExist) {
		t.Fatal("existing file reopened", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := owner.Remove(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestRetirementRequiresOwnUnlinkBeforeIdempotentSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "derived.db")
	f, owner, err := Create(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err := owner.Retire(); !errors.Is(err, ErrReplaced) || !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing name was mistaken for paid retirement", err)
		}
	}
	if err := os.Rename(path+".moved", path); err != nil {
		t.Fatal(err)
	}
	if err := owner.Retire(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign after retirement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.Retire(); err != nil {
		t.Fatal("successful receipt should remain retired", err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "foreign after retirement" {
		t.Fatal("repeated retirement removed new occupant", err)
	}
}
