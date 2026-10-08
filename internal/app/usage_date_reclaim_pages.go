package app

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

type UsageDatePageReclaimReport struct {
	Reclamation                                        usage.DatePageReclaimProgress
	Data                                               usage.DateReclaimProgress
	Verification                                       usage.ReconciliationReport
	ComparisonSteps, ReferenceSteps, RevalidationSteps uint64
}

// One finite new data batch while bootstrapping, then one finite page batch.
// Signed irreversible transitions resume after old pages are gone. Every call
// still builds fresh native references under the existing offline deadline.
func ReclaimUsageDatePagesStep(ctx context.Context, cfg config.Config, retainFrom time.Time) (UsageDatePageReclaimReport, error) {
	return reclaimUsageDatePagesStep(ctx, cfg, retainFrom, nil, nil)
}

func reclaimUsageDatePagesStep(ctx context.Context, cfg config.Config, retainFrom time.Time, beforeRevalidate, afterReclaim func() error) (report UsageDatePageReclaimReport, err error) {
	defer func() {
		if err != nil {
			report = UsageDatePageReclaimReport{}
		}
	}()
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return report, usage.ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if cfg.Replication != nil {
		return report, errors.New("offline Usage date page reclaim is unavailable on HA members")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "reclaim Usage date pages"); err != nil {
		return report, err
	}
	var compared usage.ArchiveDateVerification
	scope, err := withUsageDiskReferenceArchivePublication(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		var err error
		compared, report.ComparisonSteps, report.ReferenceSteps, err = prepareUsageDateReclaim(ctx, p, e, retainFrom)
		return err
	}, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		present, err := e.HasCurrentDatePageReclamation(ctx, p, retainFrom)
		if err != nil {
			return err
		}
		if !present {
			for {
				report.Data, err = e.ReclaimRetainedDateStep(ctx, p, retainFrom, usage.DefaultDateReclaimStepLimits())
				if err != nil {
					return err
				}
				if report.Data.Phase != "revalidating" {
					break
				}
				report.RevalidationSteps++
			}
			if report.Data.Phase != "ready" {
				report.Reclamation = usage.DatePageReclaimProgress{Phase: "data", ObligationPending: true}
				if afterReclaim != nil {
					return afterReclaim()
				}
				return nil
			}
			// The previous successful physical job supplies the private completion
			// witness. Rebuild comparison authority for the next physical job.
			var comparisons, references uint64
			compared, comparisons, references, err = prepareUsageDateReclaim(ctx, p, e, retainFrom)
			report.ComparisonSteps += comparisons
			report.ReferenceSteps += references
			if err != nil {
				return err
			}
		}
		report.Reclamation, err = e.ReclaimRetainedDatePagesStep(ctx, p, retainFrom, usage.DefaultDatePageReclaimLimits())
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
