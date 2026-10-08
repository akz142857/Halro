package app

import (
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestCommandDirectoryLockDebtExcludesNextActualOwner(t *testing.T) {
	directory := t.TempDir()
	guard, err := lock.Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	cause := errors.Join(ownedfile.ErrRetirement, errors.New("actual descriptor exit was not established"))
	err = finishCommandDataLock(guard, cause)
	var debt *commandDirectoryLockFailure
	if !errors.As(err, &debt) || debt.guard != guard || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("retirement error lost the original exclusion owner", err)
	}
	if next, err := lock.Acquire(directory); err == nil {
		_ = next.Close()
		t.Fatal("uncertain command retirement exposed its live directory")
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := lock.Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryFailure := errors.New("input refused before IO")
	if err := finishCommandDataLock(next, ordinaryFailure); !errors.Is(err, ordinaryFailure) {
		t.Fatal(err)
	}
	final, err := lock.Acquire(directory)
	if err != nil {
		t.Fatal("known retired command retained the directory", err)
	}
	if err := final.Close(); err != nil {
		t.Fatal(err)
	}
}
