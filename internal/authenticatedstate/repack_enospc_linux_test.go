//go:build linux

package authenticatedstate

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"testing"
)

// E1 point 1 (board §40): the compact candidate is written beside the selected
// pack. Real ENOSPC while copying pages or writing the candidate root must
// leave the selected pack, its rows and the budget whole; once space returns
// the same Store repacks and reopens to the same directory shape as a clean run.
func TestStoreRepackAtActualENOSPCKeepsSelectedPack(t *testing.T) {
	control := repackENOSPCRun(t, "")
	for _, phase := range []string{"repack-pages-created", "repack-pages-synced"} {
		t.Run(phase, func(t *testing.T) {
			if got := repackENOSPCRun(t, phase); !reflect.DeepEqual(got, control) {
				t.Fatal("recovered directory differs from a clean repack", got, control)
			}
		})
	}
}

type selectedPackFile struct {
	ino    uint64
	size   int64
	digest [32]byte
}

func readSelectedPackFile(t *testing.T, path string) selectedPackFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return selectedPackFile{ino: info.Sys().(*syscall.Stat_t).Ino, size: info.Size(), digest: sha256.Sum256(b)}
}

func storeDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != "foreign-space-filler" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// repackENOSPCRun returns the store directory after a completed repack cycle.
// An empty phase is the uninjected control.
func repackENOSPCRun(t *testing.T, phase string) []string {
	f := newRepackFixture(t)
	requireSealPhysicalExt4(t, f.store.dir)
	ctx := context.Background()
	f.grow(t)
	want := storeRows(t, f.store)
	selectedPages, selectedRoot := f.store.slotPath(f.store.packSlot, "pages"), f.store.slotPath(f.store.packSlot, "active.root")
	pagesBefore, rootBefore := readSelectedPackFile(t, selectedPages), readSelectedPackFile(t, selectedRoot)
	before := f.options.Budget.Snapshot().Used
	if phase != "" {
		filler := ""
		f.store.hook = func(at string) error {
			if at == phase && filler == "" {
				filler = exhaustSealPhysicalExt4(t, filepath.Dir(f.store.dir))
			}
			return nil
		}
		candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
		f.store.hook = nil
		if filler == "" {
			t.Fatal("injection phase not reached", phase)
		}
		if candidate != nil || !errors.Is(err, syscall.ENOSPC) {
			t.Fatal("repack at actual ENOSPC", candidate != nil, err)
		}
		if readSelectedPackFile(t, selectedPages) != pagesBefore || readSelectedPackFile(t, selectedRoot) != rootBefore {
			t.Fatal("selected pack changed by a failed candidate")
		}
		if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
			t.Fatal("failed repack changed authenticated rows")
		}
		t.Log("after ENOSPC", phase, "names", storeDirNames(t, f.store.dir), "used", f.options.Budget.Snapshot().Used, "before", before)
		if err := os.Remove(filler); err != nil {
			t.Fatal(err)
		}
	}
	candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
	if err != nil || candidate == nil {
		t.Fatal("repack after space returned", err)
	}
	f.seal.Revision++
	if f.seal.Roots[0], err = f.store.SealRootReference(ctx, f.schema); err != nil {
		t.Fatal(err)
	}
	if _, err := f.publisher.Publish(ctx, f.seal); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Commit(); err != nil {
		t.Fatal("commit after space returned", err)
	}
	if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
		t.Fatal("repack changed authenticated rows")
	}
	if after := f.options.Budget.Snapshot().Used; after != before {
		t.Fatal("budget leaked or double charged", before, after)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	o := f.options
	o.ExpectedRoot = nil
	o.ExpectedCut = f.seal.Roots[0].State.Cut
	o.ExpectedSealRoot, o.ExpectedSealSchema = &f.seal.Roots[0], &f.schema
	if f.store, err = OpenStore(ctx, f.store.dir, o); err != nil {
		t.Fatal("reopen after recovered repack", err)
	}
	if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
		t.Fatal("reopen lost rows")
	}
	return storeDirNames(t, f.store.dir)
}
