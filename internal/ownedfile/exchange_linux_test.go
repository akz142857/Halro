//go:build linux

package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCapturedExchangeKeepsBothActualPublicationInodes(t *testing.T) {
	dir := t.TempDir()
	source, sourceOwner, err := Create(filepath.Join(dir, "pending"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	destination, _, err := Create(filepath.Join(dir, "active"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Write([]byte("candidate")); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.Write([]byte("original")); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := source.Stat()
	if err != nil {
		t.Fatal(err)
	}
	originalInfo, err := destination.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(source.Close(), destination.Close()); err != nil {
		t.Fatal(err)
	}
	other, err := sourceOwner.ExchangeCaptured(filepath.Join(dir, "active"), originalInfo)
	if err != nil || other == nil {
		t.Fatal(err)
	}
	actual, err := os.Stat(filepath.Join(dir, "active"))
	if err != nil || !os.SameFile(actual, sourceInfo) {
		t.Fatal(err)
	}
	old, err := os.Stat(filepath.Join(dir, "pending"))
	if err != nil || !os.SameFile(old, originalInfo) {
		t.Fatal(err)
	}
	if err := sourceOwner.VerifyName(); err != nil {
		t.Fatal(err)
	}
	if err := other.VerifyName(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "pending")); err != nil || string(raw) != "original" {
		t.Fatal(string(raw), err)
	}
}

func TestCapturedExchangeForeignNamesRefuseBeforeMutation(t *testing.T) {
	for _, name := range []string{"pending", "active"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source, owner, err := Create(filepath.Join(dir, "pending"), os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			destination, _, err := Create(filepath.Join(dir, "active"), os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := destination.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(source.Close(), destination.Close()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name)
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
				t.Fatal(err)
			}
			foreign, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if other, err := owner.ExchangeCaptured(filepath.Join(dir, "active"), previous); other != nil || !errors.Is(err, ErrReplaced) {
				t.Fatal("foreign inode exchanged", other, err)
			}
			actual, err := os.Stat(path)
			if err != nil || !os.SameFile(actual, foreign) {
				t.Fatal("foreign name mutated", err)
			}
			if raw, err := os.ReadFile(path); err != nil || string(raw) != "foreign" {
				t.Fatal(string(raw), err)
			}
			if _, err := os.Stat(path + ".original"); err != nil {
				t.Fatal("original disappeared", err)
			}
		})
	}
}
