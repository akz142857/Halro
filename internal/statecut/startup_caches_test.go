package statecut

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func persistedCommonStartupCaches(t *testing.T, f *commonFixture) StartupCacheCandidates {
	t.Helper()
	return persistStartupCachesAt(t, f.owners, f.authority.Instance, f.dir, "candidate")
}

func persistStartupCachesAt(t *testing.T, owners StandaloneOwners, identity [32]byte, directory, suffix string) StartupCacheCandidates {
	t.Helper()
	ctx := context.Background()
	ac, err := owners.Audit.CaptureStartupCache(ctx, owners.Budget, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	gc, err := owners.NativeGovernance.CaptureStartupCache(ctx, owners.Budget, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	ad, err := owners.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: audit.MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ad.Release(); err != nil {
			t.Error(err)
		}
	})
	gd, err := owners.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: governance.MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gd.Release(); err != nil {
			t.Error(err)
		}
	})
	af, err := ac.PersistNew(ctx, filepath.Join(directory, "audit-"+suffix+".cache"), audit.StartupCachePersistOptions{Guard: owners.Guard, Budget: owners.Budget, DiskReservation: ad, Identity: identity, Head: ac.Head()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := af.Discard(); err != nil {
			t.Error(err)
		}
	})
	gf, err := gc.PersistNew(ctx, filepath.Join(directory, "governance-"+suffix+".cache"), governance.StartupCachePersistOptions{Guard: owners.Guard, Budget: owners.Budget, DiskReservation: gd, Identity: identity, Head: gc.Head()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gf.Discard(); err != nil {
			t.Error(err)
		}
	})
	return StartupCacheCandidates{Audit: af, Governance: gf}
}

func TestCommonStandalonePublishesWithActualDurableStartupCaches(t *testing.T) {
	f := newCommonFixture(t)
	caches := persistedCommonStartupCaches(t, f)
	fees := f.owners.Budget.Snapshot().Used
	publication, err := PublishStandaloneWithStartupCaches(context.Background(), f.owners, f.base, f.authority, caches)
	if err != nil || publication.Seal.RootCount != 7 || publication.CleanupError() != nil {
		t.Fatal("actual durable startup cache publication", publication, err)
	}
	if got := f.owners.Budget.Snapshot().Used; got != fees {
		t.Fatal("whole file loans leaked or refunded persistent fees", got, fees)
	}
	if selected, found, err := f.owners.Publisher.Select(f.authority); err != nil || !found || selected != publication.Seal {
		t.Fatal("actual common selection", found, err)
	}
	// RAM capsules have already closed; the durable creation owners remain.
	for _, path := range []string{caches.Audit.Path(), caches.Governance.Path()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("publication consumed an immutable startup source", err)
		}
	}
}

func TestCommonStandaloneStartupCacheTamperAndClosedOwnerRefusePublication(t *testing.T) {
	for _, fault := range []string{"audit-bytes", "governance-bytes", "audit-replaced", "governance-closed", "missing"} {
		t.Run(fault, func(t *testing.T) {
			f := newCommonFixture(t)
			caches := persistedCommonStartupCaches(t, f)
			path := caches.Audit.Path()
			if fault == "governance-bytes" {
				path = caches.Governance.Path()
			}
			switch fault {
			case "audit-bytes", "governance-bytes":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Error(err)
					}
				})
				bad := append([]byte(nil), raw...)
				bad[len(bad)-1] ^= 1
				if err := os.WriteFile(path, bad, 0600); err != nil {
					t.Fatal(err)
				}
			case "audit-replaced":
				if err := os.Rename(path, path+".owned"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(path); err != nil {
						t.Error(err)
					}
					if err := os.Rename(path+".owned", path); err != nil {
						t.Error(err)
					}
				})
			case "governance-closed":
				if err := caches.Governance.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing":
				caches.Audit = nil
			}
			fees := f.owners.Budget.Snapshot().Used
			publication, err := PublishStandaloneWithStartupCaches(context.Background(), f.owners, f.base, f.authority, caches)
			if err == nil || publication.Seal != (authenticatedstate.CandidateSeal{}) || publication.Receipt != (authenticatedstate.SealPublicationReceipt{}) {
				t.Fatal("unqualified persistent source published", fault, publication, err)
			}
			if got := f.owners.Budget.Snapshot().Used; got != fees {
				t.Fatal("rejected candidate changed fees", got, fees)
			}
			if _, found, err := f.owners.Publisher.Current(); err != nil || found {
				t.Fatal("rejected candidate entered durable namespace", found, err)
			}
			if fault == "missing" && !errors.Is(err, authenticatedstate.ErrSealIneligible) {
				t.Fatal(err)
			}
		})
	}
}
