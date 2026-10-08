//go:build darwin || linux

package lock

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestLockOwnedCleanupJoinsBeforeOriginalExclusionRetires(t *testing.T) {
	dir := t.TempDir()
	guard, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	entered, leave := make(chan struct{}), make(chan struct{})
	if err := guard.RetainCleanup(func() error { close(entered); <-leave; return nil }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- guard.Close() }()
	<-entered
	go func() { done <- guard.Close() }()
	if guard.CoversDataDirectory(dir) {
		t.Fatal("closing guard admitted a new borrower")
	}
	if next, err := AcquireExistingReadOnly(dir); !errors.Is(err, ErrAlreadyLocked) {
		if next != nil {
			next.Close()
		}
		t.Fatal("original exclusion retired before source join", err)
	}
	select {
	case err := <-done:
		t.Fatal("Close returned before join", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(leave)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	next, err := AcquireExistingReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockOwnedCleanupDebtPreservesOriginalDescriptorAndFiniteAdmission(t *testing.T) {
	dir := t.TempDir()
	guard, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	actual := guard.file
	defer actual.Close() // fixture retires the deliberately uncertain owner
	cause := errors.New("actual source could not join")
	for range 8 {
		if err := guard.RetainCleanup(func() error { return cause }); err != nil {
			t.Fatal(err)
		}
	}
	if err := guard.RetainCleanup(func() error { t.Fatal("ninth owner entered"); return nil }); err == nil {
		t.Fatal("unbounded owner registration")
	}
	err = guard.Close()
	if !errors.Is(err, cause) || !errors.Is(err, ownedfile.ErrRetirement) || guard.file != actual {
		t.Fatal(err)
	}
	if guard.Close() != err {
		t.Fatal("cleanup debt was forgotten")
	}
	if next, err := AcquireExistingReadOnly(dir); !errors.Is(err, ErrAlreadyLocked) {
		if next != nil {
			next.Close()
		}
		t.Fatal("cleanup debt released original exclusion", err)
	}
}

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
	path := filepath.Join(directory, ".halro.lock")
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

func TestLockIdentifiesItsLiveDataDirectory(t *testing.T) {
	directory := t.TempDir()
	other := t.TempDir()
	guard, err := Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if !guard.CoversDataDirectory(directory) || guard.CoversDataDirectory(other) {
		t.Fatal("lock did not identify exactly its protected data directory")
	}
	if err := os.Rename(filepath.Join(directory, ".halro.lock"), filepath.Join(directory, ".halro.lock.saved")); err != nil {
		t.Fatal(err)
	}
	if guard.CoversDataDirectory(directory) {
		t.Fatal("replaced lock pathname remained trusted")
	}
}

func TestLockActualCloseDebtRetainsDescriptorAndCannotRetryToSuccess(t *testing.T) {
	directory := t.TempDir()
	l, err := Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	actual := l.file
	if err := actual.Close(); err != nil {
		t.Fatal(err)
	}
	first := l.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) {
		t.Fatal(first)
	}
	for range 2 {
		if l.Close() != first || l.file != actual || l.CoversDataDirectory(directory) {
			t.Fatal("unknown actual lock exit was erased")
		}
	}
}

func TestLockPublicationActualCloseDebtRefusesDataLockPublication(t *testing.T) {
	directory := t.TempDir()
	result, err := acquireDataDirectory(directory, func(path string) (*Lock, error) {
		l, err := AcquireInitialization(path)
		if err != nil {
			return nil, err
		}
		if err := l.file.Close(); err != nil {
			t.Fatal(err)
		}
		return l, nil
	})
	if result != nil || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(result, err)
	}
	// The candidate data lock actually exited before returning failure.
	l, err := Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockRefusesSymlinkAndHardlinkBeforeRewritingForeignFile(t *testing.T) {
	for _, shape := range []string{"symlink", "hardlink"} {
		t.Run(shape, func(t *testing.T) {
			directory := t.TempDir()
			foreign := filepath.Join(t.TempDir(), "foreign")
			before := []byte("foreign native bytes remain unchanged")
			if err := os.WriteFile(foreign, before, 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, ".halro.lock")
			var err error
			if shape == "symlink" {
				err = os.Symlink(foreign, path)
			} else {
				err = os.Link(foreign, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if l, err := Acquire(directory); err == nil || l != nil {
				t.Fatal(l, err)
			}
			after, err := os.ReadFile(foreign)
			if err != nil || string(after) != string(before) {
				t.Fatal("foreign alias rewritten", err)
			}
		})
	}
}

func TestLockConcurrentCloseAndCoverageJoin(t *testing.T) {
	directory := t.TempDir()
	l, err := Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				l.CoversDataDirectory(directory)
			}
		})
		wg.Go(func() {
			if err := l.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if l.CoversDataDirectory(directory) {
		t.Fatal("closed lock covers data directory")
	}
}
