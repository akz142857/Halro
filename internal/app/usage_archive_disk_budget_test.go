package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestUsageArchiveWorkRAMRemainsSharedWhenFileCeilingsAreDisabled(t *testing.T) {
	cfg := commandBudgetFixture(t)
	cfg.ResourceBudget.UsageArchiveDiskBytes, cfg.ResourceBudget.UsageArchiveReadRAMBytes = 0, 0
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	archive, closeArchive, err := newOneShotUsageArchiveDiskBudgetWithPressure(ctx, cfg, pressure)
	if err != nil || archive == nil {
		t.Fatal("enabled budget lost archive workspace owner", err)
	}
	defer closeArchive()
	release, err := archive.workReservation()(ctx, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM != 32<<20 {
		t.Fatal("disabled file ceiling bypassed shared RAM", s)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.workReservation()(ctx, 288<<20); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("uncalibrated comparison workspace was admitted", err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("refused workspace retained fees", s)
	}
}

func TestOneShotUsageArchiveBudgetOwnsPersistentAndTransientClaims(t *testing.T) {
	dataDir := t.TempDir()
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes, b.BackgroundDiskBytes = 256<<20, 256<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 128<<20, 128<<20
	b.IODiskBytes = 64
	b.UsageArchiveDiskBytes = 64
	b.UsageArchiveReadRAMBytes = 8
	b.RecoveryClaims, b.MaxClaims = 11, 23
	b.ControlRAMBytes = int64(b.MaxClaims) * resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 12 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += 6 * resourcebudget.ClaimRAMBytes
	cfg := config.Config{Storage: config.Storage{DataDir: dataDir}, ResourcePressure: pressureConfigFixture(), ResourceBudget: &b}

	archive, releaseArchive, err := newOneShotUsageArchiveDiskBudget(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager := archive.manager
	if snapshot := manager.Snapshot(); snapshot.RecoveryClaims != 1 || snapshot.Classes[resourcebudget.Recovery].Disk != b.UsageArchiveDiskBytes {
		t.Fatal("one-shot archive was not charged before inventory", snapshot)
	}
	releaseReaderRAM, err := archive.reserveRAM(context.Background(), b.UsageArchiveReadRAMBytes)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.Classes[resourcebudget.Recovery].RAM != b.UsageArchiveReadRAMBytes {
		t.Fatal("one-shot archive reader RAM did not use Recovery", snapshot)
	}
	if err := releaseReaderRAM(); err != nil {
		t.Fatal(err)
	}
	releaseTransient, err := archive.reserve(context.Background(), 16)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.RecoveryClaims != 2 || snapshot.Classes[resourcebudget.Recovery].Disk != b.UsageArchiveDiskBytes+16 {
		t.Fatal("one-shot transient work did not share the Recovery Manager", snapshot)
	}
	if err := releaseTransient(); err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.RecoveryClaims != 1 || snapshot.Classes[resourcebudget.Recovery].Disk != b.UsageArchiveDiskBytes {
		t.Fatal("transient release disturbed the persistent archive owner", snapshot)
	}
	if err := releaseArchive(); err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.Used.Disk != 0 || snapshot.Owners != 0 {
		t.Fatal("one-shot archive claim survived command cleanup", snapshot)
	}
}

func TestRuntimeUsageArchiveBudgetMovesTransientClaimsFromRecoveryToIO(t *testing.T) {
	dataDir := t.TempDir()
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes, b.BackgroundDiskBytes = 256<<20, 256<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 128<<20, 128<<20
	b.IODiskBytes = 64
	b.UsageArchiveDiskBytes = 64
	b.UsageArchiveReadRAMBytes = 8
	b.RecoveryClaims, b.MaxClaims = 11, 23
	b.ControlRAMBytes = int64(b.MaxClaims) * resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 12 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += 6 * resourcebudget.ClaimRAMBytes
	cfg := config.Config{Storage: config.Storage{DataDir: dataDir}, ResourcePressure: pressureConfigFixture(), ResourceBudget: &b}
	manager, err := resourcebudget.New(b.Limits(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	pressure := &resourcePressureRuntime{budget: manager}
	persistent, err := acquireUsageArchiveDisk(context.Background(), cfg, pressure)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistent.Reclassify(resourcebudget.Background); err != nil {
		t.Fatal(err)
	}
	defer persistent.Release()

	archive := newRuntimeUsageArchiveDiskBudget(cfg, pressure)
	releaseStartup, err := archive.reserve(context.Background(), 16)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Recovery].Disk != 16 {
		t.Fatal("startup archive work did not use Recovery", snapshot)
	}
	releaseStartupRAM, err := archive.reserveRAM(context.Background(), b.UsageArchiveReadRAMBytes)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Recovery].RAM != b.UsageArchiveReadRAMBytes {
		t.Fatal("startup archive reader RAM did not use Recovery", snapshot)
	}
	if err := releaseStartupRAM(); err != nil {
		t.Fatal(err)
	}
	if err := releaseStartup(); err != nil {
		t.Fatal(err)
	}

	archive.activateIO()
	releaseIO, err := archive.reserve(context.Background(), 16)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.IO].Disk != 16 {
		t.Fatal("runtime archive work did not use IO", snapshot)
	}
	releaseIORAM, err := archive.reserveRAM(context.Background(), b.UsageArchiveReadRAMBytes)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.IO].RAM != b.UsageArchiveReadRAMBytes {
		t.Fatal("runtime archive reader RAM did not use IO", snapshot)
	}
	if err := releaseIORAM(); err != nil {
		t.Fatal(err)
	}
	if err := releaseIO(); err != nil {
		t.Fatal(err)
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Used.Disk != b.UsageArchiveDiskBytes {
		t.Fatal("transient archive charge survived operation cleanup", snapshot)
	}
}

func pressureConfigFixture() *config.ResourcePressure {
	return &config.ResourcePressure{RuntimeEnterBytes: 100, RuntimeExitBytes: 80, CgroupEnterBytes: 200, CgroupExitBytes: 160, HeadroomEnterBytes: 20, HeadroomExitBytes: 40, DiskEnterBytes: 100, DiskExitBytes: 200, InodesEnter: 10, InodesExit: 20, MaxAge: config.Duration(time.Second), Cooldown: config.Duration(time.Nanosecond), SampleInterval: config.Duration(time.Millisecond)}
}
