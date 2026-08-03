//go:build darwin || linux

package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExclusiveLock(t *testing.T) {
	directory := t.TempDir()
	first, err := Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if _, err := Acquire(directory); !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("expected ErrAlreadyLocked, got %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(directory)
	if err != nil {
		t.Fatalf("lock should be reusable after close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInitializationLockBlocksWritersWithoutCreatingDataDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "new-data")
	initialization, err := AcquireInitialization(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer initialization.Close()
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initialization lock created live data directory: %v", err)
	}
	if _, err := Acquire(directory); !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("writer raced initialization publication: %v", err)
	}
}

func TestReadOnlyLockDoesNotRewriteOwnerMetadata(t *testing.T) {
	directory := t.TempDir()
	initial, err := Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, ".heimdall.lock")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := AcquireExistingReadOnly(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("read-only lock changed metadata: before=%q after=%q", before, after)
	}
}
