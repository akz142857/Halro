package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestCOWRootPublicationTracksActualRenameAndCloseDebt(t *testing.T) {
	for _, failure := range []string{"close", "after-rename"} {
		t.Run(failure, func(t *testing.T) {
			s, o := testStoreWithoutCleanup(t, 3)
			beforeRAM := o.Budget.Snapshot().Used.RAM
			boom := errors.New("after actual rename")
			if failure == "close" {
				s.closeRootFile = func(file *os.File) error {
					if err := file.Close(); err != nil {
						return err
					}
					return file.Close()
				}
			} else {
				s.hook = func(phase string) error {
					if phase == "root-renamed" {
						return boom
					}
					return nil
				}
			}
			if _, err := s.Update(context.Background(), nextCut(80), func(tx *Transaction) error { return tx.Put([]byte("new-verified-fact"), []byte("value")) }); err == nil {
				t.Fatal("failed publisher returned success")
			}
			if failure == "close" {
				if _, err := os.Stat(filepath.Join(s.dir, "root.pending")); err != nil {
					t.Fatal("unknown actual close retired pending inode", err)
				}
				for i := 0; i < 2; i++ {
					if err := s.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
						t.Fatal("close debt erased", err)
					}
				}
				if o.Budget.Snapshot().Used.RAM != beforeRAM+UpdateRAMBytes+resourcebudget.ClaimRAMBytes || s.retiredUpdate == (resourcebudget.Permit{}) {
					t.Fatal("unknown root close refunded resident or update work", beforeRAM, o.Budget.Snapshot())
				}
				if err := s.Discard(); !errors.Is(err, ownedfile.ErrRetirement) {
					t.Fatal("discard erased actual close debt", err)
				}
			} else {
				if _, err := os.Stat(filepath.Join(s.dir, "root.pending")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("published pending alias remained", err)
				}
				if _, err := os.Stat(filepath.Join(s.dir, "active.root")); err != nil {
					t.Fatal("published root was removed", err)
				}
				if err := s.Discard(); err != nil {
					t.Fatal("known retired failed candidate cannot be discarded", err)
				}
			}
		})
	}
}

func TestSnapshotConstructorUnknownCloseRetainsOriginalAdmission(t *testing.T) {
	o := storeOptions(t)
	path := filepath.Join(t.TempDir(), "snapshot")
	create := func(path string, flags int, mode os.FileMode) (*os.File, *ownedfile.Owner, error) {
		file, _, err := ownedfile.Create(path, flags, mode)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return nil, nil, errors.Join(ownedfile.ErrRetirement, file.Close())
	}
	next := func() (Row, bool, error) { t.Fatal("failed constructor consumed source"); return Row{}, false, nil }
	result, err := buildSnapshotWithCreate(context.Background(), path, Options{Context: o.Scope, Key: o.Key, MaxDiskBytes: o.MaxDiskBytes, Budget: o.Budget}, next, false, create)
	if result != nil || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("constructor debt disappeared", result, err)
	}
	if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{RAM: BuildRAMBytes + 4096 + resourcebudget.ClaimRAMBytes, Disk: o.MaxDiskBytes}) {
		t.Fatal("nil output refunded actual constructor debt", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unknown output inode disappeared", err)
	}
}

func TestMaterializedImageActualCloseDebtCannotBeRetriedAway(t *testing.T) {
	s, schema, root := imageStore(t, [][]byte{[]byte("abcd")}, nil)
	credit, err := s.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64})
	if err != nil {
		t.Fatal(err)
	}
	image, err := s.MaterializeImage(context.Background(), filepath.Join(t.TempDir(), "image"), schema, root, 4, 64, credit)
	if err != nil {
		t.Fatal(err)
	}
	if err := credit.Release(); err != nil {
		t.Fatal(err)
	}
	before := s.budget.Snapshot().Used
	if err := image.file.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := image.Remove(); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("actual close debt erased", err)
		}
		if s.budget.Snapshot().Used != before {
			t.Fatal("failed close refunded owned image", before, s.budget.Snapshot())
		}
	}
	if _, err := os.Stat(image.path); err != nil {
		t.Fatal("unknown image inode removed", err)
	}
}

func TestCOWSnapshotActualCloseFailureRetainsPinAndResidentFees(t *testing.T) {
	s, o := testStoreWithoutCleanup(t, 3)
	pin, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := o.Budget.Snapshot().Used
	actual := pin.file
	if err := pin.file.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := pin.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("actual pin close failure disappeared", err)
		}
		if o.Budget.Snapshot().Used != before {
			t.Fatal("actual pin close failure refunded its fee", before, o.Budget.Snapshot())
		}
		if pin.file != actual || len(pin.key) != 32 {
			t.Fatal("unknown pin exit lost original descriptor or verification key")
		}
		if _, err := s.Snapshot(context.Background()); !errors.Is(err, ErrRecovery) {
			t.Fatal("unknown pin exit admitted another reader", err)
		}
	}
	if err := s.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("owner close erased pin debt", before, o.Budget.Snapshot())
	}
}

func TestCOWPackActualCloseDebtKeepsDescriptorKeysAndOriginalCredit(t *testing.T) {
	s, o := testStoreWithoutCleanup(t, 3)
	actual := s.pack
	before := o.Budget.Snapshot().Used
	if err := actual.Close(); err != nil {
		t.Fatal(err)
	}
	first := s.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) {
		t.Fatal(first)
	}
	for i := range 2 {
		if s.Close() != first || s.Discard() != first || s.pack != actual || len(s.key) != 32 || o.Budget.Snapshot().Used != before {
			t.Fatal("unknown pack exit lost descriptor, keys or fee", i, o.Budget.Snapshot())
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "pages")); err != nil {
		t.Fatal(err)
	}
}

func TestCOWDiscardDirectoryCloseDebtCannotRetryToSuccess(t *testing.T) {
	s, o := testStoreWithoutCleanup(t, 3)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before := o.Budget.Snapshot().Used
	calls := 0
	s.syncRetiredDirectory = func(path string) error {
		calls++
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return ownedfile.CloseFile(f)
	}
	first := s.Discard()
	if !errors.Is(first, ownedfile.ErrRetirement) || !s.discardDirRetired {
		t.Fatal(first, s.discardDirRetired)
	}
	latched := s.Close()
	for range 2 {
		if s.Discard() != latched || s.Close() != latched || calls != 1 || o.Budget.Snapshot().Used != before {
			t.Fatal("directory exit retried or refunded original cleanup credit", calls, before, o.Budget.Snapshot())
		}
	}
	if _, err := os.Stat(s.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
