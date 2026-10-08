//go:build linux

package metadatastate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haMetadataPhysicalCrashInput struct {
	metadataPhysicalCrashInput
	Permitted  metadatajournal.Head
	SourceHead metadatajournal.Head
	SourcePath string
}

// The harness supplies a separate authenticated source's confirmation cut.
// This component fixture does not manufacture Member/Ordering Runtime evidence.
func openHAMetadataPhysicalCrash(t *testing.T, input haMetadataPhysicalCrashInput) (*Baseline, ReopenReceipt, func()) {
	t.Helper()
	ctx := context.Background()
	manager := metadataPhysicalBudget(t)
	guard, err := lock.Acquire(filepath.Dir(input.Journal))
	if err != nil {
		t.Fatal(err)
	}
	pc, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: AuthenticatedMetadataSealIndexID, Codec: AuthenticatedMetadataSealCodec}
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: input.Seal.MaxPageBytes, MaxValueBytes: input.Seal.MaxValueBytes, MaxTreeHeight: input.Seal.MaxTreeHeight, RequiredRoots: []authenticatedstate.SealRootSchema{schema}}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(filepath.Dir(input.Journal), guard, input.PublisherKey, hard, manager, pc)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	jc, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	permission := metadatajournal.BoundReplicationAuthority{Baseline: input.Binding, FileRevision: input.Permitted.FileRevision, MaxFileBytes: 64 << 20}
	o := HASelectedSourceOpenOptions{PhysicalTerminalReserve: true, OpenOptions: OpenOptions{BaselineOptions: BaselineOptions{Identity: input.Evidence.Identity, InstanceID: input.Evidence.InstanceID, KeyGeneration: input.Evidence.KeyGeneration, Budget: manager, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20}, Evidence: input.Evidence, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 64 << 20}, Publisher: pub, Guard: guard, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error {
		return visit(ctx, input.Authority, HASelectedMetadataPosition{Epoch: input.Permitted.Epoch, Sequence: input.Permitted.Sequence, Hash: input.Permitted.Hash}, permission)
	}}
	b, receipt, err := OpenHASelectedSourceRoot(ctx, metadataTestKey(), input.Root, input.Journal, o)
	if err != nil {
		t.Fatal("fresh actual bound physical factory", err)
	}
	native := b.journal
	if _, err := b.ConfigureHAReplicaJournal(ctx, metadataTestKey(), permission); err != nil {
		t.Fatal(err)
	}
	if native != b.journal {
		t.Fatal("physical recovery reopened its native owner")
	}
	var once sync.Once
	return b, receipt, func() {
		once.Do(func() {
			if err := b.Close(); err != nil && !errors.Is(err, authenticatedstate.ErrRecovery) {
				t.Error(err)
			}
			if err := errors.Join(pub.Close(), guard.Close(), rc.Release(), jc.Release(), pc.Release()); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestHAMetadataPhysicalSIGKILLRestoresBoundNativeAndPerFrameCOW(t *testing.T) {
	if cfg := os.Getenv("HALRO_HA_METADATA_PHYSICAL_CRASH_CHILD"); cfg != "" {
		raw, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var input haMetadataPhysicalCrashInput
		if err := json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		b, _, closeOwner := openHAMetadataPhysicalCrash(t, input)
		defer closeOwner()
		ctx := context.Background()
		if err := b.EnableHAPhysicalTerminals(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.PrepareHAReplicaPhysicalEscrow(ctx, 2); err != nil {
			t.Fatal(err)
		}
		permission := *b.haReplicaPrepared
		r, err := metadatajournal.NewBoundReplicationRangeReader(input.SourcePath, input.PublisherKey, 1<<20, permission)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := r.Read(permission.Baseline.Epoch, 1, 2)
		err = errors.Join(err, r.Close())
		if err != nil {
			t.Fatal(err)
		}
		if err := b.journal.AppendReplicatedContext(ctx, permission.Baseline.Epoch, 1, 2, frames); err != nil {
			t.Fatal(err)
		}
		phase := os.Getenv("HALRO_HA_METADATA_PHYSICAL_CRASH_PHASE")
		if phase == "first-cow-durable" {
			b.haPhysicalSource.withConfirmedApply = testHAMetadataPhysicalConfirmedSource(input.SourceHead)
			if _, err := b.ReplayTo(ctx, input.SourceHead, 1); err != nil {
				t.Fatal(err)
			}
		}
		if b.journal.Head() != input.SourceHead || (phase == "native-durable" && b.position.Sequence != 0) || (phase == "first-cow-durable" && b.position.Sequence != 1) {
			t.Fatal("actual kill boundary mismatch")
		}
		fmt.Println("actual-ha-metadata-phase", phase)
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("actual isolated ext4 subprocess fixture required")
	}
	for _, phase := range []string{"native-durable", "first-cow-durable"} {
		t.Run(phase, func(t *testing.T) {
			b, f := haPhysicalSelectedFixture(t, true)
			seal, found, err := f.options.Publisher.Current()
			if err != nil || !found {
				t.Fatal(err)
			}
			header, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			sourcePath := filepath.Join(t.TempDir(), "independent-source.journal")
			if err := os.WriteFile(sourcePath, header, 0600); err != nil {
				t.Fatal(err)
			}
			source, err := metadatajournal.OpenBoundContext(context.Background(), sourcePath, f.key, f.head.Baseline, f.head.FileRevision, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				if _, err := source.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte{byte(i)}, Value: []byte("authenticated-before-kill")}}); err != nil {
					t.Fatal(err)
				}
			}
			independent := source.Head()
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			input := haMetadataPhysicalCrashInput{metadataPhysicalCrashInput: metadataPhysicalCrashInput{Root: b.output, Journal: f.path, Evidence: b.evidence, Binding: f.head.Baseline, Seal: seal, Authority: f.options.Authority, PublisherKey: bytes.Clone(f.key)}, Permitted: f.head, SourceHead: independent, SourcePath: sourcePath}
			if err := errors.Join(b.Close(), f.options.Publisher.Close(), f.options.Guard.Close()); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(input)
			cfg := filepath.Join(t.TempDir(), "independent-authority.json")
			if err := os.WriteFile(cfg, raw, 0600); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run", "^TestHAMetadataPhysicalSIGKILLRestoresBoundNativeAndPerFrameCOW$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), "HALRO_HA_METADATA_PHYSICAL_CRASH_CHILD="+cfg, "HALRO_HA_METADATA_PHYSICAL_CRASH_PHASE="+phase)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL || !bytes.Contains(output, []byte("actual-ha-metadata-phase "+phase)) {
				t.Fatal("child missed actual SIGKILL", err, string(output))
			}
			input.Permitted = independent // Separate source's authenticated receipt; never candidate header.
			fresh, receipt, closeFresh := openHAMetadataPhysicalCrash(t, input)
			defer closeFresh()
			// ReopenReceipt counts the complete semantic suffix from the independently
			// selected common, including the already durable COW frame.
			expectedFrames := uint64(2)
			if fresh.position != independent || receipt.AppliedFrames != expectedFrames || fresh.physicalDebt != 0 || fresh.haPhysicalSource.receiveWindow == nil {
				t.Fatal("actual receipt/source per-frame recovery changed", receipt, fresh.position, fresh.physicalDebt)
			}
			if err := fresh.View(context.Background(), func(tx *Transaction) error {
				for i := range 2 {
					value, found, err := tx.Get([]string{"projects"}, []byte{byte(i)})
					if err != nil || !found || string(value) != "authenticated-before-kill" {
						return errors.Join(ErrSource, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			closeFresh()
			again, _, closeAgain := openHAMetadataPhysicalCrash(t, input)
			defer closeAgain()
			if again.position != independent || again.physicalDebt != 0 || again.haPhysicalSource.receiveWindow == nil {
				t.Fatal("zero-debt unpublished recovery lost original receipt")
			}
		})
	}
}
