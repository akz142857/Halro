package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

var offlineDateScope = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

func TestVerifyUsageDateScopeBothCatalogsAndFormatsRemainReadOnly(t *testing.T) {
	for _, format := range []string{usage.FormatParquet, usage.FormatNDJSON} {
		for _, paged := range []bool{false, true} {
			t.Run(format+map[bool]string{false: "/legacy", true: "/paged"}[paged], func(t *testing.T) {
				r, _ := summaryRuntime(t, 2)
				r.stopBackgroundWork()
				cfg := r.config
				cfg.Usage.ExportFormat = format
				if paged {
					installDiskUsageQueries(t, r)
					var err error
					r.usageExporter, err = usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: format})
					if err != nil {
						t.Fatal(err)
					}
					r.exportUsageParquet()
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if !paged {
					if _, err := exportLegacyUsageFixture(cfg); err != nil {
						t.Fatal(err)
					}
				}
				before := doctorTree(t, cfg.Storage.DataDir)
				report, err := VerifyUsageRetainedDates(context.Background(), cfg, offlineDateScope)
				if err != nil || report.RetainFromDate != "9999-01-01" || report.LedgerRecords != 2 || report.ParquetRecords != 2 || report.RequiredRows != 0 || report.MatchedRows != 2 || report.ExpiredOmittedRows != 0 || report.AfterSequence != 0 || report.Role != "standalone" {
					t.Fatal(report, err)
				}
				if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
					t.Fatal("date comparison modified source/derivatives")
				}
				// Test an explicitly permitted all-expired omission without fabricating a
				// sequence floor. Keep the real archive frontier and native history.
				path := filepath.Join(cfg.UsagePath(), "manifest.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var root map[string]any
				if err := json.Unmarshal(raw, &root); err != nil {
					t.Fatal(err)
				}
				root["files"] = []any{}
				if paged {
					root["catalog"] = map[string]any{"version": 1, "head": map[string]any{}}
				}
				raw, err = json.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				before = doctorTree(t, cfg.Storage.DataDir)
				report, err = VerifyUsageRetainedDates(context.Background(), cfg, offlineDateScope)
				if err != nil || report.LedgerRecords != 2 || report.ParquetRecords != 0 || report.MatchedRows != 0 || report.ExpiredOmittedRows != 2 || report.ThroughSequence == 0 {
					t.Fatal(report, err)
				}
				if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
					t.Fatal("date omission comparison modified files")
				}
				required := time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC)
				if report, err := VerifyUsageRetainedDates(context.Background(), cfg, required); !errors.Is(err, usage.ErrDiskUsageCorrupt) || report != (usage.ReconciliationReport{}) {
					t.Fatal("missing retained rows escaped", report, err)
				}
			})
		}
	}
}

func TestVerifyUsageDateScopeHonorsReplicaAppliedAndPrimaryDurable(t *testing.T) {
	cfg, member, key, _, applied := offlineUsageMemberFixture(t)
	before := doctorTree(t, cfg.Storage.DataDir)
	report, err := VerifyUsageRetainedDates(context.Background(), cfg, offlineDateScope)
	if err != nil || report.Role != "replica" || report.OrderingIndex != 2 || report.AllowedLedgerHead != applied || report.NativeHead.Sequence <= applied.Sequence || report.SourceHead != applied || report.LedgerRecords != 2 || report.MatchedRows != 2 {
		t.Fatal(report, err)
	}
	if !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
		t.Fatal("date Replica verification modified source")
	}
	member.Role, member.ConfirmedIndex = replication.RolePrimary, 2
	if err := replication.WriteState(cfg.ReplicationStatePath(), member, key[:]); err != nil {
		t.Fatal(err)
	}
	report, err = VerifyUsageRetainedDates(context.Background(), cfg, offlineDateScope)
	if err != nil || report.Role != "primary" || report.OrderingIndex != 3 || report.AllowedLedgerHead != report.NativeHead || report.SourceHead != report.NativeHead || report.LedgerRecords != 2 {
		t.Fatal(report, err)
	}
}

func TestVerifyUsageDateScopeFinalEvidenceAndCancellationWithdrawReport(t *testing.T) {
	for _, kind := range []string{"member", "cancel", "native", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			cfg, member, key, _, _ := offlineUsageMemberFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			report, err := verifyUsageScope(ctx, cfg, 0, &offlineDateScope, func() error {
				switch kind {
				case "member":
					member.AppliedIndex, member.Projection.Index = 3, 3
					return replication.WriteState(cfg.ReplicationStatePath(), member, key[:])
				case "cancel":
					cancel()
				case "native":
					file, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, err = file.Write([]byte("late torn native"))
					return errors.Join(err, file.Close())
				case "metadata":
					raw, err := os.ReadFile(cfg.MetadataPath())
					if err != nil {
						return err
					}
					if err := os.WriteFile(cfg.MetadataPath()+".next", raw, 0o600); err != nil {
						return err
					}
					return os.Rename(cfg.MetadataPath()+".next", cfg.MetadataPath())
				}
				return nil
			})
			if err == nil || report != (usage.ReconciliationReport{}) {
				t.Fatal(report, err)
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
	cfg := testConfig(t)
	if report, err := VerifyUsageRetainedDates(context.Background(), cfg, time.Time{}); !errors.Is(err, usage.ErrDiskUsageLimit) || report != (usage.ReconciliationReport{}) {
		t.Fatal(report, err)
	}
	if _, err := os.Stat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid policy created native directory", err)
	}
}
