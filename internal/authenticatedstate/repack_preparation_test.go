package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestRepackReservationIncludesEveryFixedOpaqueSlotAndRejectsOverflow(t *testing.T) {
	ceiling := int64(64 << 10)
	got, err := RepackReservationBytes(ceiling)
	want := ceiling + storeControlDisk + 2*cowRootBytes + 2*repackIntentBytes + repackOpaqueSlots*(repackOpaqueDirectoryDisk+2*repackOpaqueFileDisk)
	if err != nil || got != want {
		t.Fatal(got, want, err)
	}
	for _, invalid := range []int64{0, -1, math.MaxInt64} {
		if _, err := RepackReservationBytes(invalid); !errors.Is(err, ErrLimit) {
			t.Fatal(invalid, err)
		}
	}
}

func TestStoreRepackOpaquePreIntentPreservesOriginalInodesAndCanCommit(t *testing.T) {
	f := newRepackFixture(t)
	f.grow(t)
	ctx := context.Background()
	want := storeRows(t, f.store)
	old := f.store.slotPath(0, "pages")
	oldInfo, err := os.Lstat(old)
	if err != nil {
		t.Fatal(err)
	}
	pages, intent := f.store.slotPath(1, "pages"), f.store.slotPath(1, "repack.intent")
	partial := bytes.Repeat([]byte{0x4c}, repackIntentBytes/2)
	if err := os.WriteFile(pages, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intent, partial, 0600); err != nil {
		t.Fatal(err)
	}
	pageInfo, err := os.Lstat(pages)
	if err != nil {
		t.Fatal(err)
	}
	intentInfo, err := os.Lstat(intent)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
	if err != nil || candidate == nil {
		t.Fatal("real repack did not follow opaque preservation", err)
	}
	for _, original := range []struct {
		name string
		info os.FileInfo
	}{{"pages", pageInfo}, {"repack.intent", intentInfo}} {
		actual, err := os.Lstat(filepath.Join(f.store.dir, "repack.unqualified-0", original.name))
		if err != nil || !os.SameFile(original.info, actual) || original.info.Size() != actual.Size() {
			t.Fatal("opaque inode changed", original.name, err)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(f.store.dir, "repack.unqualified-0", "repack.intent")); err != nil || !bytes.Equal(raw, partial) {
		t.Fatal("opaque bytes changed", err)
	}
	if actual, err := os.Lstat(old); err != nil || !os.SameFile(oldInfo, actual) {
		t.Fatal("selected source retired before common publication", err)
	}
	f.seal.Revision++
	f.seal.Roots[0], err = f.store.SealRootReference(ctx, f.schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.publisher.Publish(ctx, f.seal); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
		t.Fatal("compact common root lost rows")
	}
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old selected source not retired after common publication", err)
	}
	if _, err := os.Lstat(filepath.Join(f.store.dir, "repack.unqualified-0", "pages")); err != nil {
		t.Fatal("commit deleted opaque bytes", err)
	}
}

func TestStoreRepackOpaquePreIntentRejectsForeignAndExhaustedShapes(t *testing.T) {
	for _, fault := range []string{"nonempty-pages", "oversize-intent", "symlink-pages", "hardlink-pages", "public-pages", "control-bearing-candidate", "foreign-slot-symlink", "slots-full", "replaced-pages", "replaced-destination", "wrong-common", "insufficient-reservation"} {
		t.Run(fault, func(t *testing.T) {
			f := newRepackFixture(t)
			f.grow(t)
			ctx := context.Background()
			selectedRoot := f.seal.Roots[0]
			want := storeRows(t, f.store)
			pages, intent := f.store.slotPath(1, "pages"), f.store.slotPath(1, "repack.intent")
			if err := os.WriteFile(pages, nil, 0600); err != nil {
				t.Fatal(err)
			}
			partial := []byte("partial intent without authority")
			if err := os.WriteFile(intent, partial, 0600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(outside, nil, 0600); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "nonempty-pages":
				if err := os.WriteFile(pages, []byte("foreign pages"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize-intent":
				if err := os.WriteFile(intent, make([]byte, repackIntentBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-pages":
				if err := os.Remove(pages); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, pages); err != nil {
					t.Fatal(err)
				}
			case "hardlink-pages":
				if err := os.Remove(pages); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(outside, pages); err != nil {
					t.Fatal(err)
				}
			case "public-pages":
				if err := os.Chmod(pages, 0644); err != nil {
					t.Fatal(err)
				}
			case "control-bearing-candidate":
				if err := os.WriteFile(f.store.slotPath(1, "active.root"), []byte("foreign root"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-slot-symlink":
				if err := os.Symlink(filepath.Dir(outside), filepath.Join(f.store.dir, "repack.unqualified-0")); err != nil {
					t.Fatal(err)
				}
			case "slots-full":
				for i := 0; i < repackOpaqueSlots; i++ {
					path := filepath.Join(f.store.dir, fmt.Sprintf("repack.unqualified-%d", i))
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "preserved"), []byte("never reused"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "replaced-pages", "replaced-destination":
				f.store.hook = func(phase string) error {
					if phase != "repack-opaque-slot-created" {
						return nil
					}
					path := pages
					if fault == "replaced-destination" {
						path = filepath.Join(f.store.dir, "repack.unqualified-0")
					}
					if err := os.Rename(path, path+".original"); err != nil {
						return err
					}
					if fault == "replaced-destination" {
						return os.Mkdir(path, 0700)
					}
					return os.WriteFile(path, nil, 0600)
				}
			case "wrong-common":
				f.seal.Revision++
			case "insufficient-reservation":
				old, err := f.options.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: f.store.ceiling + storeControlDisk + 2*cowRootBytes + 2*repackIntentBytes})
				if err != nil {
					t.Fatal(err)
				}
				defer old.Release()
				f.reservation = old
			}
			candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
			if candidate != nil || err == nil {
				t.Fatal("unsafe pre-intent shape accepted", candidate, err)
			}
			f.store.hook = nil
			if root, err := f.store.SealRootReference(ctx, f.schema); err != nil || root != selectedRoot {
				t.Fatal("refusal changed original source root", err)
			}
			if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
				t.Fatal("refusal changed original source rows")
			}
			if _, err := os.Lstat(intent); err != nil {
				t.Fatal("refusal removed unknown intent", err)
			}
		})
	}
}

func TestStoreRepackCommitRejectsOriginalInodeReplacementAndRetainsFees(t *testing.T) {
	for _, fault := range []string{"valid-pack-replacement", "same-root-replacement", "missing-pack"} {
		t.Run(fault, func(t *testing.T) {
			f := newRepackFixture(t)
			f.grow(t)
			ctx := context.Background()
			candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
			if err != nil || candidate == nil {
				t.Fatal(err)
			}
			f.seal.Revision++
			f.seal.Roots[0], err = f.store.SealRootReference(ctx, f.schema)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.publisher.Publish(ctx, f.seal); err != nil {
				t.Fatal(err)
			}
			name := "pages"
			if fault == "same-root-replacement" {
				name = "active.root"
			}
			path := f.store.slotPath(candidate.old.slot, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if fault != "missing-pack" {
				// This replacement is fully valid under the SAME key/lineage and
				// row digests. A new full audit would accept it; job identity must
				// still refuse to delete this foreign inode.
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := f.options.Budget.Snapshot().Used
			err = candidate.Commit()
			if !errors.Is(err, ownedfile.ErrReplaced) {
				t.Fatal("foreign old-job inode was accepted", err)
			}
			f.expectedCloseErr = ownedfile.ErrReplaced
			if after := f.options.Budget.Snapshot().Used; after != before {
				t.Fatal("uncertain old retirement refunded fees", before, after)
			}
			if saved, err := os.Lstat(path + ".original"); err != nil || !os.SameFile(saved, original) {
				t.Fatal("original inode was lost", err)
			}
			if fault != "missing-pack" {
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
					t.Fatal("foreign replacement deleted or changed", err)
				}
			}
			if _, err := f.store.Snapshot(ctx); err == nil {
				t.Fatal("failed retirement kept business admission")
			}
		})
	}
}

func TestStoreRepackCommitUsesSelectedCopyWithoutReauditingObsoleteRows(t *testing.T) {
	f := newRepackFixture(t)
	f.grow(t)
	ctx := context.Background()
	want := storeRows(t, f.store)
	candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
	if err != nil || candidate == nil {
		t.Fatal(err)
	}
	f.seal.Revision++
	f.seal.Roots[0], err = f.store.SealRootReference(ctx, f.schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.publisher.Publish(ctx, f.seal); err != nil {
		t.Fatal(err)
	}
	// The old reachable bytes were completely authenticated/copied before
	// common selection. An obsolete-body fault afterwards must not force a
	// second history walk or invalidate the already selected complete copy.
	old := f.store.slotPath(candidate.old.slot, "pages")
	if _, err := candidate.old.file.WriteAt([]byte{0x7f}, headerBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := candidate.old.file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, candidate.old.controls[0].ModTime(), candidate.old.controls[0].ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Commit(); err != nil {
		t.Fatal("commit walked obsolete historical data", err)
	}
	if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
		t.Fatal("selected compact copy changed")
	}
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old job source retained", err)
	}
}
