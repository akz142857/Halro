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

func TestSealFileActualIdentityAndUnknownCloseRejectResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.seal")
	key := make([]byte, 32)
	key[0] = 17
	seal := sealCandidateFixture()
	hard := sealHardFor(seal)
	raw, err := EncodeSealCandidate(seal, key, hard)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, info, digest, err := readSealFileWithOpen(path, key, hard, func(name string) (*os.File, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return f, nil
	})
	if !errors.Is(err, ErrSealFileRetirement) || got != (CandidateSeal{}) || info != nil || digest != ([32]byte{}) {
		t.Fatal("unknown actual close returned a seal", err)
	}
	other := filepath.Join(dir, "other.seal")
	if err := os.WriteFile(other, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readSealFileWithOpen(path, key, hard, func(string) (*os.File, error) { return os.Open(other) }); !errors.Is(err, ErrCorrupt) {
		t.Fatal("foreign actual FD accepted", err)
	}
	link := filepath.Join(dir, "link.seal")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readSealFileWithOpen(link, key, hard, func(string) (*os.File, error) { t.Fatal("symlink reached open"); return nil, nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}

func TestSealPublisherConstructorDebtRetainsPublisherAndWriteFees(t *testing.T) {
	dir := t.TempDir()
	budget := testBudget(t)
	reservation := sealPublisherReservation(t, budget)
	defer reservation.Release()
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	key := make([]byte, 32)
	key[0] = 21
	p, err := OpenSealCandidatePublisher(dir, guard, key, sealHardFor(sealCandidateFixture()), budget, reservation)
	if err != nil {
		t.Fatal(err)
	}
	p.createPending = func(name string) (*os.File, *ownedfile.Owner, error) {
		file, _, err := ownedfile.Create(name, os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return nil, nil, errors.Join(ownedfile.ErrRetirement, file.Close())
	}
	before := budget.Snapshot().Used
	if receipt, err := p.Publish(context.Background(), sealCandidateFixture()); receipt != (SealPublicationReceipt{}) || !errors.Is(err, ErrSealFileRetirement) {
		t.Fatal("unknown pending constructor granted publication", receipt, err)
	}
	if got := budget.Snapshot().Used; got.RAM != before.RAM+candidateSealMaxBytes+resourcebudget.ClaimRAMBytes || got.Disk != before.Disk {
		t.Fatal("failed constructor refunded IO owner", before, got)
	}
	if err := p.Close(); !errors.Is(err, ErrSealFileRetirement) {
		t.Fatal(err)
	}
	if err := p.Retire(); !errors.Is(err, ErrSealFileRetirement) {
		t.Fatal("publisher retired unknown pending inode", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "active.seal.pending")); err != nil {
		t.Fatal(err)
	}
}
func TestSealPublisherActualCloseAndRenamedPendingKeepWorkspace(t *testing.T) {
	for _, mode := range []string{"close", "renamed"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			budget := testBudget(t)
			reservation := sealPublisherReservation(t, budget)
			defer reservation.Release()
			guard, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			key := make([]byte, 32)
			key[0] = 18
			var actual *os.File
			boom := errors.New("stop after actual close")
			p, err := openSealCandidatePublisher(dir, guard, key, sealHardFor(sealCandidateFixture()), budget, reservation, func(point string) error {
				if point != "seal-file-synced" {
					return nil
				}
				if mode == "close" {
					if err := actual.Close(); err != nil {
						return err
					}
					return boom
				}
				pending := filepath.Join(dir, "active.seal.pending")
				if err := os.Rename(pending, pending+".escaped"); err != nil {
					return err
				}
				return os.WriteFile(pending, []byte("foreign"), 0600)
			})
			if err != nil {
				t.Fatal(err)
			}
			p.createPending = func(path string) (*os.File, *ownedfile.Owner, error) {
				f, owner, err := ownedfile.Create(path, os.O_RDWR, 0600)
				actual = f
				return f, owner, err
			}
			before := budget.Snapshot().Used
			receipt, err := p.Publish(context.Background(), sealCandidateFixture())
			if !errors.Is(err, ErrSealFileRetirement) || receipt != (SealPublicationReceipt{}) {
				t.Fatal("uncertain cleanup returned success", err)
			}
			if mode == "close" && !errors.Is(err, boom) {
				t.Fatal("original phase failure lost", err)
			}
			pending := filepath.Join(dir, "active.seal.pending")
			if _, err := os.Lstat(pending); err != nil {
				t.Fatal("uncertain inode removed", err)
			}
			if mode == "renamed" {
				if _, err := os.Lstat(pending + ".escaped"); err != nil {
					t.Fatal("renamed original removed", err)
				}
				raw, err := os.ReadFile(pending)
				if err != nil || string(raw) != "foreign" {
					t.Fatal("foreign pending removed", err)
				}
			}
			if _, err := os.Lstat(filepath.Join(dir, "active.seal")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial output published", err)
			}
			snapshot := budget.Snapshot()
			if snapshot.Classes[resourcebudget.IO].RAM != candidateSealMaxBytes || snapshot.Used.Disk != before.Disk {
				t.Fatal("unknown actual retirement refunded", snapshot)
			}
			if !errors.Is(p.Close(), ErrSealFileRetirement) || !errors.Is(p.Close(), ErrSealFileRetirement) || !errors.Is(p.Retire(), ErrSealFileRetirement) {
				t.Fatal("retirement debt forgotten")
			}
			if budget.Snapshot().Classes[resourcebudget.IO].RAM != candidateSealMaxBytes {
				t.Fatal("repeated close refunded unknown IO")
			}
		})
	}
}
