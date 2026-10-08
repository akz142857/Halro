package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageDateReferencesFreshOwnerAndFinalAuthority(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		for _, fault := range []string{"success", "native", "metadata", "cancel"} {
			t.Run(format+"/"+fault, func(t *testing.T) {
				cfg := offlineDateActivationFixture(t, format)
				cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
				activated, err := ActivateUsageDateRetention(context.Background(), cfg, cutoff)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var indexed usage.DateCatalogReferenceProgress
				scope, err := withUsageDiskReference(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
					limits := usage.DefaultDateCatalogReferenceLimits()
					limits.Pages = 1
					for i := 0; i < 128; i++ {
						var err error
						indexed, err = e.PrepareDateCatalogReferencesStep(ctx, cutoff, limits)
						if err != nil {
							return err
						}
						if indexed.Phase == "ready" {
							return nil
						}
					}
					return errors.New("reference fixture did not finish")
				}, func() error {
					switch fault {
					case "native":
						f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
						if err != nil {
							return err
						}
						_, err = f.Write([]byte("late torn native"))
						return errors.Join(err, f.Close())
					case "metadata":
						raw, err := os.ReadFile(cfg.MetadataPath())
						if err != nil {
							return err
						}
						if err := os.WriteFile(cfg.MetadataPath()+".replacement", raw, 0o600); err != nil {
							return err
						}
						return os.Rename(cfg.MetadataPath()+".replacement", cfg.MetadataPath())
					case "cancel":
						cancel()
					}
					return nil
				})
				if indexed.Phase != "ready" || indexed.Root != activated.Activation.Current || indexed.FilesIndexed != indexed.Root.Files {
					t.Fatal(indexed)
				}
				if fault == "success" {
					if err != nil || scope.SourceHead != activated.Verification.SourceHead {
						t.Fatal(scope, err)
					}
				} else {
					if err == nil || scope != (usage.ReconciliationReport{}) {
						t.Fatal("index supplied source authority", scope, err)
					}
					if fault == "cancel" && !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				}
				paths, globErr := filepath.Glob(filepath.Join(cfg.UsagePath(), ".archive-date-ref-index-*"))
				if globErr != nil || len(paths) != 0 {
					t.Fatal("reference scratch survived physical owner cleanup", paths, globErr)
				}
				guard, lockErr := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
				if lockErr != nil {
					t.Fatal("directory lock survived cleanup", lockErr)
				}
				if err := guard.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
