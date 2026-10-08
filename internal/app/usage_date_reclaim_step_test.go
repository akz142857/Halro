package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageDateReclaimStepFreshNativeAndReadyReopen(t *testing.T) {
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
			out, err := ReclaimUsageDateStep(context.Background(), cfg, cutoff)
			if err != nil || out.Reclamation.Phase != "ready" || out.Reclamation.Visited != 1 || out.Reclamation.Eligible != 1 || out.Reclamation.FilesRemoved != 1 || out.Reclamation.RowsVerified != int(entry.Records) || !out.Reclamation.ObligationPending || out.Verification.Role != "standalone" || out.ComparisonSteps == 0 || out.ReferenceSteps == 0 {
				t.Fatal(out, err)
			}
			again, err := ReclaimUsageDateStep(context.Background(), cfg, cutoff)
			if err != nil || again.Reclamation.Phase != "ready" || again.Reclamation.Visited != 1 || again.Reclamation.FilesRemoved != 0 || again.Reclamation.FilesMissing != 0 || again.Reclamation.RowsVerified != 0 || again.Reclamation.LogicalBytesUnlinked != 0 || again.Verification.SourceHead != out.Verification.SourceHead {
				t.Fatal(again, err)
			}
			for path, before := range map[string][]byte{cfg.LedgerPath(): native, filepath.Join(cfg.UsagePath(), "manifest.json"): root} {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal(path, err)
				}
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
				t.Fatal(err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDateReclaimStepIndependentAuthorityAndLateFailure(t *testing.T) {
	for _, fault := range []string{"native-before", "metadata-before", "cancel-before", "native-after", "metadata-after", "cancel-after"} {
		t.Run(fault, func(t *testing.T) {
			cfg, entry, cutoff := offlineDateFileReclaimFixture(t, usage.FormatNDJSON)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
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
				if err = os.WriteFile(cfg.MetadataPath()+".replacement", raw, 0o600); err != nil {
					return err
				}
				return os.Rename(cfg.MetadataPath()+".replacement", cfg.MetadataPath())
			}
			var before, after func() error
			if fault == "native-after" || fault == "metadata-after" || fault == "cancel-after" {
				after = inject
			} else {
				before = inject
			}
			out, err := reclaimUsageDateStep(ctx, cfg, cutoff, before, after)
			if err == nil || out != (UsageDateReclaimStepReport{}) {
				t.Fatal("invalid authority returned success", out, err)
			}
			if fault == "cancel-before" || fault == "cancel-after" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			_, statErr := os.Stat(filepath.Join(cfg.UsagePath(), filepath.FromSlash(entry.Path)))
			if after != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("late failure bypassed actual unlink", statErr)
				}
			} else if statErr != nil {
				t.Fatal("pre-unlink failure deleted target", statErr)
			}
			if _, err := os.Stat(filepath.Join(cfg.UsagePath(), ".archive-date-activation-v1.json")); err != nil {
				t.Fatal("failure erased obligation", err)
			}
			checkDateReclaimCleanup(t, cfg)
		})
	}
}

func TestUsageDateReclaimStepRefusesHAMemberAndDowngrade(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	for _, downgrade := range []bool{false, true} {
		if downgrade {
			cfg.Replication = nil
		}
		if out, err := ReclaimUsageDateStep(context.Background(), cfg, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil || out != (UsageDateReclaimStepReport{}) {
			t.Fatal(downgrade, out, err)
		}
	}
}

func TestUsageDateReclaimStepOfflineRepeatedCallsPastReplayChunk(t *testing.T) {
	r, _ := summaryRuntime(t, 0)
	for i := 0; i < 70; i++ {
		request, err := r.accounting.BeginRequestDetailed(context.Background(), "project_1", "key_1", fmt.Sprintf("reclaim_%d", i), "chat")
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := r.accounting.ReserveAttemptDetailed(context.Background(), request, 100_000, 100, budget.AttemptMetadata{RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1", AttemptNumber: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.MarkStarted(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Settle(context.Background(), attempt, budget.Settlement{CommittedMicrosUSD: 90, ProviderInputTokens: 7, ProviderOutputTokens: 3, Outcome: "success", LatencyMillis: 12}); err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Finalize(context.Background(), request, "success"); err != nil {
			t.Fatal(err)
		}
	}
	r.stopBackgroundWork()
	cfg := r.config
	cfg.Usage.ExportFormat = usage.FormatNDJSON
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Only this test's derived archive is rebuilt into one-row immutable files.
	if err := os.RemoveAll(cfg.UsagePath()); err != nil {
		t.Fatal(err)
	}
	_, err := withUsageDiskReference(context.Background(), cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		limits := usage.DefaultDiskExportLimits()
		limits.Source.Rows, limits.Source.PageRows = 1, 1
		for i := 0; i < 256; i++ {
			cut, err := e.ExportDiskStep(ctx, p, limits)
			if err != nil {
				return err
			}
			if cut.ThroughSequence == cut.Head.Sequence {
				return nil
			}
		}
		return errors.New("one-row export did not finish")
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	activation, err := ActivateUsageDateRetention(context.Background(), cfg, cutoff)
	if err != nil || activation.Activation.Previous.Files != 70 || activation.Activation.Current.Files != 0 {
		t.Fatal(activation, err)
	}
	removed := 0
	for i := 0; i < 32; i++ {
		out, err := ReclaimUsageDateStep(context.Background(), cfg, cutoff)
		if err != nil || out.Reclamation.FilesRemoved > 4 || out.Reclamation.EntriesProcessed > 64 {
			t.Fatal(i, out, err)
		}
		removed += out.Reclamation.FilesRemoved
		if out.Reclamation.Phase == "ready" {
			if removed != 70 || out.Reclamation.Visited != 70 || out.Reclamation.Eligible != 70 {
				t.Fatal(removed, out)
			}
			again, err := ReclaimUsageDateStep(context.Background(), cfg, cutoff)
			if err != nil || again.Reclamation.Phase != "ready" || again.RevalidationSteps == 0 || again.Reclamation.FilesRemoved != 0 || again.Reclamation.Visited != 70 {
				t.Fatal("fresh owner could not cross readonly replay chunks", again, err)
			}
			checkDateReclaimCleanup(t, cfg)
			return
		}
	}
	t.Fatal("fresh offline calls did not finish finite data work")
}
