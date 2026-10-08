//go:build linux || darwin

package authenticatedstate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestStoreRepackCrashHelper(t *testing.T) {
	phase, directory := os.Getenv("HALRO_REPACK_CRASH_PHASE"), os.Getenv("HALRO_REPACK_CRASH_DIRECTORY")
	if phase == "" {
		t.Skip("subprocess-only repack crash helper")
	}
	ctx := context.Background()
	o := storeOptions(t)
	o.MaxDiskBytes, o.MaxAppendBytes = 64<<10, 16<<10
	guard, err := lock.Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	fixture := sealCandidateFixture()
	fixture.Roots[0].Kind = SealRootState
	p, err := OpenSealCandidatePublisher(directory, guard, o.Key, sealHardFor(fixture), o.Budget, sealPublisherReservation(t, o.Budget))
	if err != nil {
		t.Fatal(err)
	}
	selected, found, err := p.Current()
	if err != nil || !found {
		t.Fatal(err)
	}
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, IndexID: 1, Codec: 1}
	o.ExpectedCut, o.ExpectedSealRoot, o.ExpectedSealSchema = selected.Roots[0].State.Cut, &selected.Roots[0], &schema
	o.DiskReservation, err = o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(ctx, filepath.Join(directory, "state"), o)
	if err != nil {
		t.Fatal(err)
	}
	if nativePath := os.Getenv("HALRO_REPACK_PHYSICAL_NATIVE"); nativePath != "" {
		encoded, err := json.Marshal(selected)
		if err != nil {
			t.Fatal(err)
		}
		native, verify := repackPhysicalNativeFixture(t, nativePath)
		if _, err = s.EnablePhysicalTerminalQualified(ctx, sha256.Sum256(encoded), native, verify); err != nil {
			t.Fatal(err)
		}
	}
	s.hook = func(reached string) error {
		if reached == phase {
			if err := os.WriteFile(filepath.Join(directory, "crash-reached"), []byte(phase), 0600); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			for {
				time.Sleep(time.Hour)
			}
		}
		return nil
	}
	reservation, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: s.RepackDiskBytes()})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.PrepareRepack(ctx, p, guard, selected, schema, reservation)
	if err != nil || candidate == nil {
		t.Fatal("prepare crash candidate", err)
	}
	selected.Revision++
	selected.Roots[0], err = s.SealRootReference(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(ctx, selected); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("planned crash was not reached")
}

func repackPhysicalNativeFixture(t *testing.T, path string) (PhysicalNativeReference, func(PhysicalNativeReference, int64) error) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	identity, err := resourcebudget.CapturePhysicalIdentity(f)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := physicalPathIdentity(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	reference := PhysicalNativeReference{File: identity, Parent: parent, Generation: 1}
	return reference, func(prior PhysicalNativeReference, end int64) error {
		if prior != reference || end != 0 {
			return ErrSealIneligible
		}
		return resourcebudget.CheckPhysicalTail(f, 0, end)
	}
}

func TestStoreRepackActualSIGKILLSelectsCompletePackAndRetries(t *testing.T) {
	for _, phase := range []string{"repack-pages-created", "repack-intent-created", "repack-intent-partially-written", "repack-intent-written", "repack-intent-synced", "repack-pages-synced", "repack-roots-synced", "repack-common-selected", "repack-old-pack-retired"} {
		t.Run(phase, func(t *testing.T) {
			f := newRepackFixture(t)
			f.grow(t)
			want := storeRows(t, f.store)
			directory := filepath.Dir(f.store.dir)
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.publisher.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.guard.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreRepackCrashHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_REPACK_CRASH_PHASE="+phase, "HALRO_REPACK_CRASH_DIRECTORY="+directory)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || ctx.Err() != nil {
				t.Fatalf("child failed before planned crash: %v %s", err, output)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child was not SIGKILLed: %v %s", err, output)
			}
			if reached, err := os.ReadFile(filepath.Join(directory, "crash-reached")); err != nil || string(reached) != phase {
				t.Fatal("unproven crash point", err)
			}
			guard, err := lock.Acquire(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			p, err := OpenSealCandidatePublisher(directory, guard, f.options.Key, sealHardFor(f.seal), f.options.Budget, sealPublisherReservation(t, f.options.Budget))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			selected, found, err := p.Current()
			if err != nil || !found {
				t.Fatal(err)
			}
			o := f.options
			o.ExpectedRoot = nil
			o.ExpectedCut, o.ExpectedSealRoot, o.ExpectedSealSchema = selected.Roots[0].State.Cut, &selected.Roots[0], &f.schema
			f.store, err = OpenStore(context.Background(), f.store.dir, o)
			if err != nil {
				t.Fatal("reopen actual common-selected pack", err)
			}
			if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
				t.Fatal("crash lost or changed authenticated rows")
			}
			candidate, err := f.store.PrepareRepack(context.Background(), p, guard, selected, f.schema, f.reservation)
			if err != nil {
				t.Fatal("retry interrupted fixed-slot copy", err)
			}
			if candidate != nil {
				if err := candidate.Rollback(); err != nil {
					t.Fatal("retry rollback", err)
				}
			}
			if phase == "repack-pages-created" || phase == "repack-intent-created" || phase == "repack-intent-partially-written" {
				opaque := filepath.Join(f.store.dir, "repack.unqualified-0")
				if info, err := os.Lstat(filepath.Join(opaque, "pages")); err != nil || info.Size() != 0 {
					t.Fatal("earliest pages inode not preserved", info, err)
				}
				if phase != "repack-pages-created" {
					info, err := os.Lstat(filepath.Join(opaque, "repack.intent"))
					if err != nil || info.Size() >= repackIntentBytes {
						t.Fatal("partial intent not preserved as opaque", info, err)
					}
				}
			}
			if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
				t.Fatal("retry lost authenticated rows")
			}
		})
	}
}

func runRepackCrashProcess(t *testing.T, directory, phase string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreRepackCrashHelper$", "-test.count=1")
	child.Env = append(os.Environ(), "HALRO_REPACK_CRASH_PHASE="+phase, "HALRO_REPACK_CRASH_DIRECTORY="+directory)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || ctx.Err() != nil {
		t.Fatalf("crash helper failed before %s: %v %s", phase, err, output)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("not actual SIGKILL at %s: %v %s", phase, err, output)
	}
	if raw, err := os.ReadFile(filepath.Join(directory, "crash-reached")); err != nil || string(raw) != phase {
		t.Fatal("unproven crash point", phase, err)
	}
}

func TestStoreRepackOpaquePreservationActualSIGKILLRetriesAndKeepsSourceInode(t *testing.T) {
	for _, phase := range []string{"repack-opaque-slot-created", "repack-opaque-pages-preserved", "repack-opaque-intent-preserved"} {
		t.Run(phase, func(t *testing.T) {
			f := newRepackFixture(t)
			f.grow(t)
			want := storeRows(t, f.store)
			sourcePath := f.store.slotPath(f.store.packSlot, "pages")
			sourceInfo, err := os.Lstat(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			sourceBytes, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Dir(f.store.dir)
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.publisher.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.guard.Close(); err != nil {
				t.Fatal(err)
			}
			runRepackCrashProcess(t, directory, "repack-intent-partially-written")
			partial, err := os.ReadFile(f.store.slotPath(1, "repack.intent"))
			if err != nil {
				t.Fatal(err)
			}
			runRepackCrashProcess(t, directory, phase)
			guard, err := lock.Acquire(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			p, err := OpenSealCandidatePublisher(directory, guard, f.options.Key, sealHardFor(f.seal), f.options.Budget, sealPublisherReservation(t, f.options.Budget))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			selected, found, err := p.Current()
			if err != nil || !found {
				t.Fatal(err)
			}
			o := f.options
			o.ExpectedRoot = nil
			o.ExpectedCut, o.ExpectedSealRoot, o.ExpectedSealSchema = selected.Roots[0].State.Cut, &selected.Roots[0], &f.schema
			f.store, err = OpenStore(context.Background(), f.store.dir, o)
			if err != nil {
				t.Fatal("selected source failed to reopen", err)
			}
			candidate, err := f.store.PrepareRepack(context.Background(), p, guard, selected, f.schema, f.reservation)
			if err != nil || candidate == nil {
				t.Fatal("earliest preservation retry failed to prepare real repack", err)
			}
			if err := candidate.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
				t.Fatal("original selected rows changed")
			}
			info, err := os.Lstat(sourcePath)
			if err != nil || !os.SameFile(sourceInfo, info) {
				t.Fatal("source inode changed", err)
			}
			bytes, err := os.ReadFile(sourcePath)
			if err != nil || !reflect.DeepEqual(bytes, sourceBytes) {
				t.Fatal("source bytes changed", err)
			}
			foundIntent := false
			for i := 0; i < repackOpaqueSlots; i++ {
				raw, err := os.ReadFile(filepath.Join(f.store.dir, fmt.Sprintf("repack.unqualified-%d", i), "repack.intent"))
				if err == nil && reflect.DeepEqual(raw, partial) {
					foundIntent = true
				}
			}
			if !foundIntent {
				t.Fatal("partial intent was altered or lost")
			}
		})
	}
}
