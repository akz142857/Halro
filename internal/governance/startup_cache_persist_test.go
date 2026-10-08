package governance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func persistFixtureCache(t *testing.T, f *startupCacheFixture) (*StartupCacheFile, StartupCachePersistOptions) {
	t.Helper()
	disk, err := f.options.CacheBudget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	o := StartupCachePersistOptions{Guard: f.selected.Guard, Budget: f.options.CacheBudget, DiskReservation: disk, Identity: f.selected.Identity, Head: f.cache.Head()}
	file, err := f.cache.PersistNew(context.Background(), filepath.Join(filepath.Dir(f.path), "retry-candidate"), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Release(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Discard(); err != nil && !(f.persistDebt && errors.Is(err, ownedfile.ErrRetirement)) {
			t.Error(err)
		}
	})
	return file, o
}

func TestStartupCachePersistActualFileAndFrozenOwner(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	file, _ := persistFixtureCache(t, f)
	ctx := context.Background()
	budget := f.options.CacheBudget
	if err := file.WithFrozenOwner(ctx, f.native, budget, file.Identity(), file.Head(), func(context.Context) error { t.Fatal("unfrozen callback"); return nil }); !errors.Is(err, ErrStartupCache) {
		t.Fatal(err)
	}
	called := false
	if err := f.native.WithFrozenStandaloneHead(ctx, budget, func(ctx context.Context, head Summary) error {
		if err := file.WithFrozenOwner(ctx, f.native, budget, [32]byte{8}, head, func(context.Context) error { t.Fatal("wrong identity callback"); return nil }); !errors.Is(err, ErrStartupCache) {
			return err
		}
		return file.WithFrozenOwner(ctx, f.native, budget, file.Identity(), head, func(context.Context) error { called = true; return nil })
	}); err != nil || !called {
		t.Fatal(called, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.WithFrozenOwner(ctx, f.native, budget, file.Identity(), file.Head(), func(context.Context) error { return nil }); !errors.Is(err, ErrStartupCache) {
		t.Fatal("closed reader qualified", err)
	}
}

func TestStartupCachePersistRejectsExistingAndKnownCreatedFailure(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	file, o := persistFixtureCache(t, f)
	disk, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Release()
	o.DiskReservation = disk
	before, err := os.ReadFile(file.Path())
	if err != nil {
		t.Fatal(err)
	}
	if extra, err := f.cache.PersistNew(context.Background(), file.Path(), o); extra != nil || !errors.Is(err, os.ErrExist) {
		t.Fatal(extra, err)
	}
	after, err := os.ReadFile(file.Path())
	if err != nil || string(before) != string(after) {
		t.Fatal("existing candidate changed", err)
	}
	path := filepath.Join(filepath.Dir(f.path), "rejected-candidate")
	reject := errors.New("known pre-sync rejection")
	used := o.Budget.Snapshot().Used
	if result, err := f.cache.persistNew(context.Background(), path, o, func(stage string, _ *os.File) error {
		if stage == "before-file-sync" {
			return reject
		}
		return nil
	}); result != nil || !errors.Is(err, reject) {
		t.Fatal(result, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created rejected file not retired", err)
	}
	if got := o.Budget.Snapshot().Used; got != used {
		t.Fatal("known rejection leaked", got, used)
	}
}

func TestStartupCacheFileLoanDetectsActualContentReplacement(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	file, _ := persistFixtureCache(t, f)
	raw, err := os.ReadFile(file.Path())
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(file.Path(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.native.WithFrozenStandaloneHead(context.Background(), f.options.CacheBudget, func(ctx context.Context, head Summary) error {
		err := file.WithFrozenOwner(ctx, f.native, f.options.CacheBudget, file.Identity(), head, func(context.Context) error { t.Fatal("tampered candidate callback"); return nil })
		if !errors.Is(err, ErrStartupCache) {
			t.Fatalf("tamper qualified: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupCacheFileCloseCancelsAndJoinsActualLoan(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	file, _ := persistFixtureCache(t, f)
	entered, leave := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.native.WithFrozenStandaloneHead(context.Background(), f.options.CacheBudget, func(ctx context.Context, head Summary) error {
			return file.WithFrozenOwner(ctx, f.native, f.options.CacheBudget, file.Identity(), head, func(ctx context.Context) error { close(entered); <-ctx.Done(); <-leave; return ctx.Err() })
		})
	}()
	<-entered
	used := f.options.CacheBudget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- file.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close did not join loan", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := f.options.CacheBudget.Snapshot().Used; got != used {
		t.Fatal("live loan fee released", got, used)
	}
	close(leave)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStartupCachePersistUncertainActualCloseKeepsCreationAndWholeFee(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	disk, err := f.options.CacheBudget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	o := StartupCachePersistOptions{Guard: f.selected.Guard, Budget: f.options.CacheBudget, DiskReservation: disk, Identity: f.selected.Identity, Head: f.cache.Head()}
	before := o.Budget.Snapshot().Used
	file, err := f.cache.persistNew(context.Background(), filepath.Join(filepath.Dir(f.path), "uncertain-candidate"), o, func(stage string, actual *os.File) error {
		if stage == "before-close" {
			return actual.Close()
		}
		return nil
	})
	if file == nil || !errors.Is(err, ownedfile.ErrRetirement) || file.file == nil || file.owner == nil {
		t.Fatal(file, err)
	}
	if got := o.Budget.Snapshot().Used; got.RAM < before.RAM+StartupCachePersistRAMBytes {
		t.Fatal("uncertain actual close refunded whole job", got, before)
	}
	if err := file.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if err := file.Discard(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(file.Path()); err != nil {
		t.Fatal("uncertain inode deleted", err)
	}
	if err := disk.Release(); err != nil {
		t.Fatal(err)
	}
	f.persistDebt = true
}

func TestStartupCacheFileLoanUncertainActualCloseKeepsCapsuleAndWholeFee(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	file, _ := persistFixtureCache(t, f)
	budget := f.options.CacheBudget
	before := budget.Snapshot().Used
	called := false
	err := f.native.WithFrozenStandaloneHead(context.Background(), budget, func(ctx context.Context, head Summary) error {
		return file.withFrozenOwner(ctx, f.native, budget, file.Identity(), head, func(context.Context) error { called = true; return nil }, func(actual *os.File) {
			if err := actual.Close(); err != nil {
				t.Error(err)
			}
		})
	})
	if !called || !errors.Is(err, ownedfile.ErrRetirement) || file.file == nil || file.retainedCache == nil || file.retainedCache.closed {
		t.Fatal("actual loan retirement lost its reader/capsule", called, err)
	}
	f.persistDebt = true
	if got := budget.Snapshot().Used; got.RAM < before.RAM+StartupCachePersistRAMBytes+startupCacheRAMBytes || got.Disk != before.Disk {
		t.Fatal("actual loan retirement refunded work or immutable creation", got, before)
	}
	if err := file.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if err := file.Discard(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(file.Path()); err != nil {
		t.Fatal("uncertain actual loan let creation retire", err)
	}
}

func TestStartupCacheFileLoanRejectsIdenticalBytesAtReplacementInode(t *testing.T) {
	for _, duringLoan := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-loan", true: "after-callback"}[duringLoan], func(t *testing.T) {
			f := newStartupCacheFixture(t, 3)
			file, _ := persistFixtureCache(t, f)
			raw, err := os.ReadFile(file.Path())
			if err != nil {
				t.Fatal(err)
			}
			original := file.Path() + ".original"
			replace := func() {
				if err := os.Rename(file.Path(), original); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file.Path(), raw, 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Rename(original, file.Path()); err != nil {
						t.Error(err)
					}
				})
			}
			if !duringLoan {
				replace()
			}
			budget := f.options.CacheBudget
			before := budget.Snapshot().Used
			called := false
			err = f.native.WithFrozenStandaloneHead(context.Background(), budget, func(ctx context.Context, head Summary) error {
				return file.WithFrozenOwner(ctx, f.native, budget, file.Identity(), head, func(context.Context) error {
					called = true
					replace()
					return nil
				})
			})
			if err == nil || called != duringLoan {
				t.Fatal("copied bytes replaced actual creation evidence", called, err)
			}
			if got := budget.Snapshot().Used; got != before {
				t.Fatal("known replacement refusal changed fees", got, before)
			}
			if got, err := os.ReadFile(file.Path()); err != nil || string(got) != string(raw) {
				t.Fatal("reader altered the replacement inode", err)
			}
		})
	}
}
