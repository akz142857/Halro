package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

func usageReferenceBudgetFixture(t *testing.T) config.Config {
	t.Helper()
	cfg := offlineDateActivationFixture(t, usage.FormatNDJSON)
	resource := commandBudgetFixture(t)
	b := *resource.ResourceBudget
	b.TotalRAMBytes += 1 << 20
	b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes = 8<<20, 8<<20
	addProjectionMappingFixtureRAM(t, &b)
	b.RecoveryClaims, b.MaxClaims = 10, 21
	b.ControlRAMBytes = 21 * resourcebudget.ClaimRAMBytes
	cfg.ResourceBudget, cfg.ResourcePressure = &b, resource.ResourcePressure
	return cfg
}

func TestUsageReferenceSharedLifetimeAndOwnedRemoval(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "actual-cleanup", true: "replacement-preserved"}[replace], func(t *testing.T) {
			cfg := usageReferenceBudgetFixture(t)
			ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			t.Setenv("TMPDIR", parent)
			var replaced string
			out, err := withUsageDiskReference(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, _ *usage.Exporter, _ usage.ReconciliationReport) error {
				s := pressure.logicalBudget().Snapshot()
				if s.Classes[resourcebudget.Recovery].RAM < 208<<20 || s.Classes[resourcebudget.Recovery].Disk < 20<<20 {
					t.Fatal("private references escaped shared fees", s)
				}
				if _, err := p.ReadStats(ctx); err != nil {
					return err
				}
				if replace {
					paths, err := filepath.Glob(filepath.Join(parent, "halro-usage-verify-*", "usage.db"))
					if err != nil || len(paths) != 1 {
						t.Fatal(paths, err)
					}
					replaced = paths[0]
					if err := os.Rename(replaced, replaced+".original"); err != nil {
						return err
					}
					return os.WriteFile(replaced, []byte("foreign replacement"), 0600)
				}
				return nil
			}, nil)
			s := pressure.logicalBudget().Snapshot()
			if replace {
				if err == nil || out != (usage.ReconciliationReport{}) || s.Classes[resourcebudget.Recovery].RAM != 208<<20 || s.Classes[resourcebudget.Recovery].Disk != 16<<20 {
					t.Fatal("failed cleanup refunded its job", out, err, s)
				}
				raw, readErr := os.ReadFile(replaced)
				if readErr != nil || string(raw) != "foreign replacement" {
					t.Fatal("replacement was removed", readErr)
				}
			} else {
				if err != nil || s.Used != (resourcebudget.Cost{}) {
					t.Fatal(err, s)
				}
				entries, err := os.ReadDir(parent)
				if err != nil || len(entries) != 0 {
					t.Fatal("owned scratch survived actual cleanup", entries, err)
				}
			}
		})
	}
}

func TestUsageReferenceBudgetRefusesBeforeScratchAndNeverUsesRecoveryForOnline(t *testing.T) {
	cfg := usageReferenceBudgetFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A monitor-bound manager begins unknown. Online maintenance cannot borrow
	// Recovery simply because ordinary IO pressure or capacity refuses it.
	if _, err := acquireUsageReferenceBudget(ctx, cfg, pressure, resourcebudget.IO); err == nil {
		t.Fatal("unknown pressure admitted online references")
	}
	occupied, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: 100 << 20})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	_, err = withUsageDiskReference(ctx, cfg, func(context.Context, *usage.DiskProjection, *usage.Exporter, usage.ReconciliationReport) error {
		t.Fatal("refused job invoked work")
		return nil
	}, nil)
	if err == nil {
		t.Fatal("exhausted workspace was admitted")
	}
	if err := occupied.Release(); err != nil {
		t.Fatal(err)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil || len(entries) != 0 {
		t.Fatal(entries, readErr)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("refusal leaked", s)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := acquireUsageReferenceBudget(canceled, cfg, pressure, resourcebudget.Recovery); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
