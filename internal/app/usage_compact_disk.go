package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

// No historical Files array is returned. Archive describes the committed root;
// successful completion requires the entire frozen source, final exact native
// row correspondence, independent source authority and physical cleanup.
type UsageCompactionReport struct {
	Archive         usage.ArchivePosition      `json:"archive"`
	AfterSequence   uint64                     `json:"after_sequence"`
	ThroughSequence uint64                     `json:"through_sequence"`
	Steps           int                        `json:"steps"`
	WrittenRows     uint64                     `json:"written_rows"`
	Verification    usage.ReconciliationReport `json:"verification"`
}

type usageCompactionLimits struct {
	Steps  int
	Step   usage.DiskExportLimits
	Verify usage.ArchiveVerifyLimits
}

func defaultUsageCompactionLimits() usageCompactionLimits {
	return usageCompactionLimits{Steps: 64, Step: usage.DefaultDiskExportStepLimits(), Verify: usage.DefaultArchiveVerifyLimits()}
}

// CompactUsage reconstructs fresh disk references and exports finite complete
// prefixes until the frozen target is reached. A failed job can leave already
// committed prefixes or retryable orphans; the next invocation resumes from the
// published root. A partial prefix is never reported as full compaction success.
// Offline mutation remains unavailable on HA members, including downgraded
// config. This operation never trims native facts or grants retention authority.
func CompactUsage(ctx context.Context, cfg config.Config) (UsageCompactionReport, error) {
	return compactUsageDisk(ctx, cfg, defaultUsageCompactionLimits(), nil, nil)
}

// Private seams inject interruption after an actual published step or before
// the independent final authority check, without granting production ownership.
func compactUsageDisk(ctx context.Context, cfg config.Config, limits usageCompactionLimits, afterStep func(int, usage.DiskArchiveCut) error, beforeRevalidate func() error) (report UsageCompactionReport, err error) {
	defer func() {
		if err != nil {
			report = UsageCompactionReport{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if limits.Steps < 1 || limits.Steps > 64 {
		return report, usage.ErrDiskUsageLimit
	}
	if cfg.Replication != nil {
		return report, errors.New("offline usage compact is unavailable on HA members; use role-owned maintenance")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "compact usage"); err != nil {
		return report, err
	}
	var verified usage.ArchiveVerification
	scope, err := withUsageDiskReference(ctx, cfg, func(ctx context.Context, disk *usage.DiskProjection, exporter *usage.Exporter, scope usage.ReconciliationReport) error {
		position, readErr := exporter.ReadArchivePosition(ctx)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if readErr == nil {
			// Verify before any migration/append. An old catalog or valid SHA is
			// insufficient to accept a missing native prefix or wrong row set.
			if _, err := exporter.VerifyDiskArchiveAfter(ctx, disk, 0, limits.Verify); err != nil {
				return fmt.Errorf("verify existing usage before compaction: %w", err)
			}
		}
		report.AfterSequence = position.LastSequence
		through := position.LastSequence
		for step := 0; step < limits.Steps; step++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "compact usage"); err != nil {
				return err
			}
			cut, err := exporter.ExportDiskStepPreservingCatalog(ctx, disk, limits.Step)
			if err != nil {
				return fmt.Errorf("compact usage step: %w", err)
			}
			if cut.Head != scope.SourceHead || cut.AfterSequence != through || cut.ThroughSequence < through || cut.ThroughSequence > scope.SourceHead.Sequence {
				return usage.ErrDiskUsageCorrupt
			}
			report.Steps++
			report.WrittenRows += uint64(cut.Rows)
			if afterStep != nil {
				if err := afterStep(report.Steps, cut); err != nil {
					return err
				}
			}
			if cut.ThroughSequence == scope.SourceHead.Sequence {
				through = cut.ThroughSequence
				break
			}
			if cut.ThroughSequence == through {
				return fmt.Errorf("%w: usage compaction made no complete-prefix progress", usage.ErrDiskUsageCorrupt)
			}
			through = cut.ThroughSequence
		}
		if through != scope.SourceHead.Sequence {
			return fmt.Errorf("%w: usage compaction step limit reached at sequence %d of %d; retry resumes the committed prefix", usage.ErrDiskUsageLimit, through, scope.SourceHead.Sequence)
		}
		position, err := exporter.ReadArchivePosition(ctx)
		if err != nil || position.LastSequence != through || !position.CompletePrefix || position.CatalogVersion != 0 && position.CatalogVersion != 1 {
			return errors.Join(err, usage.ErrDiskUsageCorrupt)
		}
		verified, err = exporter.VerifyDiskArchiveAfter(ctx, disk, 0, limits.Verify)
		if err != nil {
			return fmt.Errorf("verify compacted usage: %w", err)
		}
		report.Archive, report.ThroughSequence = position, through
		return nil
	}, beforeRevalidate)
	if err != nil {
		return report, err
	}
	scope.LedgerRecords, scope.ParquetRecords = int64(verified.SourceRows), int64(verified.ArchiveRows)
	scope.AfterSequence, scope.ThroughSequence = verified.AfterSequence, verified.ThroughSequence
	report.Verification = scope
	return report, nil
}
