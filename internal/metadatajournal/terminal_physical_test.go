package metadatajournal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
)

func physicalLoanJournal(t *testing.T) *Log {
	t.Helper()
	l := retirementJournal(t)
	guard, err := lock.Acquire(filepath.Dir(l.path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Error(err)
		}
	})
	l.startupGuard = guard
	l.startupBudget = metadataFrozenBudget(t)
	return l
}
func TestMetadataPhysicalActualParentCloseDebtPreservesOwnerAndKey(t *testing.T) {
	l := physicalLoanJournal(t)
	actual := l.file
	err := l.WithPhysicalNative(context.Background(), l.startupBudget, func(q *PhysicalNativeLoan) error {
		return l.physical.parent.Close() // actual descriptor fails the deferred Stat/Close
	})
	if !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	err = l.Close()
	if !errors.Is(err, ownedfile.ErrRetirement) || len(l.key) == 0 || l.physical == nil || l.file != nil {
		t.Fatal("actual owner lost", err, l.file == actual)
	}
	if l.Close() != err || !errors.Is(l.Discard(), ownedfile.ErrRetirement) {
		t.Fatal("retirement retried")
	}
	if _, err := os.Stat(l.path); err != nil {
		t.Fatal("source inode removed", err)
	}
}
func TestMetadataPhysicalLoanCloseJoinsCallbackAndPreservesForeignName(t *testing.T) {
	l := physicalLoanJournal(t)
	entered, release := make(chan struct{}), make(chan struct{})
	result, closed := make(chan error, 1), make(chan error, 1)
	var escaped *PhysicalNativeLoan
	go func() {
		result <- l.WithPhysicalNative(context.Background(), l.startupBudget, func(q *PhysicalNativeLoan) error { escaped = q; close(entered); <-release; return nil })
	}()
	<-entered
	go func() { closed <- l.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close escaped real callback", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := os.Rename(l.path, l.path+".original"); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign native must survive")
	if err := os.WriteFile(l.path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if err := <-closed; !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if _, err := escaped.Reference(); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal("escaped loan revived", err)
	}
	raw, err := os.ReadFile(l.path)
	if err != nil || string(raw) != string(foreign) {
		t.Fatal("foreign removed", err)
	}
	if len(l.key) == 0 || l.physical == nil {
		t.Fatal("key/owner lost")
	}
}
