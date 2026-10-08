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

func TestUsageDatePagesFreshBootstrapAndReopenAfterOldPagesGone(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			cfg, _, cutoff := offlineDateFileReclaimFixture(t, format)
			nativeBefore, err := os.ReadFile(cfg.LedgerPath())
			if err != nil {
				t.Fatal(err)
			}
			rootBefore, err := os.ReadFile(filepath.Join(cfg.UsagePath(), "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			policyBefore, err := os.ReadFile(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json"))
			if err != nil {
				t.Fatal(err)
			}
			pages, err := filepath.Glob(filepath.Join(cfg.UsagePath(), "catalog", "v1", "page-*.json"))
			if err != nil || len(pages) == 0 {
				t.Fatal(pages, err)
			}
			out, err := ReclaimUsageDatePagesStep(context.Background(), cfg, cutoff)
			if err != nil || out.Reclamation.Phase != "ready" || !out.Reclamation.ObligationPending || out.Reclamation.PagesRemoved != len(pages) || out.Data.Phase != "ready" || out.Verification.Role != "standalone" || out.ComparisonSteps < 2 || out.ReferenceSteps < 2 {
				t.Fatal(out, err)
			}
			for _, p := range pages {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("old page remains", p, err)
				}
			}
			again, err := ReclaimUsageDatePagesStep(context.Background(), cfg, cutoff)
			if err != nil || again.Reclamation.Phase != "ready" || again.Reclamation.PagesProcessed != 0 || again.Reclamation.LogicalBytesUnlinked != 0 || again.Data != (usage.DateReclaimProgress{}) {
				t.Fatal("reopen needed missing old chain", again, err)
			}
			for path, before := range map[string][]byte{cfg.LedgerPath(): nativeBefore, filepath.Join(cfg.UsagePath(), "manifest.json"): rootBefore, filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json"): policyBefore} {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("authority/current root changed", path, err)
				}
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDatePagesIndependentAuthorityAndLateFailure(t *testing.T) {
	for _, fault := range []string{"native-before", "metadata-before", "cancel-before", "cancel-after", "metadata-after", "native-after"} {
		t.Run(fault, func(t *testing.T) {
			cfg, _, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
			pages, err := filepath.Glob(filepath.Join(cfg.UsagePath(), "catalog", "v1", "page-*.json"))
			if err != nil || len(pages) == 0 {
				t.Fatal(pages, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inject := func() error {
				if fault == "cancel-before" || fault == "cancel-after" {
					cancel()
					return nil
				}
				if fault == "native-before" || fault == "native-after" {
					f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						return err
					}
					_, err = f.Write([]byte("torn native tail"))
					return errors.Join(err, f.Close())
				}
				raw, err := os.ReadFile(cfg.MetadataPath())
				if err != nil {
					return err
				}
				if err := os.WriteFile(cfg.MetadataPath()+".replacement", raw, 0600); err != nil {
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
			out, err := reclaimUsageDatePagesStep(ctx, cfg, cutoff, before, after)
			if err == nil || out != (UsageDatePageReclaimReport{}) {
				t.Fatal("late/independent failure returned scope", out, err)
			}
			if before != nil {
				for _, p := range pages {
					if _, err := os.Stat(p); err != nil {
						t.Fatal("prepublication failure removed old page", err)
					}
				}
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
				t.Fatal("lost policy receipt", err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDatePagesRefusesHAMemberAndDowngrade(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			cfg.Replication = nil
		}
		if out, err := ReclaimUsageDatePagesStep(context.Background(), cfg, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil || out != (UsageDatePageReclaimReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}
