package app

import (
	"errors"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
	"os"
	"path/filepath"
	"testing"
)

func TestSeedGuardRetainsOriginalExclusionAfterUnknownDescendantExit(t *testing.T) {
	directory := t.TempDir()
	guard, err := lock.Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	file, err := os.OpenFile(filepath.Join(directory, "actual-native"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cause := ownedfile.CloseError(file, errors.New("actual descendant exit unproven"))
	retained := retireSeedGuard(guard, cause)
	var owner *seedLockRetirementFailure
	if !errors.As(retained, &owner) || owner.Guard != guard || !errors.Is(retained, ownedfile.ErrRetirement) {
		t.Fatal("original guard or child debt lost", retained)
	}
	if next, err := lock.AcquireExistingReadOnly(directory); !errors.Is(err, lock.ErrAlreadyLocked) {
		if next != nil {
			next.Close()
		}
		t.Fatal("unjoined descendant released process exclusion", err)
	}
	if _, err = file.Stat(); err != nil {
		t.Fatal("debt descriptor was falsely retired", err)
	}
}
