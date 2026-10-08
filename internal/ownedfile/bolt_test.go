package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func TestExclusiveBoltActualHandoffFailurePreservesCreationReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate")
	var actual *os.File
	db, receipt, err := openExclusiveBolt(path, 0600, &bbolt.Options{}, func(name string, flags int, mode os.FileMode) (*os.File, *Owner, error) {
		file, owner, err := Create(name, flags, mode)
		if err != nil {
			return nil, nil, err
		}
		actual = file
		if _, err := file.Write([]byte("not a valid database")); err != nil {
			t.Fatal(err)
		}
		return file, owner, nil
	})
	if db != nil || receipt == nil || !errors.Is(err, ErrRetirement) {
		t.Fatal("opaque actual engine failure granted retirement", db, receipt, err)
	}
	if _, err := actual.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("failed engine left actual descriptor open", err)
	}
	if named, err := os.Lstat(path); err != nil || !os.SameFile(receipt.file, named) {
		t.Fatal("constructor receipt or original inode lost", err)
	}
	if err := receipt.Retire(); !errors.Is(err, ErrRetirement) {
		t.Fatal("opaque engine receipt granted unlink", err)
	}
	if alias, err := receipt.LinkCaptured(path + ".alias"); alias != nil || !errors.Is(err, ErrRetirement) {
		t.Fatal("opaque engine receipt granted an alias", alias, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("opaque engine output removed", err)
	}
}

func TestExclusiveBoltRefusesExistingFileWithoutHandoffDebt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	db, receipt, err := OpenExclusiveBolt(path, 0600, &bbolt.Options{})
	if db != nil || receipt != nil || !errors.Is(err, os.ErrExist) || errors.Is(err, ErrRetirement) {
		t.Fatal("pre-handoff refusal became engine debt", db, receipt, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "foreign" {
		t.Fatal("foreign file altered", err)
	}
}
