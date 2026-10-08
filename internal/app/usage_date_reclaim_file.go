package app

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

type UsageDateFileReclaimReport struct {
	Reclamation                     usage.DateFileReclamation
	Verification                    usage.ReconciliationReport
	ComparisonSteps, ReferenceSteps uint64
}

// Offline Standalone, one explicit derived partition only. Native authentication,
// fresh references and final Metadata/native checks surround the actual unlink.
// No root/catalog/obligation/native facts or shared physical fees are removed.
func ReclaimUsageDateFile(ctx context.Context, cfg config.Config, retainFrom time.Time, entry usage.ManifestFile) (UsageDateFileReclaimReport, error) {
	return reclaimUsageDateFile(ctx, cfg, retainFrom, entry, nil, nil)
}

func reclaimUsageDateFile(ctx context.Context, cfg config.Config, retainFrom time.Time, entry usage.ManifestFile, beforeRevalidate, afterReclaim func() error) (report UsageDateFileReclaimReport, err error) {
	return reclaimUsageDateTarget(ctx, cfg, retainFrom, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter) (usage.DateFileReclamation, error) {
		return e.ReclaimRetainedDateFile(ctx, p, retainFrom, entry, usage.DefaultDateFileReclaimLimits())
	}, beforeRevalidate, afterReclaim)
}

func reclaimUsageDateTarget(ctx context.Context, cfg config.Config, retainFrom time.Time, reclaim func(context.Context, *usage.DiskProjection, *usage.Exporter) (usage.DateFileReclamation, error), beforeRevalidate, afterReclaim func() error) (report UsageDateFileReclaimReport, err error) {
	defer func() {
		if err != nil {
			report = UsageDateFileReclaimReport{}
		}
	}()
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return report, usage.ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if cfg.Replication != nil {
		return report, errors.New("offline Usage date file reclaim is unavailable on HA members")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "reclaim Usage date file"); err != nil {
		return report, err
	}
	var compared usage.ArchiveDateVerification
	scope, err := withUsageDiskReferenceArchivePublication(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		var err error
		compared, report.ComparisonSteps, report.ReferenceSteps, err = prepareUsageDateReclaim(ctx, p, e, retainFrom)
		return err
	}, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		var err error
		report.Reclamation, err = reclaim(ctx, p, e)
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
