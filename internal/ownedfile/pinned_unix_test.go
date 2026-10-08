//go:build linux || darwin

package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedCreationAndRetirementPreserveChangedParentNamespace(t *testing.T) {
	for _, operation := range []string{"create", "retire"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "parent")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "owned")
			swap := func(*os.File, *os.File) error {
				if err := os.Rename(directory, directory+".original"); err != nil {
					return err
				}
				if err := os.Mkdir(directory, 0700); err != nil {
					return err
				}
				return os.WriteFile(path, []byte("foreign"), 0600)
			}
			if operation == "create" {
				file, owner, err := createPinned(path, os.O_RDWR, 0600, swap)
				if file != nil || owner != nil || !errors.Is(err, ErrReplaced) || !errors.Is(err, ErrRetirement) {
					t.Fatal(file, owner, err)
				}
			} else {
				file, owner, err := Create(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte("original")); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				if err := owner.retireWithPhase(swap); !errors.Is(err, ErrReplaced) {
					t.Fatal(err)
				}
			}
			if raw, err := os.ReadFile(path); err != nil || string(raw) != "foreign" {
				t.Fatal(string(raw), err)
			}
			if _, err := os.Stat(filepath.Join(directory+".original", "owned")); err != nil {
				t.Fatal("original inode silently retired", err)
			}
		})
	}
}

func TestPinnedRetirementLatchesActualDescriptorCloseDebt(t *testing.T) {
	for _, closeTarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent", true: "target"}[closeTarget], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "owned")
			file, owner, err := Create(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			err = owner.retireWithPhase(func(parent, file *os.File) error {
				if closeTarget {
					return file.Close()
				}
				return parent.Close()
			})
			if !errors.Is(err, ErrRetirement) {
				t.Fatal("actual close failure disappeared", err)
			}
			if err := owner.Retire(); !errors.Is(err, ErrRetirement) {
				t.Fatal("retry erased actual close debt", err)
			}
			if err := owner.Remove(); !errors.Is(err, ErrRetirement) {
				t.Fatal("compatibility retry erased debt", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("unqualified retirement removed original", err)
			}
		})
	}
}

func TestOwnedRemovalDoesNotRefundMissingOrRedirectedOriginal(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "symlink-parent", true: "missing-original"}[missing], func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "parent")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "owned")
			file, owner, err := Create(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if missing {
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(directory, directory+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(directory+".saved", directory); err != nil {
					t.Fatal(err)
				}
			}
			if err := owner.Remove(); !errors.Is(err, ErrReplaced) {
				t.Fatal("missing or redirected inode became paid retirement", err)
			}
		})
	}
}
