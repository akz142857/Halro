package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

func TestRebuildUsageSummaryDiskFiniteNativeRowsRepeatAndRestart(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	installLegacyUsageForTest(t, r) // Preserve the independent incremental rollup oracle.
	for i := 0; i < 45; i++ {
		req, err := r.accounting.BeginRequestDetailed(context.Background(), "project_1", "key_1", fmt.Sprint("disk_summary_", i), fmt.Sprint("chat_", i))
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := r.accounting.ReserveAttemptDetailed(context.Background(), req, 100000, 100, budget.AttemptMetadata{ProviderID: fmt.Sprint("provider_", i), ProviderModel: fmt.Sprint("model_", i), AttemptNumber: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.MarkStarted(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Settle(context.Background(), attempt, budget.Settlement{CommittedMicrosUSD: 90, ProviderInputTokens: 7, ProviderOutputTokens: 3, Outcome: "success", LatencyMillis: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Finalize(context.Background(), req, "success"); err != nil {
			t.Fatal(err)
		}
	}
	r.saveUsageCheckpoint() // Flush the explicit incremental oracle before Close.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	want := allRollupRows(t, cfg.MetadataPath())
	if len(want) <= 100 {
		t.Fatal("fixture did not cross real page boundary", len(want))
	}
	s, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := s.LedgerChainCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Existing foreign rows are derivatives, never input to the new summary.
	var key domain.RollupKey
	for k := range want {
		key = k
		break
	}
	stampForeignRollup(t, cfg.MetadataPath(), ledger.Watermark{Generation: anchor.Generation, Sequence: anchor.Sequence, Offset: anchor.Offset}, key)
	before := doctorTree(t, cfg.Storage.DataDir)
	metadataName, err := filepath.Rel(cfg.Storage.DataDir, cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	delete(before, metadataName)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	for _, slot := range []int{1, 2, 1} {
		report, err := RebuildUsageSummary(context.Background(), cfg)
		if err != nil || report.RollupRows != len(want) || report.AccountingDays != 1 || report.Watermark.Sequence != 225 || report.RollupVersion != domain.RollupVersion {
			t.Fatal(report, err)
		}
		if !reflect.DeepEqual(allRollupRows(t, cfg.MetadataPath()), want) {
			t.Fatal("rebuild changed native-derived totals")
		}
		s, err := boltstore.OpenReadOnly(cfg.MetadataPath())
		if err != nil {
			t.Fatal(err)
		}
		state, err := s.UsageRollupState()
		if err != nil || state.Slot != slot {
			t.Fatal(state, err)
		}
		_, marker, err := s.UsageCheckpoint()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := usage.RestoreCheckpoint(marker, s.UsageCheckpointSegmentPayload); err == nil {
			t.Fatal("untrusted disk marker restored as Aggregate")
		}
		nextAnchor, err := s.LedgerChainCheckpoint()
		if err != nil || nextAnchor != anchor {
			t.Fatal("rebuild changed native deletion anchor", nextAnchor, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		after := doctorTree(t, cfg.Storage.DataDir)
		delete(after, metadataName)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("summary rebuild changed non-Metadata files")
		}
		entries, err := os.ReadDir(scratch)
		if err != nil || len(entries) != 0 {
			t.Fatal("reference cleanup failed", entries, err)
		}
	}
	// Current default disk startup ignores the marker and rebuilds from native
	// WAL. It must not add replay increments to the published Metadata slot.
	r, err = Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	r.saveUsageCheckpoint()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allRollupRows(t, cfg.MetadataPath()), want) {
		t.Fatal("restart doubled or discarded rollups")
	}
}

func TestRebuildUsageSummaryDiskFinalFailureAndCancellationWithdrawReports(t *testing.T) {
	for _, mode := range []string{"before-proof", "after-publish-cancel", "after-publish-native"} {
		t.Run(mode, func(t *testing.T) {
			cfg := compactDiskFixture(t, usage.FormatNDJSON, "missing")
			before := doctorTree(t, cfg.Storage.DataDir)
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var beforeCheck, afterPublish func() error
			switch mode {
			case "before-proof":
				beforeCheck = func() error { return errors.New("injected final proof error") }
			case "after-publish-cancel":
				afterPublish = func() error { cancel(); return nil }
			case "after-publish-native":
				afterPublish = func() error {
					f, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, err = f.Write([]byte("late torn source"))
					return errors.Join(err, f.Close())
				}
			}
			report, err := rebuildUsageSummary(ctx, cfg, beforeCheck, afterPublish)
			if err == nil || report != (SummaryRebuildReport{}) {
				t.Fatal("failure escaped success report", report, err)
			}
			if mode == "before-proof" && !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
				t.Fatal("failed proof mutated existing derivatives")
			}
			if mode == "after-publish-cancel" {
				if _, err := RebuildUsageSummary(context.Background(), cfg); err != nil {
					t.Fatal("completed but unacknowledged root could not retry", err)
				}
			}
			entries, err := os.ReadDir(scratch)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed operation retained reference", entries, err)
			}
			guard, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal("directory lock retained", err)
			}
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRebuildUsageSummaryDiskRefusesMemberAndPreCancelledMissingDirectory(t *testing.T) {
	cfg, _, _, _, _ := offlineUsageMemberFixture(t)
	before := doctorTree(t, cfg.Storage.DataDir)
	for _, configured := range []bool{true, false} {
		if !configured {
			cfg.Replication = nil
		}
		if report, err := RebuildUsageSummary(context.Background(), cfg); err == nil || report != (SummaryRebuildReport{}) || !reflect.DeepEqual(before, doctorTree(t, cfg.Storage.DataDir)) {
			t.Fatal("offline rebuild mutated member", configured, report, err)
		}
	}
	cfg = testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := RebuildUsageSummary(ctx, cfg); !errors.Is(err, context.Canceled) || report != (SummaryRebuildReport{}) {
		t.Fatal(report, err)
	}
	if _, err := os.Stat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled rebuild created data", err)
	}
}

func TestRebuildUsageSummaryDiskAcceptsPagedCatalogWithoutArchiveWrites(t *testing.T) {
	for _, format := range []string{usage.FormatParquet, usage.FormatNDJSON} {
		t.Run(format, func(t *testing.T) {
			cfg := compactDiskFixture(t, format, "paged")
			before := doctorTree(t, cfg.UsagePath())
			if report, err := RebuildUsageSummary(context.Background(), cfg); err != nil || report.Watermark.Sequence != 35 || report.AccountingDays != 1 {
				t.Fatal(report, err)
			}
			if !reflect.DeepEqual(before, doctorTree(t, cfg.UsagePath())) {
				t.Fatal("summary rebuild changed archive")
			}
		})
	}
}
