package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestPublisherAdoptsOnlyClosedBorrowedSuccessorAndRetiresActualDisk(t *testing.T) {
	owner, o := testStore(t, 20)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := owner.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	o.DiskReservation = credit
	next, err := OpenStore(context.Background(), owner.dir, o)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	baseline := o.Budget.Snapshot().Used.Disk
	if _, err := next.Update(context.Background(), nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("key_000001"), []byte("updated")) }); err != nil {
		t.Fatal(err)
	}
	if err := owner.AdoptClosedSuccessor(next); !errors.Is(err, ErrRecovery) {
		t.Fatal("active successor adopted", err)
	}
	if err := next.Discard(); !errors.Is(err, ErrLimit) {
		t.Fatal("borrower acquired deletion authority", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Discard(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("unadopted successor removed", err)
	}
	if _, err := os.Stat(filepath.Join(owner.dir, "pages")); err != nil {
		t.Fatal("refusal destroyed successor pack", err)
	}
	if err := owner.AdoptClosedSuccessor(next); err != nil {
		t.Fatal(err)
	}
	if got := o.Budget.Snapshot().Used.Disk; got != baseline {
		t.Fatal("adoption changed disk credit", got, baseline)
	}
	if err := owner.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owner.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("namespace not retired", err)
	}
	if got := o.Budget.Snapshot().Used.Disk; got != baseline {
		t.Fatal("external publisher credit retired early", got)
	}
	if err := credit.Release(); err != nil {
		t.Fatal(err)
	}
	credit = resourcebudget.Permit{}
	if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal(got)
	}
}

func TestPublisherDiscardPreflightsForeignRootAndNamesBeforeRemovingPack(t *testing.T) {
	for _, shape := range []string{"extra", "root_replaced", "root_modified"} {
		t.Run(shape, func(t *testing.T) {
			s, _ := testStore(t, 1)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(s.dir, "active.root")
			raw, err := os.ReadFile(root)
			if err != nil {
				t.Fatal(err)
			}
			baseline := s.budget.Snapshot().Used
			switch shape {
			case "extra":
				if err := os.WriteFile(filepath.Join(s.dir, "foreign"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "root_replaced":
				if err := os.Rename(root, root+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(root, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "root_modified":
				changed := append([]byte(nil), raw...)
				changed[0] ^= 1
				if err := os.WriteFile(root, changed, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Discard(); err == nil {
				t.Fatal("foreign namespace removed")
			}
			if _, err := os.Stat(filepath.Join(s.dir, "pages")); err != nil {
				t.Fatal("pack removed before complete preflight", err)
			}
			if got := s.budget.Snapshot().Used; got != baseline {
				t.Fatal("disk refunded on refusal", got)
			}
			switch shape {
			case "extra":
				os.Remove(filepath.Join(s.dir, "foreign"))
			case "root_replaced":
				os.Remove(root)
				os.Rename(root+".saved", root)
			case "root_modified":
				os.WriteFile(root, raw, 0600)
			}
		})
	}
}

func TestPublisherAdoptionRefusesForeignScopeAndChangedClosedRoot(t *testing.T) {
	owner, o := testStore(t, 1)
	foreign, _ := testStore(t, 1)
	owner.Close()
	foreign.Close()
	if err := owner.AdoptClosedSuccessor(foreign); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	next, err := OpenStore(context.Background(), owner.dir, o)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.Close()
	guard, err := lock.AcquireExistingReadOnly(owner.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.AdoptClosedSuccessor(next); !errors.Is(err, lock.ErrAlreadyLocked) {
		t.Fatal("existing writer ignored", err)
	}
	guard.Close()
	path := filepath.Join(owner.dir, "active.root")
	raw, _ := os.ReadFile(path)
	changed := append([]byte(nil), raw...)
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.AdoptClosedSuccessor(next); !errors.Is(err, ErrCorrupt) {
		t.Fatal("changed closed receipt adopted", err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
