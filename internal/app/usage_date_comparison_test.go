package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

func compareOfflineDateCandidate(t *testing.T, ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, cutoff time.Time) usage.DateCatalogVerifyProgress {
	t.Helper()
	if _, err := e.MigrateArchiveCatalog(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		out, err := e.PrepareRetainedDateCatalogStep(ctx, cutoff, usage.DefaultDateCatalogBuildLimits())
		if err != nil {
			t.Fatal(err)
		}
		if out.Phase == "ready" {
			break
		}
		if i == 99 {
			t.Fatal("build stalled")
		}
	}
	limits := usage.DefaultDateCatalogVerifyLimits()
	limits.Files = 1
	limits.Source.Rows = 1
	limits.Source.PageRows = 1
	for i := 0; i < 100; i++ {
		out, err := e.VerifyRetainedDateCatalogStep(ctx, p, cutoff, limits)
		if err != nil {
			t.Fatal(err)
		}
		if out.Phase == "ready" {
			return out
		}
	}
	t.Fatal("comparison stalled")
	return usage.DateCatalogVerifyProgress{}
}

func TestUsageDateComparisonFreshReferenceAndRoleCuts(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			r, _ := summaryRuntime(t, 2)
			r.stopBackgroundWork()
			cfg := r.config
			cfg.Usage.ExportFormat = format
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := exportLegacyUsageFixture(cfg); err != nil {
				t.Fatal(err)
			}
			cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			var compared usage.DateCatalogVerifyProgress
			scope, err := withUsageDiskReference(context.Background(), cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
				compared = compareOfflineDateCandidate(t, ctx, p, e, cutoff)
				return nil
			}, nil)
			if err != nil || compared.Report.SourceRows != 2 || compared.Report.RequiredRows != 2 || compared.Report.MatchedRows != 2 || compared.Report.SourceHead != scope.AllowedLedgerHead || scope.Role != "standalone" {
				t.Fatal(scope, compared, err)
			}
			// Another operation owns new native-derived Ledger/Usage generations;
			// a persisted Ready must be rebuilt rather than inherited as trust.
			previous := compared.Report
			again, err := withUsageDiskReference(context.Background(), cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
				compared = compareOfflineDateCandidate(t, ctx, p, e, cutoff)
				return nil
			}, nil)
			if err != nil || compared.Report != previous || again.SourceHead != scope.SourceHead {
				t.Fatal(again, compared, err)
			}
			entries, err := filepath.Glob(filepath.Join(cfg.UsagePath(), ".archive-date-index-*"))
			if err != nil || len(entries) != 0 {
				t.Fatal("comparison scratch survived owner cleanup", entries, err)
			}
			root, err := os.ReadFile(filepath.Join(cfg.UsagePath(), "manifest.json"))
			if err != nil || !bytes.Contains(root, []byte(`"schema_version":9`)) {
				t.Fatal("comparison activated date root", err)
			}
		})
	}
	for _, role := range []replication.Role{replication.RoleReplica, replication.RolePrimary} {
		t.Run(string(role), func(t *testing.T) {
			cfg, member, key, _, applied := offlineUsageMemberFixture(t)
			if role == replication.RolePrimary {
				member.Role = role
				member.ConfirmedIndex = 2
				if err := replication.WriteState(cfg.ReplicationStatePath(), member, key[:]); err != nil {
					t.Fatal(err)
				}
			}
			var compared usage.DateCatalogVerifyProgress
			scope, err := withUsageDiskReference(context.Background(), cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
				compared = compareOfflineDateCandidate(t, ctx, p, e, offlineDateScope)
				return nil
			}, nil)
			if err != nil || compared.Report.SourceRows != 2 || compared.Report.ArchiveRows != 0 || compared.Report.ExpiredOmittedRows != 2 || compared.Report.SourceHead != scope.AllowedLedgerHead {
				t.Fatal(scope, compared, err)
			}
			if role == replication.RoleReplica && (scope.SourceHead != applied || scope.OrderingIndex != 2 || scope.NativeHead.Sequence <= applied.Sequence) {
				t.Fatal(scope)
			}
			if role == replication.RolePrimary && (scope.SourceHead != scope.NativeHead || scope.OrderingIndex != 3) {
				t.Fatal(scope)
			}
		})
	}
}

func TestUsageDateComparisonFinalAuthorityFailureWithdrawsScope(t *testing.T) {
	for _, fault := range []string{"member", "native", "metadata", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			cfg, member, key, _, _ := offlineUsageMemberFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := false
			scope, err := withUsageDiskReference(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
				out := compareOfflineDateCandidate(t, ctx, p, e, offlineDateScope)
				ready = out.Phase == "ready"
				return nil
			}, func() error {
				switch fault {
				case "member":
					member.AppliedIndex = 3
					member.Projection.Index = 3
					return replication.WriteState(cfg.ReplicationStatePath(), member, key[:])
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
			if !ready || err == nil || scope != (usage.ReconciliationReport{}) {
				t.Fatal("candidate comparison replaced final independent authority", ready, scope, err)
			}
			if fault == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			guard, lockErr := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
			if lockErr != nil {
				t.Fatal("directory owner survived cleanup", lockErr)
			}
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
			entries, globErr := filepath.Glob(filepath.Join(cfg.UsagePath(), ".archive-date-index-*"))
			if globErr != nil || len(entries) != 0 {
				t.Fatal(entries, globErr)
			}
		})
	}
}
