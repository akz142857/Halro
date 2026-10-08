package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestActualCloseDebtRetainsDescriptorInReturnedError(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "owned"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CloseFile(f); err != nil {
		t.Fatal(err)
	}
	err = CloseFile(f)
	var actual *actualCloseFailure
	if !errors.Is(err, ErrRetirement) || !errors.Is(err, os.ErrClosed) || !errors.As(err, &actual) || actual.file != f {
		t.Fatal("error lost actual descriptor owner", err)
	}
}
