package app

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

type UsageDateOrphanPageReclaimReport struct {
	Reclamation                                                usage.DateOrphanPageReclaimProgress
	Verification                                               usage.ReconciliationReport
	ComparisonSteps, ReferenceSteps, ReadOnlyScanSteps         uint64
	ReadOnlyEntries, ReadOnlyProtected, ReadOnlyCandidatePages uint64
	ReadOnlyUnrecognized, ReadOnlyMissing                      uint64
}

// Fresh offline Standalone authority and at most one finite actual unlink batch.
// Candidate protection and read-only directory chunks continue on the same fresh
// owner. Authentic original page-chain completion remains a prerequisite.
// Full native construction and total offline work retain the one-minute deadline.
func ReclaimUsageDateOrphanPagesStep(ctx context.Context, cfg config.Config, retainFrom time.Time) (UsageDateOrphanPageReclaimReport, error) {
	return reclaimUsageDateOrphanPagesStep(ctx, cfg, retainFrom, usage.DefaultDateOrphanPageReclaimLimits(), nil, nil)
}

func reclaimUsageDateOrphanPagesStep(ctx context.Context, cfg config.Config, retainFrom time.Time, limits usage.DateOrphanPageReclaimLimits, beforeRevalidate, afterReclaim func() error) (report UsageDateOrphanPageReclaimReport, err error) {
	defer func() {
		if err != nil {
			report = UsageDateOrphanPageReclaimReport{}
		}
	}()
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return report, usage.ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if cfg.Replication != nil {
		return report, errors.New("offline Usage orphan page reclaim is unavailable on HA members")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "reclaim Usage orphan page step"); err != nil {
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
			report.Reclamation, err = e.ReclaimRetainedDateOrphanPagesStep(ctx, p, retainFrom, limits)
			if err != nil {
				return err
			}
			if report.Reclamation.Phase == "scan-complete" || report.Reclamation.PagesRemoved != 0 {
				break
			}
			if report.ReadOnlyScanSteps == ^uint64(0) {
				return usage.ErrDiskUsageLimit
			}
			for _, value := range []struct {
				target *uint64
				count  int
			}{{&report.ReadOnlyEntries, report.Reclamation.EntriesScanned}, {&report.ReadOnlyProtected, report.Reclamation.PagesProtected}, {&report.ReadOnlyCandidatePages, report.Reclamation.CandidatePagesProcessed}, {&report.ReadOnlyUnrecognized, report.Reclamation.EntriesUnrecognized}, {&report.ReadOnlyMissing, report.Reclamation.PagesMissing}} {
				if ^uint64(0)-*value.target < uint64(value.count) {
					return usage.ErrDiskUsageLimit
				}
				*value.target += uint64(value.count)
			}
			report.ReadOnlyScanSteps++
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
