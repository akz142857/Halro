//go:build linux

package authenticatedstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func sealCycleBudget(t *testing.T) (*resourcebudget.Manager, resourcebudget.Permit) {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 64 << 20, Disk: 32 << 20}, MaxClaims: 64, MaxOwners: 16, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	permit, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: PhysicalSealDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	return manager, permit
}
func sealCycleKey() []byte { key := make([]byte, 32); key[0] = 11; return key }
func prepareSealCycle(t *testing.T, dir string) {
	t.Helper()
	manager, permit := sealCycleBudget(t)
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	selected := sealCandidateFixture()
	p, err := OpenSealCandidatePublisher(dir, guard, sealCycleKey(), sealHardFor(selected), manager, permit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	if err := p.EnablePhysicalSealPreparation(context.Background(), selected, permit); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestPhysicalSealCycleSIGKILLHelper(t *testing.T) {
	dir := os.Getenv("HALRO_SEAL_CYCLE_DIRECTORY")
	if dir == "" {
		t.Skip("child only")
	}
	manager, permit := sealCycleBudget(t)
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenPhysicalSealCandidatePublisher(context.Background(), dir, guard, sealCycleKey(), sealHardFor(sealCandidateFixture()), manager, permit, sealEligibilityFor(sealCandidateFixture()))
	if err != nil {
		t.Fatal(err)
	}
	selected, found, err := p.Current()
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if err := p.ValidatePhysicalSealPreparation(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv("HALRO_SEAL_CYCLE_KILL")
	p.phase = func(actual string) error {
		if actual == phase {
			fmt.Println("ACTUAL_PHYSICAL_SEAL_PHASE", phase)
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	selected.Revision++
	if _, err := p.Publish(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	if phase != "" {
		t.Fatal("kill phase did not occur", phase)
	}
	if err := p.ValidatePhysicalSealPreparation(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestPhysicalSealCycleActualSIGKILLRestoresAtENOSPC(t *testing.T) {
	for _, phase := range []string{"physical-seal-intent-synced", "seal-file-synced", "seal-renamed", "seal-directory-synced", "physical-seal-renewed"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			requireSealPhysicalExt4(t, dir)
			prepareSealCycle(t, dir)
			identities := map[string]os.FileInfo{}
			for _, name := range []string{"active.seal", "active.seal.pending", "terminal.seal.0", "terminal.seal.1"} {
				info, err := os.Lstat(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				identities[name] = info
			}
			filler := exhaustSealPhysicalExt4(t, dir)
			command := exec.Command(os.Args[0], "-test.run=^TestPhysicalSealCycleSIGKILLHelper$", "-test.count=1", "-test.v")
			command.Env = append(os.Environ(), "HALRO_SEAL_CYCLE_DIRECTORY="+dir, "HALRO_SEAL_CYCLE_KILL="+phase)
			raw, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("actual child did not SIGKILL", string(raw), err)
			}
			t.Log(string(raw))
			recovery := exec.Command(os.Args[0], "-test.run=^TestPhysicalSealCycleSIGKILLHelper$", "-test.count=1", "-test.v")
			recovery.Env = append(os.Environ(), "HALRO_SEAL_CYCLE_DIRECTORY="+dir)
			raw, err = recovery.CombinedOutput()
			if err != nil {
				t.Fatal("independent fresh publisher", string(raw), err)
			}
			for _, name := range []string{"terminal.seal.0", "terminal.seal.1"} {
				after, err := os.Lstat(filepath.Join(dir, name))
				if err != nil || !os.SameFile(after, identities[name]) {
					t.Fatal("receipt control inode changed", name, err)
				}
			}
			for _, name := range []string{"active.seal", "active.seal.pending"} {
				after, err := os.Lstat(filepath.Join(dir, name))
				if err != nil || (!os.SameFile(after, identities["active.seal"]) && !os.SameFile(after, identities["active.seal.pending"])) {
					t.Fatal("actual prepared publication inode lost", name, err)
				}
			}
			if fs := requireSealPhysicalExt4(t, dir); fs.Bavail != 0 {
				t.Fatal("recovery released physical preparation", fs.Bavail)
			}
			if err := os.Remove(filler); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestPhysicalSealCycleForeignTargetsKeepOriginalCredit(t *testing.T) {
	for _, name := range []string{"active.seal.pending", "terminal.seal.0", "terminal.seal.1"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			requireSealPhysicalExt4(t, dir)
			prepareSealCycle(t, dir)
			path := filepath.Join(dir, name)
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
				t.Fatal(err)
			}
			foreign, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			manager, permit := sealCycleBudget(t)
			guard, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			before := manager.Snapshot().Used
			p, err := OpenPhysicalSealCandidatePublisher(context.Background(), dir, guard, sealCycleKey(), sealHardFor(sealCandidateFixture()), manager, permit, sealEligibilityFor(sealCandidateFixture()))
			if err == nil || p != nil {
				t.Fatal("foreign target granted physical permission", err)
			}
			if !errors.Is(err, ownedfile.ErrReplaced) && !errors.Is(err, ErrRecovery) {
				t.Fatal(err)
			}
			after := manager.Snapshot().Used
			if after.Disk < before.Disk {
				t.Fatal("unknown persistent target refunded", before, after)
			}
			actual, err := os.Lstat(path)
			raw, readErr := os.ReadFile(path)
			if err != nil || readErr != nil || !os.SameFile(actual, foreign) || string(raw) != "foreign" {
				t.Fatal("foreign candidate altered", err, readErr)
			}
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
