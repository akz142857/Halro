package governance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestStartupCacheReadonlyFrozenReuseKeepsOriginalFileAndFees(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	budget := f.options.CacheBudget
	disk, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Release()
	o := StartupCachePersistOptions{Guard: f.selected.Guard, Budget: budget, DiskReservation: disk, Identity: f.selected.Identity, Head: f.cache.Head()}
	path := filepath.Join(filepath.Dir(f.path), "reuse-immutable")
	created, err := f.cache.PersistNew(ctx, path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer created.Discard()
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if reader, err := f.cache.ReuseFrozenFile(ctx, f.native, path, o); reader != nil || !errors.Is(err, ErrStartupCache) {
		t.Fatal("unfrozen reader", reader, err)
	}
	before := budget.Snapshot()
	for range 6 {
		err = f.native.WithFrozenStandaloneHead(ctx, budget, func(ctx context.Context, head Summary) error {
			reader, err := f.cache.ReuseFrozenFile(ctx, f.native, path, o)
			if err != nil {
				return err
			}
			if err := reader.Discard(); !errors.Is(err, ErrStartupCache) {
				t.Fatal("reader granted deletion", err)
			}
			if err := reader.ReplacePendingCandidate(ctx, path, original); !errors.Is(err, ErrStartupCache) {
				t.Fatal("reader granted replacement", err)
			}
			called := false
			loanErr := reader.WithFrozenOwner(ctx, f.native, budget, o.Identity, head, func(context.Context) error { called = true; return nil })
			if !called {
				t.Fatal("actual native/file loan did not run", loanErr)
			}
			return errors.Join(loanErr, reader.Close())
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if after := budget.Snapshot(); after.Used != before.Used || after.CompletionReserved != before.CompletionReserved || after.NormalClaims != before.NormalClaims {
		t.Fatal("readonly reuse changed original fees", before, after)
	}
	if actual, err := os.Stat(path); err != nil || !os.SameFile(original, actual) {
		t.Fatal("reuse replaced original inode", err)
	}
	err = f.native.WithFrozenStandaloneHead(ctx, budget, func(ctx context.Context, head Summary) error {
		reader, err := f.cache.ReuseFrozenFile(ctx, f.native, path, o)
		if err != nil {
			return err
		}
		defer reader.Close()
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		moved := path + ".original"
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		defer func() { _ = os.Remove(path); _ = os.Rename(moved, path) }()
		if err := os.WriteFile(path, raw, 0600); err != nil {
			return err
		}
		if err := reader.WithFrozenOwner(ctx, f.native, budget, o.Identity, head, func(context.Context) error { t.Fatal("replacement callback ran"); return nil }); !errors.Is(err, ownedfile.ErrReplaced) {
			t.Fatal("identical foreign inode reused", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStartupCacheReadonlyReuseUnknownLoanCloseHoldsOriginalFees(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	budget := f.options.CacheBudget
	disk, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Release()
	o := StartupCachePersistOptions{Guard: f.selected.Guard, Budget: budget, DiskReservation: disk, Identity: f.selected.Identity, Head: f.cache.Head()}
	path := filepath.Join(filepath.Dir(f.path), "reuse-unknown")
	created, err := f.cache.PersistNew(ctx, path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer created.Discard()
	var reader *StartupCacheFile
	before := budget.Snapshot().Used
	err = f.native.WithFrozenStandaloneHead(ctx, budget, func(ctx context.Context, head Summary) error {
		var err error
		reader, err = f.cache.ReuseFrozenFile(ctx, f.native, path, o)
		if err != nil {
			return err
		}
		return reader.withFrozenOwner(ctx, f.native, budget, o.Identity, head, func(context.Context) error { return nil }, func(actual *os.File) {
			if err := actual.Close(); err != nil {
				t.Error(err)
			}
		})
	})
	if !errors.Is(err, ownedfile.ErrRetirement) || reader == nil || reader.file == nil || reader.retainedCache == nil {
		t.Fatal("unknown actual reader lost", reader, err)
	}
	if err := reader.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("unknown close refunded child", err)
	}
	if reader.reservationReturned {
		t.Fatal("unknown reader returned original Disk loan")
	}
	if after := budget.Snapshot().Used; after.RAM < before.RAM+StartupCachePersistRAMBytes+startupCacheRAMBytes || after.Disk != before.Disk {
		t.Fatal("unknown reader refunded work", before, after)
	}
	f.persistDebt = true
}
