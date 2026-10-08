package app

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

type UsageDateReclaimStepReport struct {
	Reclamation                                        usage.DateReclaimProgress
	Verification                                       usage.ReconciliationReport
	ComparisonSteps, ReferenceSteps, RevalidationSteps uint64
}

// Fresh offline Standalone reference scope, finite new data work. Reopening
// revalidates the saved prefix before extension; the whole offline reference
// operation still has its existing one-minute deadline and no saved DB trust.
func ReclaimUsageDateStep(ctx context.Context, cfg config.Config, retainFrom time.Time) (UsageDateReclaimStepReport, error) {
	return reclaimUsageDateStep(ctx, cfg, retainFrom, nil, nil)
}

func reclaimUsageDateStep(ctx context.Context, cfg config.Config, retainFrom time.Time, beforeRevalidate, afterReclaim func() error) (report UsageDateReclaimStepReport, err error) {
	defer func() {
		if err != nil {
			report = UsageDateReclaimStepReport{}
		}
	}()
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return report, usage.ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if cfg.Replication != nil {
		return report, errors.New("offline Usage date reclaim is unavailable on HA members")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "reclaim Usage date step"); err != nil {
		return report, err
	}
	var compared usage.ArchiveDateVerification
	scope, err := withUsageDiskReferenceArchivePublication(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		var err error
		compared, report.ComparisonSteps, report.ReferenceSteps, err = prepareUsageDateReclaim(ctx, p, e, retainFrom)
		return err
	}, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		for {
			var err error
			report.Reclamation, err = e.ReclaimRetainedDateStep(ctx, p, retainFrom, usage.DefaultDateReclaimStepLimits())
			if err != nil {
				return err
			}
			if report.Reclamation.Phase != "revalidating" {
				break
			}
			report.RevalidationSteps++
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

func prepareUsageDateReclaim(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, retainFrom time.Time) (compared usage.ArchiveDateVerification, comparisons, references uint64, err error) {
	for {
		out, err := e.VerifyPublishedDateCatalogStep(ctx, p, retainFrom, usage.DefaultDateCatalogVerifyLimits())
		if err != nil {
			return compared, comparisons, references, err
		}
		comparisons++
		if out.Phase == "ready" {
			compared = out.Report
			break
		}
	}
	for {
		out, err := e.PrepareDateCatalogReferencesStep(ctx, retainFrom, usage.DefaultDateCatalogReferenceLimits())
		if err != nil {
			return compared, comparisons, references, err
		}
		references++
		if out.Phase == "ready" {
			return compared, comparisons, references, nil
		}
	}
}
