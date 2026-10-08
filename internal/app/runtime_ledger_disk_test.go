package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func ledgerAppendDiskConfigFixture() config.ResourceBudget {
	b := runtimeInventoryBudgetFixture()
	b.LedgerAppendDiskBytes = 1 << 20
	b.RecoveryClaims++
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	return b
}

func ledgerAppendOwnerFixture(t *testing.T, limit int64) (*runtimeLedgerAppendDiskOwner, *resourcebudget.Manager) {
	t.Helper()
	cfg := testConfig(t)
	b := ledgerAppendDiskConfigFixture()
	b.LedgerAppendDiskBytes = limit
	cfg.ResourceBudget = &b
	manager, err := resourcebudget.New(b.Limits(runtimePressureFixture().MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := prepareRuntimeLedgerAppendDisk(context.Background(), cfg, &resourcePressureRuntime{budget: manager})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.publish(); err != nil {
		t.Fatal(err)
	}
	return owner, manager
}

func nativeFrameFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	l, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("frame-000")}, ledger.Options{MaxBatch: 1})
	if l.ActiveBytes() == 0 {
		t.Fatal("missing actual MAC frame")
	}
	frames, err := ledger.ReadReplicationFrames(path, key, 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	return frames, key
}

func TestLedgerAppendDiskStandaloneRejectsBeforeWriteAndStaysHealthy(t *testing.T) {
	owner, manager := ledgerAppendOwnerFixture(t, 1)
	path := filepath.Join(t.TempDir(), "ledger.wal")
	status := ledger.NewStatus()
	l, err := ledger.OpenWithOptions(path, status, ledger.Options{ChainKey: bytes.Repeat([]byte{0x61}, 32), ReserveAppendDisk: owner.reserve, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("frame-000")); !errors.Is(err, errLedgerAppendDiskLimit) || !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("unfunded native append accepted", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || status.Load() != ledger.AccountingHealthy || l.ActiveBytes() != 0 || owner.consumed() != 0 {
		t.Fatalf("definite refusal changed native head/health %v %v %d", err, status.Load(), owner.consumed())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot().Used != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: 1}) {
		t.Fatal("persistent native growth reservation refunded on Close", manager.Snapshot())
	}
}

func TestLedgerAppendDiskReplicaRejectsBeforeWriteAndDoesNotChargeRetry(t *testing.T) {
	frames, key := nativeFrameFixture(t)
	for _, enough := range []bool{false, true} {
		t.Run(map[bool]string{false: "reject", true: "retry"}[enough], func(t *testing.T) {
			limit := int64(len(frames))
			if !enough {
				limit--
			}
			owner, _ := ledgerAppendOwnerFixture(t, limit)
			path := filepath.Join(t.TempDir(), "ledger.wal")
			seed, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
			if err != nil {
				t.Fatal(err)
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}
			status := ledger.NewStatus()
			l, err := ledger.OpenWithOptions(path, status, ledger.Options{ChainKey: key, Replica: true, ReserveAppendDisk: owner.reserve})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			err = l.AppendReplicated(1, 1, 1, frames)
			if !enough {
				actual, readErr := os.ReadFile(path)
				if !errors.Is(err, errLedgerAppendDiskLimit) || readErr != nil || len(actual) != 0 || status.Load() != ledger.AccountingHealthy || owner.consumed() != 0 {
					t.Fatalf("replica quota refusal changed bytes %v %v %v", err, readErr, status.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if owner.consumed() != int64(len(frames)) {
				t.Fatal("MAC frame not charged")
			}
			if err := l.AppendReplicated(1, 1, 1, frames); err != nil {
				t.Fatal(err)
			}
			if owner.consumed() != int64(len(frames)) {
				t.Fatal("durable duplicate consumed new Disk capacity")
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, frames) {
				t.Fatal("replica writes differ from actual source", err)
			}
		})
	}
}

type ledgerDiskPartialWriter struct{ file *os.File }

func (w ledgerDiskPartialWriter) Write(raw []byte) (int, error) {
	n, err := w.file.Write(raw[:1])
	return n, errors.Join(err, syscall.ENOSPC)
}
func (w ledgerDiskPartialWriter) Sync() error { return w.file.Sync() }

func TestLedgerAppendDiskPartialActualWriteKeepsWholeCapacity(t *testing.T) {
	frames, key := nativeFrameFixture(t)
	owner, manager := ledgerAppendOwnerFixture(t, int64(len(frames)))
	path := filepath.Join(t.TempDir(), "ledger.wal")
	status := ledger.NewStatus()
	l, err := ledger.OpenWithOptions(path, status, ledger.Options{ChainKey: key, ReserveAppendDisk: owner.reserve, MaxBatch: 1, WrapDurability: func(f *os.File) ledger.DurabilityWriter { return ledgerDiskPartialWriter{file: f} }})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("frame-000")); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal(err)
	}
	physical, err := os.Stat(path)
	if err != nil || physical.Size() != 1 || owner.consumed() != int64(len(frames)) || status.Load() != ledger.AccountingUnavailable {
		t.Fatalf("unknown actual write refunded possible full bytes %v %+v %d %v", err, physical, owner.consumed(), status.Load())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot().Used; got != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: int64(len(frames))}) {
		t.Fatalf("unknown native tail fees lost after Close %+v", got)
	}
}

func TestLedgerAppendDiskTwoRollsDoNotResetCapacity(t *testing.T) {
	frames, key := nativeFrameFixture(t)
	owner, _ := ledgerAppendOwnerFixture(t, 3*int64(len(frames)))
	path := filepath.Join(t.TempDir(), "ledger.wal")
	status := ledger.NewStatus()
	l, err := ledger.OpenWithOptions(path, status, ledger.Options{ChainKey: key, ReserveAppendDisk: owner.reserve, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, id := range []string{"frame-000", "frame-001"} {
		if _, err := l.Append(context.Background(), projectedWriterAcceptance(id)); err != nil {
			t.Fatal(err)
		}
		if result, err := l.Roll(); err != nil || !result.Rolled {
			t.Fatalf("actual Roll %v %+v", err, result)
		}
	}
	if l.Generation() != 3 || owner.consumed() != 2*int64(len(frames)) {
		t.Fatalf("roll discarded monotonic native capacity gen=%d used=%d", l.Generation(), owner.consumed())
	}
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("frame-002")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("frame-003")); !errors.Is(err, errLedgerAppendDiskLimit) {
		t.Fatal("generation got new native growth ceiling", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || status.Load() != ledger.AccountingHealthy || owner.consumed() != 3*int64(len(frames)) {
		t.Fatalf("new generation bypassed common growth cap %v %v %d", err, status.Load(), owner.consumed())
	}
}

type ledgerDiskHAAdmission struct{ released atomic.Int64 }

func (a *ledgerDiskHAAdmission) Release() error { a.released.Add(1); return nil }

func TestLedgerAppendDiskPrewriteRefusalReleasesOnlyUnusedHAAdmission(t *testing.T) {
	owner, _ := ledgerAppendOwnerFixture(t, 1)
	admission := &ledgerDiskHAAdmission{}
	var ordered atomic.Int64
	l, _, _ := projectedWriterLog(t, nil, ledger.Options{MaxBatch: 1, ReserveAppendDisk: owner.reserve, BeforeDurable: func(ledger.DurableBatch) (ledger.AppendAdmission, error) { return admission, nil }, AfterDurable: func(ledger.DurableBatch) (uint64, error) { ordered.Add(1); return 1, nil }, WaitConfirmed: func(context.Context, uint64) error { return nil }})
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("frame-000")); !errors.Is(err, errLedgerAppendDiskLimit) {
		t.Fatal(err)
	}
	if admission.released.Load() != 1 || ordered.Load() != 0 || l.ActiveBytes() != 0 {
		t.Fatalf("native quota changed HA ordering or leaked unused confirmation admission released=%d ordered=%d", admission.released.Load(), ordered.Load())
	}
}

func TestLedgerAppendDiskDefaultRuntimeOwnsActualAccountingWritesAndClose(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		pressure, b := runtimePressureFixture(), ledgerAppendDiskConfigFixture()
		cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	})
	if r.ledgerAppendDisk == nil || r.persistentInventory == nil {
		t.Fatal("actual default Runtime omitted native growth/inventory owner")
	}
	manager := r.resourcePressure.logicalBudget()
	baseline := r.persistentInventory.Snapshot().ChargeBytes
	ceiling := r.config.ResourceBudget.LedgerAppendDiskBytes
	if manager.Snapshot().Classes[resourcebudget.Background].Disk != baseline+ceiling {
		t.Fatal("native growth does not share actual baseline Manager", manager.Snapshot())
	}
	initial := r.ledgerAppendDisk.consumed()
	request, err := r.accounting.BeginRequestDetailed(context.Background(), bootstrap.ProjectID, bootstrap.KeyID, "native-disk-request", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Finalize(context.Background(), request, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if r.ledgerAppendDisk.consumed() <= initial {
		t.Fatal("actual accounting frames bypassed funded native append gate")
	}
	beforeRoll := r.ledgerAppendDisk.consumed()
	if result, err := r.ledger.Roll(); err != nil || !result.Rolled {
		t.Fatal("actual default Runtime Roll", result, err)
	}
	if r.ledgerAppendDisk.consumed() != beforeRoll+4*(64<<10) {
		t.Fatal("actual default Runtime omitted manifest/successor/sidecar admissions", r.ledgerAppendDisk.consumed(), beforeRoll)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot().Used; got != (resourcebudget.Cost{RAM: 2 * resourcebudget.ClaimRAMBytes, Disk: baseline + ceiling}) {
		t.Fatalf("default Runtime Close refunded native tree/growth fees %+v", got)
	}
	if err := r.ledgerAppendDisk.reserve(1); !errors.Is(err, resourcebudget.ErrClosed) {
		t.Fatal("closed Runtime left native growth admission live", err)
	}
}

func TestLedgerAppendDiskActualPrimaryReplicaShareFundedNativePath(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	seed, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	b := runtimeCandidateBudgetFixture()
	b.PersistentInventoryDiskBytes, b.LedgerAppendDiskBytes = 64<<20, 1<<20
	b.TotalDiskBytes, b.BackgroundDiskBytes = 4<<30, 1<<30
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 1<<30, 1<<30
	b.LedgerProjectionDiskBytes, b.LedgerSegmentIndexDiskBytes = 64<<20, 64<<20
	b.UsageProjectionDiskBytes, b.UsageArchiveDiskBytes, b.IODiskBytes = 64<<20, 32<<20, 64<<20
	addProjectionMappingFixtureRAM(t, &b)
	b.RecoveryRAMBytes += (512 << 20) + diskinventory.WorkspaceRAMBytes
	b.RecoveryClaims, b.MaxClaims = 32, 48
	b.ControlRAMBytes = 48 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes = b.RecoveryRAMBytes + 32*resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += (512 << 20) + diskinventory.WorkspaceRAMBytes + 48*resourcebudget.ClaimRAMBytes
	pressure := runtimePressureFixture()
	cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	primary, replica := openPrimaryDiskPair(t, cfg)
	for _, r := range []*Runtime{primary, replica} {
		if r.ledgerAppendDisk == nil || r.persistentInventory == nil || !r.ledgerAppendDisk.published {
			t.Fatal("actual HA Runtime omitted native ownership")
		}
	}
	beforePrimary, beforeReplica := primary.ledgerAppendDisk.consumed(), replica.ledgerAppendDisk.consumed()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := primary.ledger.Append(ctx, projectedWriterAcceptance("funded_ha_frame")); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for replica.ledgerAppendDisk.consumed() <= beforeReplica {
		select {
		case <-deadline.C:
			t.Fatal("actual mTLS replicated append bypassed native quota")
		case <-ticker.C:
		}
	}
	if primary.ledgerAppendDisk.consumed() <= beforePrimary {
		t.Fatal("actual Primary append bypassed native quota")
	}
	for _, r := range []*Runtime{primary, replica} {
		baseline := r.persistentInventory.Snapshot().ChargeBytes
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if got := r.resourcePressure.logicalBudget().Snapshot().Used.Disk; got < baseline+b.LedgerAppendDiskBytes {
			t.Fatal("HA Runtime Close refunded persisted baseline/native quota", got)
		}
		if err := r.ledgerAppendDisk.reserve(1); !errors.Is(err, resourcebudget.ErrClosed) {
			t.Fatal("HA Runtime Close left growth admission open", err)
		}
	}
}

func TestLedgerNativeAuxiliaryUsesSameManagerAndKeepsPublishedOldInodeFees(t *testing.T) {
	frames, key := nativeFrameFixture(t)
	owner, manager := ledgerAppendOwnerFixture(t, 8<<20)
	path := filepath.Join(t.TempDir(), "ledger.wal")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key, MaxBatch: 1, ReserveAppendDisk: owner.reserve, ReserveNativeFileDisk: owner.reserveNativeFile})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("frame-000")); err != nil {
		t.Fatal(err)
	}
	if owner.consumed() != int64(len(frames)) {
		t.Fatal("append fee differs from actual complete native frame")
	}
	if result, err := l.Roll(); err != nil || !result.Rolled {
		t.Fatal(result, err)
	}
	afterRoll := owner.consumed()
	if afterRoll != int64(len(frames))+4*(64<<10) {
		t.Fatal("Roll failed to fund both actual manifests, successor and sidecar", afterRoll)
	}
	manifest := filepath.Join(filepath.Dir(path), "segments.json")
	old, err := os.Open(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldBytes, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := l.Compact(1); err != nil || !result.Compressed {
		t.Fatal(result, err)
	}
	if owner.consumed() <= afterRoll+(64<<10) {
		t.Fatal("compression failed to reserve output and replacement manifest")
	}
	read := make([]byte, len(oldBytes))
	if _, err := old.ReadAt(read, 0); err != nil || !bytes.Equal(read, oldBytes) {
		t.Fatal("deleted-open old manifest lost actual bytes", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "ledger-1.wal")); !os.IsNotExist(err) {
		t.Fatal("committed compaction did not retire plain representation", err)
	}
	consumed := owner.consumed()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if owner.consumed() != consumed || manager.Snapshot().Used != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: 8 << 20}) {
		t.Fatal("actual Close refunded published/unlinked-open inode obligations", owner.consumed(), manager.Snapshot())
	}
}

func TestLedgerNativeRollManifestQuotaRefusalPrecedesNamespaceMutation(t *testing.T) {
	frames, key := nativeFrameFixture(t)
	owner, _ := ledgerAppendOwnerFixture(t, int64(len(frames))+(64<<10)-1)
	path := filepath.Join(t.TempDir(), "ledger.wal")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	status := ledger.NewStatus()
	l, err := ledger.OpenWithOptions(path, status, ledger.Options{ChainKey: key, MaxBatch: 1, ReserveAppendDisk: owner.reserve, ReserveNativeFileDisk: owner.reserveNativeFile})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(context.Background(), projectedWriterAcceptance("frame-000")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Roll(); !errors.Is(err, errLedgerAppendDiskLimit) {
		t.Fatal("unfunded Roll manifest accepted", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || status.Load() != ledger.AccountingHealthy || l.Generation() != 1 || owner.consumed() != int64(len(frames)) {
		t.Fatal("definite Roll admission failure changed native namespace/head/fees", err, status.Load(), l.Generation(), owner.consumed())
	}
	for _, name := range []string{"segments.json", "segments.json.partial", "ledger-1.wal"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); !os.IsNotExist(err) {
			t.Fatal("unfunded Roll created name", name, err)
		}
	}
}
