package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootIdentityTracksActualCommitmentAndRejectsUnverifiedOrClosedState(t *testing.T) {
	s, o := testStore(t, 5)
	before, err := s.RootIdentity(context.Background())
	if err != nil || before.Scope != o.Scope || before.Cut != o.ExpectedCut || before.Rows != 5 || before.Revision != 1 || before.Digest == ([32]byte{}) || before.ID == ([16]byte{}) {
		t.Fatal(before, err)
	}
	if _, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error { return tx.Delete([]byte("key_000001")) }); err != nil {
		t.Fatal(err)
	}
	after, err := s.RootIdentity(context.Background())
	if err != nil || after.Rows != 4 || after.Revision != 2 || after.Cut != nextCut(1) || after.Digest == before.Digest || after.ID != before.ID {
		t.Fatal(after, before, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := s.RootIdentity(ctx); !errors.Is(err, context.Canceled) || got != (RootIdentity{}) {
		t.Fatal(got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RootIdentity(context.Background()); !errors.Is(err, ErrClosed) || got != (RootIdentity{}) {
		t.Fatal(got, err)
	}
}
func TestRootIdentityCorruptionPoisonsReadersWithoutReturningReceipt(t *testing.T) {
	for _, name := range []string{"active.root", "pages"} {
		t.Run(name, func(t *testing.T) {
			s, _ := testStore(t, 5)
			path := filepath.Join(s.dir, name)
			f, err := os.OpenFile(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			var value [1]byte
			if _, err := f.ReadAt(value[:], 0); err != nil {
				t.Fatal(err)
			}
			value[0] ^= 1
			_, writeErr := f.WriteAt(value[:], 0)
			if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
				t.Fatal(err)
			}
			if got, err := s.RootIdentity(context.Background()); !errors.Is(err, ErrCorrupt) || got != (RootIdentity{}) {
				t.Fatal(got, err)
			}
			if _, err := s.Snapshot(context.Background()); err == nil {
				t.Fatal("corrupt root remained readable")
			}
			// Restore this private fixture's original byte solely so the owner's
			// digest-checked cleanup can retire it. No reader fault is cleared.
			if name == "active.root" {
				f, err := os.OpenFile(path, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				value[0] ^= 1
				_, writeErr := f.WriteAt(value[:], 0)
				if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
