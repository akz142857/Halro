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
	"github.com/akz142857/Halro/internal/usage"
)

func TestBackupSharedReferenceKeepsPricingSourceArchiveAndRetirementChecks(t *testing.T) {
	for _, fault := range []string{"published", "unpublished", "closed-metadata-published", "closed-metadata-unpublished", "wrong-report", "orphan", "cancel", "identical-source-replacement", "source-edit", "foreign-root"} {
		t.Run(fault, func(t *testing.T) {
			cfg := budgetedBackupOperationFixture(t)
			cfg.ResourceBudget.MemoryStateRequired = true
			pressure, err := newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
			if err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ctx := context.WithValue(base, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
			native, metadata, key := openSealingLedger(t, cfg)
			defer metadata.Close()
			defer clear(key)
			if err := native.Close(); err != nil {
				t.Fatal(err)
			}
			report, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), key, nil)
			if err != nil || partial {
				t.Fatal(report, partial, err)
			}
			original, err := os.ReadFile(cfg.LedgerPath())
			if err != nil {
				t.Fatal(err)
			}
			root, parent := cfg.UsagePath(), t.TempDir()
			if fault == "unpublished" || fault == "closed-metadata-unpublished" || fault == "orphan" {
				root = t.TempDir()
			}
			if fault == "closed-metadata-published" || fault == "closed-metadata-unpublished" {
				if err := metadata.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "wrong-report" {
				report.Head.Sequence++
			}
			if fault == "orphan" {
				if err := os.WriteFile(filepath.Join(root, "orphan.ndjson"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			called, foreign := false, ""
			position, err := withBackupPricingAndUsageReference(ctx, cfg, metadata, root, cfg.LedgerPath(), key, report, parent, pressure, func(e *usage.Exporter, p usage.ArchivePosition) error {
				called = true
				if p.Files == 0 || p.LastSequence == 0 {
					t.Fatal("no real published archive", p)
				}
				// Both actual roots coexist in exactly one private job directory.
				paths, statErr := filepath.Glob(filepath.Join(parent, "halro-backup-reference-packed-*", "ledger"))
				if statErr != nil || len(paths) != 1 {
					t.Fatal(paths, statErr)
				}
				if err := e.WalkArchiveBackupFiles(ctx, report.Head, func(string) error { return nil }); err != nil {
					return err
				}
				switch fault {
				case "cancel":
					cancel()
				case "identical-source-replacement":
					if err := os.Rename(cfg.LedgerPath(), cfg.LedgerPath()+".original"); err != nil {
						return err
					}
					return os.WriteFile(cfg.LedgerPath(), original, 0600)
				case "source-edit":
					changed := bytes.Clone(original)
					changed[len(changed)-1] ^= 1
					return os.WriteFile(cfg.LedgerPath(), changed, 0600)
				case "foreign-root":
					foreign = filepath.Join(filepath.Dir(paths[0]), "usage")
					if err := os.Rename(foreign, foreign+".original"); err != nil {
						return err
					}
					return os.WriteFile(foreign, []byte("foreign root"), 0600)
				}
				return nil
			})
			if fault == "published" || fault == "unpublished" {
				if err != nil || called != (fault == "published") || (fault == "unpublished" && position != (usage.ArchivePosition{})) {
					t.Fatal(position, called, err)
				}
			} else if err == nil || position != (usage.ArchivePosition{}) {
				t.Fatal("failed combined job returned authority", position, err)
			}
			if fault == "closed-metadata-published" || fault == "closed-metadata-unpublished" || fault == "wrong-report" || fault == "orphan" {
				if called {
					t.Fatal("failed pricing/source/archive reached archive output")
				}
			}
			if fault == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if fault == "identical-source-replacement" || fault == "source-edit" {
				if !errors.Is(err, ledger.ErrProjectionSource) {
					t.Fatal("borrowed source change escaped final check", err)
				}
			}
			if fault == "foreign-root" {
				raw, readErr := os.ReadFile(foreign)
				if !errors.Is(err, ownedfile.ErrRetirement) || readErr != nil || string(raw) != "foreign root" || pressure.logicalBudget().Snapshot().Used == (resourcebudget.Cost{}) {
					t.Fatal("unknown retirement lost root/debt", string(raw), readErr, err)
				}
			} else {
				entries, readErr := os.ReadDir(parent)
				if readErr != nil || len(entries) != 0 || pressure.logicalBudget().Snapshot().Used != (resourcebudget.Cost{}) {
					t.Fatal("known retirement retained job/fees", entries, readErr, pressure.logicalBudget().Snapshot())
				}
			}
		})
	}
}
