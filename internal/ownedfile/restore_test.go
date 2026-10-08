package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreAuthenticatedCapturesOnlyExactExistingDescriptor(t *testing.T) {
	for _, name := range []string{"exact", "replaced", "changed-size", "parent-replaced"} {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "pool")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "candidate")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			expected, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			parent, err := os.Stat(directory)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "replaced":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-size":
				if _, err := f.Write([]byte("foreign")); err != nil {
					t.Fatal(err)
				}
			case "parent-replaced":
				if err := os.Rename(directory, directory+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := RestoreAuthenticated(path, f, expected, parent)
			if name == "exact" {
				if err != nil || owner == nil {
					t.Fatal(err)
				}
				if err := owner.VerifyFile(f); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrReplaced) || owner != nil {
				t.Fatal("foreign target acquired retirement authority", owner, err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() {
				t.Fatal("capture changed an existing inode", err)
			}
		})
	}
}
