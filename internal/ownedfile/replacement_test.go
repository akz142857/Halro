package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreatedReplacementReceiptFollowsOnlyActualCapturedInodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active")
	old, oldOwner, err := Create(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := old.Stat()
	if err != nil {
		t.Fatal(err)
	}
	candidate, receipt, err := CreateTemporary(dir, "candidate-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if err := receipt.ReplaceCaptured(path, oldInfo); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	actual, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.VerifyFile(actual); err != nil {
		t.Fatal(err)
	}
	actual.Close()
	if err := oldOwner.Remove(); !errors.Is(err, ErrReplaced) {
		t.Fatal("old receipt removed replacement", err)
	}
	if err := receipt.Remove(); err != nil {
		t.Fatal(err)
	}
}
func TestCreatedReplacementPreservesForeignDestinationAndCandidate(t *testing.T) {
	for _, shape := range []string{"destination", "candidate"} {
		t.Run(shape, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "active")
			f, _, err := Create(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			oldInfo, _ := f.Stat()
			f.Close()
			candidate, receipt, err := CreateTemporary(dir, "candidate-*")
			if err != nil {
				t.Fatal(err)
			}
			name := candidate.Name()
			candidate.Close()
			replaced := path
			if shape == "candidate" {
				replaced = name
			}
			if err := os.Rename(replaced, replaced+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(replaced, []byte("foreign"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := receipt.ReplaceCaptured(path, oldInfo); !errors.Is(err, ErrReplaced) {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(replaced)
			if err != nil || string(raw) != "foreign" {
				t.Fatal("foreign file changed", err)
			}
			if _, err := os.Stat(name); err != nil {
				t.Fatal("candidate disappeared", err)
			}
		})
	}
}
