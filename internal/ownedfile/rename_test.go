//go:build linux || darwin

package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCapturedRenamePublishesOneNameAndRetiresSameInode(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "pending"), filepath.Join(dir, "published")
	file, owner, err := Create(from, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write([]byte("authenticated envelope")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := owner.RenameCaptured(to); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(from); !os.IsNotExist(err) {
		t.Fatal("publication left a pending alias", err)
	}
	if err := owner.VerifyFile(file); err != nil {
		t.Fatal("creation receipt lost the actual descriptor", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Retire(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(to); !os.IsNotExist(err) {
		t.Fatal("retirement did not follow the published inode", err)
	}
}

func TestCapturedRenamePreservesForeignDestinationSourceAndParent(t *testing.T) {
	for _, change := range []string{"destination", "source", "parent", "symlink-destination"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "parent")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			from, to := filepath.Join(dir, "pending"), filepath.Join(dir, "published")
			file, owner, err := Create(from, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.Write([]byte("original")); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "destination":
				err = os.WriteFile(to, []byte("foreign"), 0600)
			case "source":
				if err = os.Rename(from, from+".saved"); err == nil {
					err = os.WriteFile(from, []byte("foreign"), 0600)
				}
			case "parent":
				if err = os.Rename(dir, dir+".saved"); err == nil {
					if err = os.Mkdir(dir, 0700); err == nil {
						err = os.WriteFile(from, []byte("foreign"), 0600)
					}
				}
			case "symlink-destination":
				if err = os.WriteFile(filepath.Join(root, "foreign"), []byte("foreign"), 0600); err == nil {
					err = os.Symlink(filepath.Join(root, "foreign"), to)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.RenameCaptured(to); err == nil {
				t.Fatal("renamed through a foreign namespace")
			} else if (change == "source" || change == "parent") && !errors.Is(err, ErrReplaced) {
				t.Fatal(err)
			}
			foreign := to
			if change == "source" || change == "parent" {
				foreign = from
			}
			if raw, err := os.ReadFile(foreign); err != nil || string(raw) != "foreign" {
				t.Fatal("foreign bytes changed", string(raw), err)
			}
			if raw, err := os.ReadFile(file.Name()); change == "destination" && (err != nil || string(raw) != "original") {
				t.Fatal("failed publication changed original", string(raw), err)
			}
		})
	}
}
