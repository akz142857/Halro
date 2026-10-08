//go:build linux

package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type usagePhysicalSourceAuthority struct {
	Seal      authenticatedstate.CandidateSeal
	Usage     TypedUsageReference
	Ledger    ledger.AuthenticatedLedgerSealReference
	Directory ledger.AuthenticatedDirectoryReference
}

func TestUsagePhysicalSIGKILLHelper(t *testing.T) {
	root := os.Getenv("HALRO_USAGE_PHYSICAL_CRASH_DIR")
	if root == "" {
		t.Skip("subprocess only")
	}
	t.Setenv("HALRO_USAGE_PHYSICAL_ROOT", root)
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	ur, err := f.p.TypedSealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lr, err := f.owner.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := f.directoryLive.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(usagePhysicalSourceAuthority{f.seal, ur, lr, dr})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source-authority.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "begin")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := f.p.ActivateTypedLive(ctx, f.native, f.manager); err != nil {
		t.Fatal(err)
	}
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	for _, r := range finalizationFixture(false, false) {
		if err := f.append(ctx, r.Event); err != nil {
			t.Fatal(err)
		}
	}
	applied := 0
	_, err = f.d.catchUpTypedSourceWithPhase(ctx, f.native, f.manager, nil, func(stage string) {
		if stage == "record-applied" {
			applied++
		}
		stop := os.Getenv("HALRO_USAGE_PHYSICAL_CRASH_STAGE")
		if stop == "native-durable" && stage == "before-record-reduce" || stop == "cow-durable" && stage == "record-applied" && applied == 1 {
			if err := os.WriteFile(filepath.Join(root, "kill-ready"), []byte(stage), 0600); err != nil {
				t.Fatal(err)
			}
			for {
				time.Sleep(time.Second)
			}
		}
	})
	t.Fatal("SIGKILL helper escaped", err)
}
func waitUsagePhysicalFile(t *testing.T, path string) {
	t.Helper()
	limit := time.Now().Add(20 * time.Second)
	for time.Now().Before(limit) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture never reached %s", path)
}
func TestUsagePhysicalSIGKILLRestoreIndependentNativeAndReceiptRoot(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("requires owned isolated ext4")
	}
	for _, stage := range []string{"native-durable", "cow-durable"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "owned")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(exe, "-test.run=^TestUsagePhysicalSIGKILLHelper$", "-test.count=1", "-test.v")
			child.Env = append(os.Environ(), "HALRO_USAGE_PHYSICAL_CRASH_DIR="+root, "HALRO_USAGE_PHYSICAL_CRASH_STAGE="+stage)
			var output bytes.Buffer
			child.Stdout = &output
			child.Stderr = &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			waitUsagePhysicalFile(t, filepath.Join(root, "source-authority.json"))
			var authority usagePhysicalSourceAuthority
			raw, err := os.ReadFile(filepath.Join(root, "source-authority.json"))
			if err != nil || json.Unmarshal(raw, &authority) != nil {
				t.Fatal(err)
			}
			// Capture the actual source publisher's roots before the parent authorizes
			// any append. No post-crash candidate header chooses the recovery authority.
			if authority.Seal.Instance != sha256.Sum256([]byte("actual-selected-usage-physical")) || authority.Seal.Generation != 1 || authority.Seal.Revision != 1 || authority.Ledger.Anchor.Head.Sequence != 0 || authority.Usage.Ledger != authority.Ledger {
				t.Fatal("independent source authority invalid")
			}
			if err := os.WriteFile(filepath.Join(root, "begin"), []byte("parent-authorized"), 0600); err != nil {
				t.Fatal(err)
			}
			waitUsagePhysicalFile(t, filepath.Join(root, "kill-ready"))
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = child.Wait()
			exit, ok := err.(*exec.ExitError)
			if !ok || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("actual SIGKILL required", err, output.String())
			}
			limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1536 << 20, Disk: 512 << 20}, MaxClaims: 96, MaxOwners: 16, MaxPressureAge: time.Hour}
			for i := range limits.Classes {
				limits.Classes[i] = limits.Total
			}
			limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
			limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 96 * resourcebudget.ClaimRAMBytes}
			manager, err := resourcebudget.New(limits)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
				t.Fatal(err)
			}
			reserve := func(n int64) resourcebudget.Permit {
				p, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: n})
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			key := bytes.Repeat([]byte{0x41}, 32)
			identity := authority.Seal.Instance
			guard, err := lock.Acquire(root)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			hard := authenticatedstate.SealHardLimits{MaxPageBytes: 4 << 20, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, RequiredRoots: []authenticatedstate.SealRootSchema{authority.Ledger.Schema, authority.Usage.Projection.Schema, authority.Directory.Schema}}
			publisher, err := authenticatedstate.OpenSealCandidatePublisher(root, guard, key, hard, manager, reserve(32<<10))
			if err != nil {
				t.Fatal(err)
			}
			defer publisher.Close()
			a := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: identity, Incarnation: authority.Seal.Incarnation, Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated}
			copy(a.Positions[:], authority.Seal.Positions[:])
			chosen, found, err := publisher.Select(a)
			if err != nil || !found || chosen != authority.Seal {
				t.Fatal("actual common publisher changed", found, err)
			}
			capacity := &usagePhysicalCapacity{}
			directoryOptions := ledger.AuthenticatedDirectoryOptions{Identity: identity, Budget: manager, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20, DiskReservation: reserve(16 << 20)}
			native, _, err := ledger.OpenWithSelectedSeal(ctx, filepath.Join(root, "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: key, ReserveAppendDisk: capacity.reserve, MaxBatch: 1}, ledger.SelectedSealOpenOptions{Publisher: publisher, Guard: guard, Authority: a, Directory: filepath.Join(root, "directory"), DirectoryOptions: directoryOptions})
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			if err := native.AuditSelectedStartup(ctx, manager); err != nil {
				t.Fatal(err)
			}
			lease, err := native.BeginMaintenance(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			head, hash, _ := native.ChainHead()
			var completionRAM int64
			for _, r := range ledger.CompletionWorkspaceRequirements() {
				completionRAM += r.RAM
			}
			source := ledger.SelectedLedgerProjectionOpenOptions{Publisher: publisher, Guard: guard, Authority: a, PhysicalRecovery: &ledger.PhysicalStartupRecoveryOptions{GrowthBytes: 64 << 20, GrowthFee: reserve(64 << 20), Capacity: capacity, CompletionRAM: completionRAM}}
			lo := ledger.AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: ledger.AuthenticatedLedgerOptions{Identity: identity, MaxDiskBytes: 32 << 20, MaxAppendBytes: 2 << 20, Budget: manager, Prefix: &ledger.ProjectionPrefix{Head: head, Hash: hash}}, SavedAnchor: authority.Ledger.Anchor, DiskReservation: reserve(32 << 20)}
			owner, _, err := ledger.OpenSelectedAuthenticatedLedgerProjection(ctx, native, filepath.Join(root, "ledger-root"), lo, source)
			if err != nil {
				t.Fatal("actual Ledger restore", err)
			}
			defer owner.Close()
			options := AuthenticatedUsageOptions{Native: native, Owner: owner, Identity: identity, UsageKey: bytes.Repeat([]byte{0x42}, 32), Budget: manager, RootDirectory: filepath.Join(root, "usage"), RootMaxDiskBytes: 256 << 20, RootMaxAppendBytes: 4 << 20, PhysicalTerminalReserve: true}
			p, err := OpenSelectedTypedUsageProjection(ctx, options, lease, authority.Usage, reserve(256<<20), source)
			if err != nil {
				t.Fatal("actual Usage native/receipt restore", err)
			}
			defer p.Close()
			actual, err := p.ReadReplayAnchor(ctx)
			if err != nil || actual.Head != head || actual.Hash != hash {
				t.Fatal("restored Usage did not reach complete real native", actual, head, err)
			}
			stats, err := p.ReadStats(ctx)
			if err != nil || stats.Metrics.CostMicrosUSD != 7 {
				t.Fatal(fmt.Sprint("restored balances", stats, err))
			}
			if p.physical == nil || p.physical.remaining != 0 {
				t.Fatal("original actual prepaid targets not consumed", p.physical)
			}
		})
	}
}
