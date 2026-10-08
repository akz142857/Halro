package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestRuntimeMetadataDataFileCeilingOwnsPersistentFileAndReconciles(t *testing.T) {
	seed, _, _ := openBootstrappedRuntime(t)
	cfg := seed.config
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.MetadataPath())
	if err != nil || info.Size() > 4<<20 {
		t.Fatal("fixture must remain a small native Metadata DB", info, err)
	}
	pressure, b := runtimePressureFixture(), runtimeRAMBudgetFixture()
	b.MetadataStoreDiskBytes = info.Size()
	b.TotalDiskBytes, b.BackgroundDiskBytes = 64<<20, 16<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 16<<20, 16<<20
	b.RecoveryClaims++
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	addMetadataMappingFixtureRAM(t, &b)
	// Reopening the existing archive also admits its complete comparison workspace.
	b.TotalRAMBytes += 512 << 20
	b.RecoveryRAMBytes += 512 << 20
	b.RecoveryReserveRAMBytes += 512 << 20
	b.TotalDiskBytes += 2 << 30
	b.RecoveryDiskBytes += 1 << 30
	b.RecoveryReserveDiskBytes += 1 << 30
	cfg.ResourceBudget, cfg.ResourcePressure = &b, &pressure
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.stopBackgroundWork()
	manager := r.resourcePressure.logicalBudget()
	if r.metadataStoreDiskPermit == nil || manager.Snapshot().Classes[resourcebudget.Background].Disk != info.Size() || manager.Snapshot().Classes[resourcebudget.Recovery].Disk != 0 {
		t.Fatal("Metadata data-file owner did not use the shared published budget", manager.Snapshot())
	}
	ram, _ := metadatalimits.MappingRAMBytes(b.MetadataStoreDiskBytes)
	if s := manager.Snapshot(); s.Classes[resourcebudget.Background].RAM != ram {
		t.Fatal("live Metadata mapping did not retain its complete shared fee", s)
	}
	project := domain.Project{ID: "project-native-quota", Name: "native quota", Enabled: true, AllowedModels: []string{strings.Repeat("m", int(info.Size())+64<<10)}}
	_, err = r.store.PutProject(ctx, project, 0, nil)
	if !errors.Is(err, boltstore.ErrMetadataStoreDiskLimit) || !errors.Is(err, boltstore.ErrMetadataRecoveryRequired) {
		t.Fatal("Runtime Metadata write escaped the file ceiling", err)
	}
	ready := httptest.NewRecorder()
	r.gatewayHandler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable || !strings.Contains(ready.Body.String(), "recovery_required") {
		t.Fatal("failed quota still published ready", ready.Code, ready.Body.String())
	}
	if err := r.Close(); !errors.Is(err, boltstore.ErrMetadataRecoveryRequired) {
		t.Fatal("Close hid the missing final checkpoint", err)
	}
	if snapshot := manager.Snapshot(); snapshot.Used != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: info.Size()}) || snapshot.Classes[resourcebudget.Background].Disk != info.Size() || snapshot.RecoveryClaims != 0 || snapshot.NormalClaims != 1 {
		t.Fatal("Close refunded a native metadata file still on disk", snapshot)
	}
	b.MetadataStoreDiskBytes += 8 << 20
	b.BackgroundRAMBytes += 8 << 20
	b.TotalRAMBytes += 16 << 20
	b.RecoveryRAMBytes += 8 << 20
	b.RecoveryReserveRAMBytes += 8 << 20
	next, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	got, err := next.store.GetProject(ctx, project.ID)
	if err != nil || len(got.AllowedModels) != 1 || got.AllowedModels[0] != project.AllowedModels[0] {
		t.Fatal("reopened Runtime lost durable native work", err)
	}
	if snapshot := next.resourcePressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Background].Disk != b.MetadataStoreDiskBytes {
		t.Fatal("new Runtime did not charge the raised ceiling", snapshot)
	}
}

func addMetadataMappingFixtureRAM(t *testing.T, b *config.ResourceBudget) {
	t.Helper()
	ram, err := metadatalimits.MappingRAMBytes(b.MetadataStoreDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	b.TotalRAMBytes += 2 * ram
	b.BackgroundRAMBytes += ram
	b.RecoveryRAMBytes += ram
	b.RecoveryReserveRAMBytes += ram
}

func TestMetadataMappingRetirementRequiresActualCloseAndKeepsDisk(t *testing.T) {
	cfg := commandBudgetFixture(t)
	cfg.ResourceBudget.MetadataStoreDiskBytes = 1 << 20
	cfg.ResourceBudget.RecoveryClaims++
	cfg.ResourceBudget.MaxClaims += 2
	cfg.ResourceBudget.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	cfg.ResourceBudget.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	cfg.ResourceBudget.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	addMetadataMappingFixtureRAM(t, cfg.ResourceBudget)
	_, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := acquireMetadataStoreDisk(context.Background(), cfg, pressure)
	if err != nil {
		t.Fatal(err)
	}
	manager := pressure.logicalBudget()
	initial := manager.Snapshot().Used
	failure := errors.New("physical close did not finish")
	if err := owner.retireRAMAfterClose(failure); !errors.Is(err, failure) || manager.Snapshot().Used != initial {
		t.Fatal("failed Close refunded physical ownership", err, manager.Snapshot())
	}
	if err := owner.retireRAMAfterClose(nil); err != nil {
		t.Fatal(err)
	}
	if s := manager.Snapshot(); s.Used != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: 1 << 20}) {
		t.Fatal("RAM retirement refunded a persistent file", s)
	}
	if err := owner.retireRAMAfterClose(nil); err != nil {
		t.Fatal("repeated retirement was not idempotent", err)
	}
}

func TestMetadataDataFileBudgetRefusesWithoutSharedManagerOrOnCancellation(t *testing.T) {
	cfg := testConfig(t)
	b := runtimeRAMBudgetFixture()
	b.MetadataStoreDiskBytes = 8 << 20
	cfg.ResourceBudget = &b
	if permit, err := acquireMetadataStoreDisk(context.Background(), cfg, nil); err == nil || permit != nil {
		t.Fatal("configured file ceiling was admitted without its shared owner", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if permit, err := acquireMetadataStoreDisk(ctx, cfg, nil); !errors.Is(err, context.Canceled) || permit != nil {
		t.Fatal("cancelled file owner performed work", err)
	}
}

func TestRuntimeKeySlotsMetadataMappingBorrowsExactRootOwner(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{factory: harness.factory}); err != nil {
		t.Fatal(err)
	}
	b := runtimeRAMBudgetFixture()
	b.MetadataStoreDiskBytes = 1 << 20
	b.TotalDiskBytes, b.BackgroundDiskBytes = 64<<20, 16<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 16<<20, 16<<20
	b.RecoveryClaims++
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	addMetadataMappingFixtureRAM(t, &b)
	policy := runtimePressureFixture()
	cfg.ResourceBudget, cfg.ResourcePressure = &b, &policy
	pressure, err := newResourcePressure(cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := acquireMetadataStoreDisk(context.Background(), cfg, pressure)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	ctx := context.WithValue(context.Background(), runtimeMetadataBootstrapKey{}, runtimeMetadataBootstrap{pressure: pressure, owner: owner, budget: b, policy: policy, path: cfg.MetadataPath()})
	before := pressure.logicalBudget().Snapshot()
	key, err := unlockMemberMasterKeyWithFactory(ctx, cfg, harness.factory)
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
	if s := pressure.logicalBudget().Snapshot(); s.Used != before.Used || s.Owners != before.Owners || s.RecoveryClaims != before.RecoveryClaims {
		t.Fatal("sequential Key Slots inspection changed Runtime's physical reservation", s, before)
	}
	changed := cfg
	changedBudget := b
	changedBudget.MetadataStoreDiskBytes++
	changed.ResourceBudget = &changedBudget
	if key, err := unlockMemberMasterKeyWithFactory(ctx, changed, harness.factory); key != nil || err == nil {
		clear(key)
		t.Fatal("bootstrap changed its authority-bound resource scope", err)
	}
}
