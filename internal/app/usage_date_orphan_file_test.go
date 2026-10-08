package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageDateOrphanFileFreshNativeBothFormatsAndMissingRetry(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			cfg, original, cutoff := offlineDateFileReclaimFixture(t, format)
			if original.Format != format {
				t.Fatal("fixture physical format does not match requested format", original.Format, format)
			}
			originalPath := filepath.Join(cfg.UsagePath(), filepath.FromSlash(original.Path))
			raw, err := os.ReadFile(originalPath)
			if err != nil {
				t.Fatal(err)
			}
			path := "date=" + original.Date + "/orphan-not-in-catalog" + filepath.Ext(original.Path)
			absolute := filepath.Join(cfg.UsagePath(), filepath.FromSlash(path))
			if err := os.WriteFile(absolute, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := make(map[string][]byte)
			for _, p := range []string{cfg.LedgerPath(), filepath.Join(cfg.UsagePath(), "manifest.json"), filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")} {
				before[p], err = os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
			}
			out, err := ReclaimUsageDateOrphanFile(context.Background(), cfg, cutoff, path)
			want := original
			want.Path = path
			if err != nil || out.Entry != want || !out.Reclamation.Removed || !out.Reclamation.ObligationPending || out.Reclamation.RowsVerified != int(original.Records) || out.Verification.Role != "standalone" || out.ComparisonSteps == 0 || out.ReferenceSteps == 0 {
				t.Fatal(out, want, err)
			}
			if _, err := os.Stat(originalPath); err != nil {
				t.Fatal("original partition removed", err)
			}
			again, err := ReclaimUsageDateOrphanFile(context.Background(), cfg, cutoff, path)
			if err != nil || !again.Reclamation.AlreadyMissing || again.Entry != (usage.ManifestFile{}) || again.Reclamation.LogicalBytesUnlinked != 0 || again.Verification.SourceHead != out.Verification.SourceHead {
				t.Fatal(again, err)
			}
			for p, expected := range before {
				actual, err := os.ReadFile(p)
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatal("root/native/policy changed", p, err)
				}
			}
			// Recreated data can be inspected even after the original page chain
			// and data were physically processed and all App owners were closed.
			pages, err := ReclaimUsageDatePagesStep(context.Background(), cfg, cutoff)
			if err != nil || pages.Reclamation.Phase != "ready" {
				t.Fatal(pages, err)
			}
			if err := os.WriteFile(originalPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			recreated, err := ReclaimUsageDateOrphanFile(context.Background(), cfg, cutoff, original.Path)
			if err != nil || recreated.Entry != original || !recreated.Reclamation.Removed || !recreated.Reclamation.ObligationPending {
				t.Fatal("recreated object needed deleted original pages", recreated, err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDateOrphanFileIndependentAuthorityAndLateFailure(t *testing.T) {
	for _, fault := range []string{"native-before", "metadata-before", "cancel-before", "native-after", "metadata-after", "cancel-after", "content"} {
		t.Run(fault, func(t *testing.T) {
			cfg, original, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
			path := "date=" + original.Date + "/orphan-fence" + filepath.Ext(original.Path)
			absolute := filepath.Join(cfg.UsagePath(), filepath.FromSlash(path))
			raw, err := os.ReadFile(filepath.Join(cfg.UsagePath(), filepath.FromSlash(original.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(absolute, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inject := func() error {
				switch fault {
				case "cancel-before", "cancel-after":
					cancel()
					return nil
				case "content":
					return os.WriteFile(absolute, []byte("foreign content"), 0600)
				case "native-before", "native-after":
					f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						return err
					}
					_, err = f.Write([]byte("torn native"))
					return errors.Join(err, f.Close())
				default:
					raw, err := os.ReadFile(cfg.MetadataPath())
					if err != nil {
						return err
					}
					if err := os.WriteFile(cfg.MetadataPath()+".replacement", raw, 0600); err != nil {
						return err
					}
					return os.Rename(cfg.MetadataPath()+".replacement", cfg.MetadataPath())
				}
			}
			var before, after func() error
			if fault == "cancel-after" || fault == "native-after" || fault == "metadata-after" {
				after = inject
			} else {
				before = inject
			}
			out, err := reclaimUsageDateOrphanFile(ctx, cfg, cutoff, path, before, after)
			if err == nil || out != (UsageDateOrphanFileReclaimReport{}) {
				t.Fatal("failed whole operation retained derived descriptor/scope", out, err)
			}
			_, statErr := os.Stat(absolute)
			if after != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("actual unlink did not survive late error", statErr, err)
				}
			} else if statErr != nil {
				t.Fatal("pre-unlink refusal removed orphan", statErr)
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
				t.Fatal("policy lost", err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDateOrphanFileRefusesHAMemberAndDowngrade(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			cfg.Replication = nil
		}
		if out, err := ReclaimUsageDateOrphanFile(context.Background(), cfg, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), "date=2026-10-03/orphan.ndjson"); err == nil || out != (UsageDateOrphanFileReclaimReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}
