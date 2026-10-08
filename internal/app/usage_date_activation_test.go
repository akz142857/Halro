package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

func offlineDateActivationFixture(t *testing.T, format string) config.Config {
	t.Helper()
	r, _ := summaryRuntime(t, 2)
	r.stopBackgroundWork()
	cfg := r.config
	cfg.Usage.ExportFormat = format
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Runtime may already have exported using its original default format. The
	// isolated derived fixture must be rebuilt after selecting the test format;
	// an existing immutable partition is never rewritten by ordinary Export.
	if err := os.RemoveAll(cfg.UsagePath()); err != nil {
		t.Fatal(err)
	}
	if _, err := exportLegacyUsageFixture(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestUsageDateActivationFreshNativePublicationAndRetry(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			cfg := offlineDateActivationFixture(t, format)
			cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			out, err := ActivateUsageDateRetention(context.Background(), cfg, cutoff)
			if err != nil || out.Activation.Current.SchemaVersion != 10 || out.Activation.AlreadyPublished || !out.Activation.ReclamationPending || out.Verification.Role != "standalone" || out.Verification.RequiredRows != 2 || out.Verification.MatchedRows != 2 || out.BuildSteps == 0 || out.ComparisonSteps == 0 {
				t.Fatal(out, err)
			}
			again, err := ActivateUsageDateRetention(context.Background(), cfg, cutoff)
			if err != nil || !again.Activation.AlreadyPublished || again.Activation.Current != out.Activation.Current || again.Verification.SourceHead != out.Verification.SourceHead || again.BuildSteps != 0 || again.ComparisonSteps == 0 {
				t.Fatal(again, err)
			}
			guard, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal("directory lock escaped", err)
			}
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(cfg.UsagePath(), ".archive-date-index-*"))
			if err != nil || len(paths) != 0 {
				t.Fatal(paths, err)
			}
		})
	}
}

func TestUsageDateActivationIndependentAuthorityAndLateFailure(t *testing.T) {
	for _, fault := range []string{"native-before", "metadata-before", "cancel-before", "cancel-after", "metadata-after", "native-after"} {
		t.Run(fault, func(t *testing.T) {
			cfg := offlineDateActivationFixture(t, usage.FormatNDJSON)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			inject := func() error {
				if fault == "cancel-before" || fault == "cancel-after" {
					cancel()
					return nil
				}
				if fault == "native-before" || fault == "native-after" {
					f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, err = f.Write([]byte("late torn native"))
					return errors.Join(err, f.Close())
				}
				raw, err := os.ReadFile(cfg.MetadataPath())
				if err != nil {
					return err
				}
				if err := os.WriteFile(cfg.MetadataPath()+".replacement", raw, 0o600); err != nil {
					return err
				}
				return os.Rename(cfg.MetadataPath()+".replacement", cfg.MetadataPath())
			}
			var before, after func() error
			if fault == "cancel-after" || fault == "metadata-after" || fault == "native-after" {
				after = inject
			} else {
				before = inject
			}
			out, err := activateUsageDateRetention(ctx, cfg, cutoff, before, after)
			if err == nil || out != (UsageDateActivationReport{}) {
				t.Fatal("failed authority returned report", out, err)
			}
			if fault == "cancel-before" || fault == "cancel-after" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			root, err := os.ReadFile(filepath.Join(cfg.UsagePath(), "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			published := bytes.Contains(root, []byte(`"schema_version":10`))
			if published != (after != nil) {
				t.Fatal("root publication bypassed independent authority", string(root))
			}
			if after != nil {
				if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
					t.Fatal("late failure erased obligation", err)
				}
			}
			guard, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUsageDateActivationRefusesHAMemberAndDowngrade(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			cfg.Replication = nil
		}
		if out, err := ActivateUsageDateRetention(context.Background(), cfg, cutoff); err == nil || out != (UsageDateActivationReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}
