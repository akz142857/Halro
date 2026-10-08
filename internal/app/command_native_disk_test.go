package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func commandNativeDiskFixture(t *testing.T) config.Config {
	t.Helper()
	cfg := budgetedBackupOperationFixture(t)
	cfg.ResourceBudget.PersistentInventoryDiskBytes = 16 << 20
	cfg.ResourceBudget.LedgerAppendDiskBytes = 8 << 20
	return cfg
}

func TestCommandNativeDiskKeepsOriginalManagerTreeAndPersistentFees(t *testing.T) {
	cfg := commandNativeDiskFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	owner, err := prepareCommandNativeDisk(ctx, cfg, guard)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := prepareCommandNativeDisk(ctx, cfg, guard); err != nil || same != owner {
		t.Fatal("nested native source replaced original fixed owner", err)
	}
	options := commandNativeOptions(ctx, ledger.Options{})
	if options.ReserveAppendDisk == nil || options.ReserveNativeFileDisk == nil {
		t.Fatal("actual one-shot options lost original disk callbacks")
	}
	if err := options.ReserveAppendDisk(77); err != nil || owner.growth.consumed() != 77 {
		t.Fatal("one-shot callback detached from scope owner", err)
	}
	before := pressure.logicalBudget().Snapshot()
	if before.Classes[resourcebudget.Background].Disk != owner.baseline.Snapshot().ChargeBytes+cfg.ResourceBudget.LedgerAppendDiskBytes {
		t.Fatal("native inventory/growth substituted another Manager", before)
	}
	if err := owner.finish(nil); err != nil || pressure.logicalBudget().Snapshot().Used != before.Used {
		t.Fatal("command exit refunded original native stock/growth", err)
	}
	if _, err := prepareCommandNativeDisk(ctx, cfg, guard); !errors.Is(err, resourcebudget.ErrClosed) {
		t.Fatal("completed command reused a stale inventory selection", err)
	}
	if opts := commandNativeOptions(ctx, ledger.Options{}); !errors.Is(opts.ReserveAppendDisk(1), resourcebudget.ErrClosed) {
		t.Fatal("completed offline owner silently reverted to unlimited options")
	}
}

func TestCommandNativeUnknownIndexExitKeepsActualReaderAndPermit(t *testing.T) {
	cfg := commandNativeDiskFixture(t)
	cfg.ResourceBudget.LedgerSegmentIndexDiskBytes = 64 << 10
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	index, _, err := newOneShotLedgerSegmentIndexDiskOwner(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "actual-reader"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before := pressure.logicalBudget().Snapshot().Used
	err = retireCommandLedgerIndex(index, file, ownedfile.CloseFile(file))
	var debt *commandLedgerDiskFailure
	if !errors.Is(err, ownedfile.ErrRetirement) || !errors.As(err, &debt) || debt.index != index || debt.native != file || pressure.logicalBudget().Snapshot().Used != before {
		t.Fatal("uncertain actual descriptor released index fee or owner", err)
	}
}

func TestCommandNativeSealFundsRealRollAndRejectsBeforeMutation(t *testing.T) {
	for _, outcome := range []string{"funded", "refused"} {
		t.Run(outcome, func(t *testing.T) {
			cfg := commandNativeDiskFixture(t)
			if outcome == "refused" {
				cfg.ResourceBudget.LedgerAppendDiskBytes = (64 << 10) - 1
			}
			ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(cfg.LedgerPath())
			if err != nil {
				t.Fatal(err)
			}
			report, err := SealLedger(ctx, cfg, false)
			owner := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope).nativeDisk
			if owner == nil || owner.baseline == nil || owner.growth == nil || !owner.closed {
				t.Fatal("actual Seal did not join its original native owner", err)
			}
			if outcome == "funded" {
				if err != nil || !report.Rolled || owner.growth.consumed() != 4*(64<<10) {
					t.Fatal("actual Seal omitted manifest/successor/sidecar producer", report, err, owner.growth.consumed())
				}
			} else {
				var debt *commandNativeDiskFailure
				if !errors.Is(err, errLedgerAppendDiskLimit) || !errors.As(err, &debt) || debt.owner != owner || owner.growth.consumed() != 0 {
					t.Fatal("unfunded offline Roll accepted/lost original owner", err)
				}
				if actual, err := os.ReadFile(cfg.LedgerPath()); err != nil || !bytes.Equal(actual, before) {
					t.Fatal("offline quota refusal changed source bytes", err)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.LedgerPath()), "segments.json")); !os.IsNotExist(err) {
					t.Fatal("offline quota refusal created authority", err)
				}
			}
			if used := pressure.logicalBudget().Snapshot().Used.Disk; used < owner.baseline.Snapshot().ChargeBytes+cfg.ResourceBudget.LedgerAppendDiskBytes {
				t.Fatal("offline source exit refunded published obligation", used)
			}
		})
	}
}

func TestCommandNativeBackupInventoryRejectsBeforeActualTailRepair(t *testing.T) {
	cfg := commandNativeDiskFixture(t)
	cfg.ResourceBudget.PersistentInventoryDiskBytes = 1
	file, err := os.OpenFile(cfg.AuditPath(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{0, 1}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "refused.hmbk")
	_, err = CreateBackup(ctx, cfg, "unused-config", archive, bytes.Repeat([]byte{0x69}, 32))
	var debt *commandNativeDiskFailure
	if err == nil || !errors.As(err, &debt) || debt.owner == nil || debt.owner.growth == nil || !debt.owner.growth.closed {
		t.Fatal("failed one-shot inventory lost its native growth owner", err)
	}
	if actual, err := os.ReadFile(cfg.AuditPath()); err != nil || !bytes.Equal(actual, before) {
		t.Fatal("backup inventory ran after destructive tail repair", err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatal("failed inventory published archive", err)
	}
}

func TestCommandNativeBackupPendingRollUsesSourceOwnerAndSeparateStaging(t *testing.T) {
	cfg := commandNativeDiskFixture(t)
	log, store, key := openSealingLedger(t, cfg)
	rolled, err := log.Roll()
	if err != nil || !rolled.Rolled {
		t.Fatal("real pending Roll fixture", rolled, err)
	}
	if err := errors.Join(log.Close(), store.Close()); err != nil {
		t.Fatal(err)
	}
	clear(key)
	pending, err := json.Marshal(struct {
		Version  int              `json:"version"`
		Segments []ledger.Segment `json:"segments"`
		Pending  *ledger.Segment  `json:"pending"`
	}{Version: 1, Pending: &rolled.Sealed})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg.LedgerPath()), "segments.json"), pending, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.LedgerPath()); err != nil {
		t.Fatal(err)
	}
	ctx, _, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = CreateBackup(ctx, cfg, configPath, filepath.Join(t.TempDir(), "recovered.hmbk"), bytes.Repeat([]byte{0x69}, 32))
	if err != nil {
		t.Fatal("actual pending source backup", err)
	}
	scope := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if scope.nativeDisk == nil || scope.nativeDisk.growth.consumed() != 2*(64<<10) || !scope.nativeDisk.closed {
		t.Fatal("actual pending source repaired before native admission", scope.nativeDisk)
	}
	if scope.backupStaging == nil || scope.backupStaging.quota == scope.nativeDisk.growth || scope.backupStaging.quota.consumed() < 7*(64<<10) {
		t.Fatal("private backup consumed source stock/growth allowance")
	}
}
