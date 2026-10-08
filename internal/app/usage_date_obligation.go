package app

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

type UsageDateObligationCompletionReport struct {
	Completion                      usage.DateObligationCompletion
	Verification                    usage.ReconciliationReport
	ComparisonSteps, ReferenceSteps uint64
}

// Persist the current authenticated policy and original named-chain processing.
// This preserves authority for a later handoff, leaving orphan reclamation pending.
// Native construction and caller publication/cleanup fences remain independent.
func CompleteUsageDateObligation(ctx context.Context, cfg config.Config, retainFrom time.Time) (UsageDateObligationCompletionReport, error) {
	return completeUsageDateObligation(ctx, cfg, retainFrom, nil, nil)
}

func completeUsageDateObligation(ctx context.Context, cfg config.Config, retainFrom time.Time, beforeRevalidate, afterReclaim func() error) (report UsageDateObligationCompletionReport, err error) {
	defer func() {
		if err != nil {
			report = UsageDateObligationCompletionReport{}
		}
	}()
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return report, usage.ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if cfg.Replication != nil {
		return report, errors.New("offline Usage date obligation completion is unavailable on HA members")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "complete original Usage date obligation"); err != nil {
		return report, err
	}
	var compared usage.ArchiveDateVerification
	scope, err := withUsageDiskReferenceArchivePublication(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		var err error
		compared, report.ComparisonSteps, report.ReferenceSteps, err = prepareUsageDateReclaim(ctx, p, e, retainFrom)
		return err
	}, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		var err error
		report.Completion, err = e.CompleteRetainedDateObligation(ctx, p, retainFrom)
		if err != nil {
			return err
		}
		if afterReclaim != nil {
			return afterReclaim()
		}
		return nil
	}, beforeRevalidate)
	if err != nil {
		return report, err
	}
	scope.AfterSequence, scope.ThroughSequence = compared.AfterSequence, compared.ThroughSequence
	scope.RetainFromDate = compared.RetainFromDate
	scope.LedgerRecords, scope.ParquetRecords = int64(compared.SourceRows), int64(compared.ArchiveRows)
	scope.RequiredRows, scope.MatchedRows, scope.ExpiredOmittedRows = int64(compared.RequiredRows), int64(compared.MatchedRows), int64(compared.ExpiredOmittedRows)
	report.Verification = scope
	return report, nil
}
