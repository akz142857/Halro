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

func offlineDateFileReclaimFixture(t *testing.T, format string) (config.Config, usage.ManifestFile, time.Time) {
	t.Helper()
	cfg := offlineDateActivationFixture(t, format)
	e, err := usage.NewExporterWithOptions(cfg.UsagePath(), usage.Options{Format: format})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := e.LoadManifest()
	if err := errors.Join(err, e.Close()); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("no expired fixture partition")
	}
	cutoff := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	activated, err := ActivateUsageDateRetention(context.Background(), cfg, cutoff)
	if err != nil || activated.Activation.Current.Files != 0 {
		t.Fatal(activated, err)
	}
	return cfg, manifest.Files[0], cutoff
}

func checkDateReclaimCleanup(t *testing.T, cfg config.Config) {
	t.Helper()
	for _, pattern := range []string{".archive-date-index-*", ".archive-date-ref-index-*", ".archive-date-reclaim-spool-*"} {
		paths, err := filepath.Glob(filepath.Join(cfg.UsagePath(), pattern))
		if err != nil || len(paths) != 0 {
			t.Fatal("physical scratch escaped cleanup", paths, err)
		}
	}
	guard, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal("directory lock escaped cleanup", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUsageDateFileReclaimFreshNativeAndMissingRetry(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			cfg, entry, cutoff := offlineDateFileReclaimFixture(t, format)
			native, err := os.ReadFile(cfg.LedgerPath())
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.ReadFile(filepath.Join(cfg.UsagePath(), "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := ReclaimUsageDateFile(context.Background(), cfg, cutoff, entry)
			if err != nil || !out.Reclamation.Removed || !out.Reclamation.ObligationPending || out.Reclamation.RowsVerified != int(entry.Records) || out.Verification.Role != "standalone" || out.Verification.RequiredRows != 0 || out.Verification.ExpiredOmittedRows != 2 || out.ComparisonSteps == 0 || out.ReferenceSteps == 0 {
				t.Fatal(out, err)
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), filepath.FromSlash(entry.Path))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			again, err := ReclaimUsageDateFile(context.Background(), cfg, cutoff, entry)
			if err != nil || !again.Reclamation.AlreadyMissing || again.Reclamation.Removed || !again.Reclamation.ObligationPending || again.Reclamation.LogicalBytesUnlinked != 0 || again.Reclamation.RowsVerified != 0 || again.Verification.SourceHead != out.Verification.SourceHead {
				t.Fatal(again, err)
			}
			for path, before := range map[string][]byte{cfg.LedgerPath(): native, filepath.Join(cfg.UsagePath(), "manifest.json"): root} {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("reclaim changed native facts/root", path, err)
				}
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
				t.Fatal("unfinished obligation erased", err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDateFileReclaimIndependentAuthorityAndLateFailure(t *testing.T) {
	for _, fault := range []string{"native-before", "metadata-before", "cancel-before", "native-after", "metadata-after", "cancel-after"} {
		t.Run(fault, func(t *testing.T) {
			cfg, entry, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inject := func() error {
				switch fault {
				case "cancel-before", "cancel-after":
					cancel()
					return nil
				case "native-before", "native-after":
					f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, err = f.Write([]byte("late torn native"))
					return errors.Join(err, f.Close())
				default:
					raw, err := os.ReadFile(cfg.MetadataPath())
					if err != nil {
						return err
					}
					if err := os.WriteFile(cfg.MetadataPath()+".replacement", raw, 0o600); err != nil {
						return err
					}
					return os.Rename(cfg.MetadataPath()+".replacement", cfg.MetadataPath())
				}
			}
			var before, after func() error
			if fault == "native-after" || fault == "metadata-after" || fault == "cancel-after" {
				after = inject
			} else {
				before = inject
			}
			out, err := reclaimUsageDateFile(ctx, cfg, cutoff, entry, before, after)
			if err == nil || out != (UsageDateFileReclaimReport{}) {
				t.Fatal("failed authority returned success scope", out, err)
			}
			if fault == "cancel-before" || fault == "cancel-after" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			_, statErr := os.Stat(filepath.Join(cfg.UsagePath(), filepath.FromSlash(entry.Path)))
			if after != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("late failure undid/failed actual unlink", statErr)
				}
			} else if statErr != nil {
				t.Fatal("pre-unlink authority failure deleted target", statErr)
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
				t.Fatal("failure erased pending obligation", err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDateFileReclaimRefusesHAMemberAndDowngrade(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			cfg.Replication = nil
		}
		if out, err := ReclaimUsageDateFile(context.Background(), cfg, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), usage.ManifestFile{}); err == nil || out != (UsageDateFileReclaimReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}
