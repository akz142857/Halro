//go:build linux

package authenticatedstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPhysicalPendingRenameSIGKILLHelper(t *testing.T) {
	path, phase := os.Getenv("HALRO_PHYSICAL_PENDING_FIXTURE"), os.Getenv("HALRO_PHYSICAL_PENDING_PHASE")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f physicalReceiptCrashFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	o := storeOptions(t)
	o.ExpectedRoot = &f.Root
	o.DiskReservation, err = o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, f.Directory, o)
	if err != nil {
		t.Fatal(err)
	}
	native, verify := receiptNativeFixture(t, f.Native)
	if _, err = s.EnablePhysicalTerminalQualified(ctx, f.Selection, native, verify); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.hook = func(at string) error {
		if at == phase {
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	var requested *PhysicalTerminalWindow
	if os.Getenv("HALRO_PHYSICAL_PENDING_ORDINARY") == "" {
		requested = w
	}
	u, err := s.PreparePhysicalUpdate(ctx, requested)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateNativeFrame(u.Bind(ctx), nextCut(77), func(tx *Transaction) error { return tx.Put([]byte("rename-replayed"), []byte("exact independent cut")) }, func() error { return s.phase("component-native-durable") })
	if err = errors.Join(err, u.Finish(true, err)); err != nil {
		t.Fatal(err)
	}
	t.Fatal("SIGKILL boundary not reached")
}

// Actual subprocess death covers both ordinary and terminal candidates. The
// fixed independent fixture cut is component evidence, not Runtime authority.
func TestPhysicalPendingRenameActualSIGKILL(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	phases := []string{"physical-pending-intent-synced", "physical-pending-renamed-synced", "component-native-durable", "physical-pending-root-intent-synced", "root-synced", "root-renamed", "directory-synced"}
	for _, ordinary := range []bool{false, true} {
		for _, phase := range phases {
			t.Run(phase+map[bool]string{false: "-terminal", true: "-ordinary"}[ordinary], func(t *testing.T) {
				runPhysicalPendingRenameCase(t, phase, ordinary, "")
			})
		}
	}
	for _, mutation := range []string{"foreign-pending", "missing-pending", "both-names"} {
		t.Run(mutation, func(t *testing.T) {
			runPhysicalPendingRenameCase(t, "physical-pending-renamed-synced", false, mutation)
		})
	}
}

func runPhysicalPendingRenameCase(t *testing.T, phase string, ordinary bool, mutation string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	creator, o := testStoreWithoutCleanup(t, 3)
	selected, err := creator.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentUsage, IndexID: 1, Codec: 1}
	saved, err := creator.SealRootReference(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err = creator.Close(); err != nil {
		t.Fatal(err)
	}
	nativePath := filepath.Join(filepath.Dir(creator.dir), "native")
	if err = os.WriteFile(nativePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f := physicalReceiptCrashFixture{Directory: creator.dir, Native: nativePath, Root: selected, Selection: nextCut(99)}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(creator.dir), "fixture.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPhysicalPendingRenameSIGKILLHelper$", "-test.count=1")
	child.Env = append(os.Environ(), "HALRO_PHYSICAL_PENDING_FIXTURE="+path, "HALRO_PHYSICAL_PENDING_PHASE="+phase)
	if ordinary {
		child.Env = append(child.Env, "HALRO_PHYSICAL_PENDING_ORDINARY=1")
	}
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("not actual death: %v %s", err, output)
	}
	pending := filepath.Join(creator.dir, "root.pending")
	if mutation != "" {
		if mutation == "foreign-pending" || mutation == "missing-pending" {
			if err = os.Rename(pending, pending+".original"); err != nil {
				t.Fatal(err)
			}
			if mutation == "foreign-pending" {
				if err = os.WriteFile(pending, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			if err = os.WriteFile(filepath.Join(creator.dir, "terminal-root-00"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	o.ExpectedRoot, o.ExpectedSealRoot, o.ExpectedSealSchema, o.PhysicalRecovery = &selected, &saved, &schema, true
	s, err := OpenStore(ctx, creator.dir, o)
	if err != nil {
		t.Fatal(err)
	}
	native, verify := receiptNativeFixture(t, nativePath)
	cut := o.ExpectedCut
	published := phase == "root-renamed" || phase == "directory-synced"
	if published {
		var found bool
		cut, found, err = s.WithPhysicalRecoveryRoot(ctx, f.Selection, native, func(pin *Snapshot, comparison [32]byte, prior PhysicalNativeReference) ([32]byte, error) {
			value, exists, e := pin.Get(ctx, []byte("rename-replayed"))
			if e != nil || !exists || string(value) != "exact independent cut" || comparison != nextCut(77) {
				return [32]byte{}, errors.Join(ErrRecovery, e)
			}
			return nextCut(77), verify(prior, 0)
		})
		if err != nil || !found {
			t.Fatal("published root lost original ownership", found, err)
		}
	}
	w, _, found, err := s.RestorePhysicalTerminalForRecovery(ctx, f.Selection, native, cut, verify)
	if mutation != "" {
		if err == nil {
			t.Fatal("foreign/missing name lent target")
		}
		if mutation == "foreign-pending" {
			if _, e := os.Stat(pending); e != nil {
				t.Fatal("foreign candidate deleted", e)
			}
		}
		if closeErr := s.Close(); closeErr != nil && !errors.Is(closeErr, ErrRecovery) && !errors.Is(closeErr, ownedfile.ErrReplaced) {
			t.Fatal(closeErr)
		}
		return
	}
	count := 2
	if ordinary {
		count = 3
	}
	if published {
		count--
	}
	if err != nil || !found || w.Count() != count {
		t.Fatal("exact retained window lost", found, w, err)
	}
	if !published {
		u, e := s.PreparePhysicalUpdate(ctx, w)
		if e != nil {
			t.Fatal("actual named candidate reborrow", e)
		}
		_, e = s.UpdateNativeFrame(u.Bind(ctx), nextCut(77), func(tx *Transaction) error { return tx.Put([]byte("rename-replayed"), []byte("exact independent cut")) }, func() error { return nil })
		if e = errors.Join(e, u.Finish(true, e)); e != nil {
			t.Fatal("original prepaid replay", e)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}
