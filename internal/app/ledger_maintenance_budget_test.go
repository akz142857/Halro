package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestOnlineLedgerMaintenanceUsesOriginalIOPoolAndJoinsCancellation(t *testing.T) {
	r, _, original := providerMaintenanceBudgetRuntime(t)
	r.maintenanceOwners = newRuntimeMaintenanceOwners(r.config, r.resourcePressure)
	substitute, err := resourcebudget.New(r.config.ResourceBudget.Limits(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := substitute.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	r.resourcePressure = &resourcePressureRuntime{budget: substitute}
	occupied, err := original.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: r.config.ResourceBudget.IORAMBytes - ledgerMaintenanceRAMBytes + 1})
	if err != nil {
		t.Fatal(err)
	}
	err = r.withLedgerMaintenance(context.Background(), func(context.Context) error { t.Fatal("refused job entered IO"); return nil })
	if !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal(err)
	}
	if err := occupied.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.withLedgerMaintenance(ctx, func(context.Context) error { close(entered); <-resume; return nil })
	}()
	<-entered
	cancel()
	if s := original.Snapshot(); s.Classes[resourcebudget.IO].RAM != ledgerMaintenanceRAMBytes || s.RecoveryClaims != 0 || substitute.Snapshot().Used != (resourcebudget.Cost{}) {
		close(resume)
		<-done
		t.Fatal("original job lost fee on cancellation", s)
	}
	close(resume)
	if err := <-done; !errors.Is(err, context.Canceled) || original.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal(err, original.Snapshot())
	}
	err = r.withLedgerMaintenance(context.Background(), func(context.Context) error { return ledger.ErrSegmentRetirement })
	if !errors.Is(err, ledger.ErrSegmentRetirement) || original.Snapshot().Classes[resourcebudget.IO].RAM != ledgerMaintenanceRAMBytes {
		t.Fatal("uncertain FD retirement refunded workspace", err, original.Snapshot())
	}
}

func TestSealCommandSharedWorkspaceRefusesBeforeSourceIO(t *testing.T) {
	cfg := commandBudgetFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager := pressure.logicalBudget()
	occupied, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: cfg.ResourceBudget.RecoveryRAMBytes - ledgerMaintenanceRAMBytes + 1})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Release()
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "source-must-not-be-created")
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	var output bytes.Buffer
	if err := WriteSealLedger(ctx, cfg, false, &output); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused command reached native IO", err)
	}
	if files, err := os.ReadDir(scratch); err != nil || len(files) != 0 || output.Len() != 0 {
		t.Fatal(files, err, output.Len())
	}
}

func TestSealReportKeepsOriginalFeesUntilActualOutputRetires(t *testing.T) {
	cfg := enableLedgerSegmentIndexBudget(sealOutputFixture(t, 2, false))
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(ctx)
	defer cancel()
	writer := &anchorBlockingWriter{make(chan struct{}), make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- WriteSealLedger(base, cfg, false, writer) }()
	<-writer.entered
	before := pressure.logicalBudget().Snapshot()
	if before.Classes[resourcebudget.Recovery].RAM != ledgerMaintenanceRAMBytes || before.Classes[resourcebudget.Recovery].Disk != legacySealReportDiskBytes {
		t.Fatal("physical report lost whole operation fee", before)
	}
	cancel()
	if after := pressure.logicalBudget().Snapshot(); after.Used != before.Used {
		t.Fatal("cancellation refunded blocked output", before, after)
	}
	close(writer.finish)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if after := pressure.logicalBudget().Snapshot(); after.Classes[resourcebudget.Recovery] != (resourcebudget.Cost{}) {
		t.Fatal("closed report retained transient fee", after)
	}
}

func TestSealReportFileCeilingAndUncertainUnlink(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "ceiling", true: "renamed original"}[replace], func(t *testing.T) {
			cfg := enableBackupReferenceBudget(sealOutputFixture(t, 2, false))
			cfg.ResourceBudget.BackupWorkspaceDiskBytes = 64
			if replace {
				cfg.ResourceBudget.BackupWorkspaceDiskBytes = 1 << 20
			}
			ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			base, cancel := context.WithCancel(ctx)
			defer cancel()
			var replaced string
			if replace {
				base = generationProbeContext{Context: base, probe: func() {
					if replaced != "" {
						return
					}
					matches, err := filepath.Glob(filepath.Join(scratch, "halro-seal-report_*.json"))
					if err != nil {
						t.Fatal(err)
					}
					if len(matches) == 0 {
						return
					}
					replaced = matches[0]
					if err := os.Rename(replaced, replaced+".original"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(replaced, []byte("foreign"), 0600); err != nil {
						t.Fatal(err)
					}
					cancel()
				}}
			}
			var output bytes.Buffer
			err = WriteSealLedger(base, cfg, false, &output)
			if err == nil || output.Len() != 0 {
				t.Fatal("failed preparation published output", output.Len(), err)
			}
			s := pressure.logicalBudget().Snapshot()
			if replace {
				if !errors.Is(err, ownedfile.ErrReplaced) || replaced == "" || s.Classes[resourcebudget.Recovery].RAM != ledgerMaintenanceRAMBytes || s.Classes[resourcebudget.Recovery].Disk != 1<<20 {
					t.Fatal("uncertain inode lost physical fee", s, err)
				}
				for _, name := range []string{replaced, replaced + ".original"} {
					if _, err := os.Stat(name); err != nil {
						t.Fatal("uncertain files lost", name, err)
					}
				}
			} else if s.Classes[resourcebudget.Recovery] != (resourcebudget.Cost{}) {
				t.Fatal("definite retirement kept report fee", s)
			}
		})
	}
}
