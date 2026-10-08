package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func commandBudgetFixture(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfig(t)
	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes, b.BackgroundDiskBytes = 512<<20, 256<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 128<<20, 128<<20
	b.LedgerSegmentIndexDiskBytes = 1 << 20
	b.UsageArchiveDiskBytes, b.IODiskBytes = 4<<20, 8<<20
	b.RecoveryClaims, b.MaxClaims = 8, 17
	b.ControlRAMBytes = 17 * resourcebudget.ClaimRAMBytes
	cfg.ResourcePressure, cfg.ResourceBudget = pressureConfigFixture(), &b
	return cfg
}

func TestCommandResourceScopeSharesIndexArchiveAndAuditActualOwners(t *testing.T) {
	cfg := commandBudgetFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager := pressure.logicalBudget()
	index, _, err := newOneShotLedgerSegmentIndexDiskOwner(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Release()
	archive, release, err := newOneShotUsageArchiveDiskBudget(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			_ = release()
		}
	}()
	if archive.manager != manager {
		t.Fatal("archive forked command manager")
	}
	log, err := openOneShotConfiguredAuditLog(ctx, cfg, filepath.Join(t.TempDir(), "audit.log"), make([]byte, 32), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if s := manager.Snapshot(); s.RecoveryClaims != 3 || s.Classes[resourcebudget.Recovery].Disk != cfg.ResourceBudget.LedgerSegmentIndexDiskBytes+cfg.ResourceBudget.UsageArchiveDiskBytes || s.Classes[resourcebudget.Recovery].RAM != auditlimits.MinCacheRAMBytes {
		t.Fatal("owners did not share total/class accounting", s)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if s := manager.Snapshot(); s.RecoveryClaims != 2 || s.Classes[resourcebudget.Recovery].RAM != 0 {
		t.Fatal("Audit cache fee survived actual Close", s)
	}
	releaseErr := release()
	release = nil
	if err := errors.Join(index.Release(), releaseErr); err != nil {
		t.Fatal(err)
	}
	if s := manager.Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("command owner leaked", s)
	}
}

func TestCommandResourceScopeRefusesSubstitutionOrCancellationBeforeAuditIO(t *testing.T) {
	cfg := commandBudgetFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if owner, _, err := newOneShotLedgerSegmentIndexDiskOwnerWithPressure(ctx, cfg, other); owner != nil || err == nil {
		t.Fatal("index accepted foreign manager", err)
	}
	if owner, _, err := newOneShotUsageArchiveDiskBudgetWithPressure(ctx, cfg, other); owner != nil || err == nil {
		t.Fatal("archive accepted foreign manager", err)
	}
	path := filepath.Join(t.TempDir(), "must-not-exist", "audit.log")
	changed := cfg
	budget := *cfg.ResourceBudget
	budget.AuditLogRAMBytes++
	changed.ResourceBudget = &budget
	for _, bad := range []config.Config{changed, {ResourceBudget: nil, ResourcePressure: cfg.ResourcePressure}} {
		if log, err := openOneShotConfiguredAuditLog(ctx, bad, path, make([]byte, 32), audit.Options{}); log != nil || err == nil {
			t.Fatal("scope configuration changed", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if log, err := openOneShotConfiguredAuditLog(canceled, cfg, path, make([]byte, 32), audit.Options{}); log != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled command entered IO", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal created directory", err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("refusal acquired ownership", s)
	}
	// Terminal KMS auditing must inherit the same manager even after user
	// cancellation; WithoutCancel changes cancellation, never resource authority.
	uncanceled := context.WithoutCancel(canceled)
	_, inherited, err := commandBudgetContext(uncanceled, cfg)
	if err != nil || inherited != pressure {
		t.Fatal("terminal audit dropped scope", err)
	}
}
