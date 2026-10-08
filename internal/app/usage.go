package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

func VerifyUsage(ctx context.Context, cfg config.Config) (usage.ReconciliationReport, error) {
	return verifyUsageScopePolicy(ctx, cfg, 0, nil, nil, true)
}

func manifestAbsent(cfg config.Config) bool {
	_, err := os.Stat(filepath.Join(cfg.UsagePath(), "manifest.json"))
	return errors.Is(err, os.ErrNotExist)
}

// SummaryRebuildReport is what a rebuild wrote, so an operator can compare it
// against what the console shows instead of taking the command's word for it.
type SummaryRebuildReport struct {
	Watermark      ledger.Watermark `json:"watermark"`
	RollupVersion  int              `json:"rollup_version"`
	RollupRows     int              `json:"rollup_rows"`
	AccountingDays int              `json:"accounting_days"`
}

// pruneUsageWindow trims the console's aggregate back to its window.
//
// Two things decide what goes: the cutoff, and how far the export has actually
// got. The second is not a refinement — export selects attempts above the
// Parquet manifest's watermark, so anything trimmed below it is archived and
// anything trimmed above it is destroyed. Passing the watermark through means
// a stalled or disabled export stops the trimming with it: an aggregate that
// grows is a problem, an aggregate that quietly discards unarchived history is
// a defect.
//
// It runs after the export, on the same tick, for the same reason.
func (r *Runtime) pruneUsageWindow() {
	if r.usage.disk != nil {
		// One finite transaction per tick. The exporter requires a proof for
		// this fresh owner/root; a persisted watermark alone cannot authorize
		// removal. Member retention needs its separate role-owned protocol.
		if r.replication != nil || r.consoleWindowDays() < 1 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cutoff := time.Now().UTC().AddDate(0, 0, -r.consoleWindowDays())
		result, err := r.usageExporter.PruneVerifiedWindowStep(ctx, r.usage.disk, cutoff, usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20})
		if err != nil {
			r.logger.Warn("usage disk window not trimmed", "error", err)
			return
		}
		if result.Attempts != 0 || result.Summaries != 0 || result.Versions != 0 {
			r.logger.Info("usage disk window trimmed", "attempts", result.Attempts,
				"requests", result.Summaries, "versions", result.Versions,
				"floor", result.Window.Floor, "done", result.Done)
		}
		return
	}
	window := r.consoleWindowDays()
	if window < 1 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manifest, err := r.usageExporter.ReadArchivePosition(ctx)
	if err != nil {
		// No manifest is the ordinary state of an instance that has not
		// exported yet, and any other read failure is the export being unwell.
		// Either way the watermark is unknown, so nothing is trimmed.
		if !errors.Is(err, os.ErrNotExist) {
			r.logger.Warn("usage window not trimmed: the export manifest could not be read", "error", err)
		}
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -window)
	result := r.usage.PruneBefore(cutoff, manifest.LastSequence)
	if result.Attempts == 0 && result.Summaries == 0 {
		return
	}
	r.logger.Info("usage window trimmed",
		"attempts", result.Attempts, "requests", result.Summaries,
		"window_days", window, "floor", result.Floor)
}

// pruneUsageArchive removes exported partitions past the archive's retention.
//
// The same sweep `halro usage prune` performs, moved onto the maintenance tick.
// It was a manual, offline command — it takes the data directory lock, so it
// required stopping the gateway — which meant the setting shipped since the
// beginning did nothing on an instance nobody remembered to stop. A retention
// value that only takes effect when someone runs a command is a retention value
// an auditor cannot be shown.
//
// The cutoff keeps one extra day on purpose; usage.RetentionCutoff says why,
// and is the same rule the offline `usage prune` command applies.
func (r *Runtime) pruneUsageArchive() {
	if r.usage.disk != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := r.maintainUsageDiskDates(ctx, time.Now()); err != nil {
			r.logger.Warn("usage disk archive not pruned", "error", err)
		}
		return
	}
	days := r.config.Usage.RetentionDays
	if days < 1 {
		return
	}
	cutoff := usage.RetentionCutoff(time.Now(), days)
	report, err := r.usageExporter.PruneBefore(cutoff)
	if err != nil {
		r.logger.Warn("usage archive not pruned", "error", err)
		return
	}
	if report.FilesRemoved == 0 {
		return
	}
	r.logger.Info("usage archive pruned",
		"files", report.FilesRemoved, "rows", report.RowsRemoved,
		"cutoff", report.Cutoff, "retention_days", days)
}
