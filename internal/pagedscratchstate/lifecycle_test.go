package pagedscratchstate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestScratchActualCloseFailureKeepsOwnerKeyAndFees(t *testing.T) {
	for _, kind := range []string{"nodes", "directory"} {
		t.Run(kind, func(t *testing.T) {
			s, m := testStore(t, 64<<20)
			actual := s.files[0]
			if kind == "directory" {
				actual = s.directory
			}
			if err := actual.Close(); err != nil {
				t.Fatal(err)
			}
			before := m.Snapshot().Used
			err := s.Discard()
			var owner *Failure
			if !errors.Is(err, ownedfile.ErrRetirement) || !errors.As(err, &owner) || owner.Owner != s {
				t.Fatal(err)
			}
			if s.key == ([32]byte{}) || s.mac == nil || m.Snapshot().Used != before {
				t.Fatal("unknown Close refunded actual owner", m.Snapshot())
			}
			if again := s.Close(); !errors.Is(again, ownedfile.ErrRetirement) {
				t.Fatal(again)
			}
			if err := s.View(context.Background(), func(*Transaction) error { t.Fatal("closed owner admitted reader"); return nil }); !errors.Is(err, ErrPoisoned) {
				t.Fatal(err)
			}
			if _, err := os.Stat(s.dir); err != nil {
				t.Fatal("unknown owner unlinked", err)
			}
		})
	}
}

func TestScratchLargeDiskCeilingKeepsFixedWorkspaceAndLimitPoisons(t *testing.T) {
	s, m := testStore(t, 1<<30)
	if used := m.Snapshot().Used; used.RAM != WorkspaceRAMBytes+2*resourcebudget.ClaimRAMBytes || used.Disk != 1<<30 {
		t.Fatal(used)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if s.mac != nil || s.key != ([32]byte{}) || s.macInput != ([MaxKeyBytes + 512]byte{}) || s.macOutput != ([32]byte{}) {
		t.Fatal("known physical Close retained authentication workspace")
	}
	tiny, _ := testStore(t, 3*pageBytes)
	err := tiny.Update(context.Background(), func(tx *Transaction) error {
		if err := tx.Put([]byte("first"), nil); err != nil {
			return err
		}
		for i := 0; i <= maxEntries; i++ {
			if err := tx.Put([]byte{byte(i + 1)}, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if err := tiny.View(context.Background(), func(*Transaction) error { t.Fatal("partially mutated candidate trusted"); return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal(err)
	}
}

func TestScratchBoundaryCursorsMatchOrderedRows(t *testing.T) {
	s, _ := testStore(t, 64<<20)
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	if err := s.Update(context.Background(), func(tx *Transaction) error {
		for _, key := range keys {
			if err := tx.Put(key, key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(context.Background(), func(tx *Transaction) error {
		for _, item := range []struct {
			probe         string
			floor, strict bool
			want          string
		}{{"", false, false, "a"}, {"a", false, false, "a"}, {"a", false, true, "b"}, {"b", true, true, "a"}, {"bb", true, false, "b"}, {"bb", false, false, "c"}, {"z", true, false, "c"}, {"z", false, false, ""}, {"", true, false, ""}} {
			row, found, err := tx.boundary([]byte(item.probe), item.floor, item.strict)
			if err != nil || found != (item.want != "") || found && !bytes.Equal(row.Key, []byte(item.want)) {
				t.Fatalf("cursor %+v got %q %v %v", item, row.Key, found, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScratchScopedTransactionAndPanicCannotAuthorizeCandidate(t *testing.T) {
	s, _ := testStore(t, 64<<20)
	var escaped *Transaction
	if err := s.View(context.Background(), func(tx *Transaction) error { escaped = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := escaped.Get([]byte("key")); !errors.Is(err, ErrPoisoned) {
		t.Fatal("expired transaction reused", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("callback panic missing")
			}
		}()
		_ = s.Update(context.Background(), func(tx *Transaction) error {
			if err := tx.Put([]byte("partial"), nil); err != nil {
				t.Fatal(err)
			}
			panic("fault after physical write")
		})
	}()
	if err := s.View(context.Background(), func(*Transaction) error { t.Fatal("panicked candidate allowed export"); return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal(err)
	}
}
