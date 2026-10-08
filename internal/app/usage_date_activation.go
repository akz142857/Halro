package app

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

type UsageDateActivationReport struct {
	Activation                  usage.DateCatalogActivation
	Verification                usage.ReconciliationReport
	BuildSteps, ComparisonSteps uint64
}

// ActivateUsageDateRetention changes logical archive coverage only, preserving
// physical data and one durable old-root obligation. It does not perform prune,
// enable default disk Runtime, authenticate a saved DB or trim native facts.
// Offline HA activation is refused; role-owned Runtime publication is separate.
func ActivateUsageDateRetention(ctx context.Context, cfg config.Config, retainFrom time.Time) (UsageDateActivationReport, error) {
	return activateUsageDateRetention(ctx, cfg, retainFrom, nil, nil)
}

func activateUsageDateRetention(ctx context.Context, cfg config.Config, retainFrom time.Time, beforeRevalidate func() error, afterActivate func() error) (report UsageDateActivationReport, err error) {
	defer func() {
		if err != nil {
			report = UsageDateActivationReport{}
		}
	}()
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return report, usage.ErrDiskUsageLimit
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if cfg.Replication != nil {
		return report, errors.New("offline Usage date activation is unavailable on HA members; use role-owned maintenance")
	}
	if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "activate Usage date retention"); err != nil {
		return report, err
	}
	var compared usage.ArchiveDateVerification
	scope, err := withUsageDiskReferenceArchivePublication(ctx, cfg, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		position, err := e.ReadArchivePosition(ctx)
		if err != nil {
			return err
		}
		published := position.Retention != (usage.ArchiveDateRetention{}) && position.Retention.RetainFromDate == retainFrom.UTC().Format("2006-01-02")
		if !published {
			if _, err := e.MigrateArchiveCatalog(ctx, time.Minute); err != nil {
				return err
			}
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				out, err := e.PrepareRetainedDateCatalogStep(ctx, retainFrom, usage.DefaultDateCatalogBuildLimits())
				if err != nil {
					return err
				}
				report.BuildSteps++
				if out.Phase == "ready" {
					break
				}
			}
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			var out usage.DateCatalogVerifyProgress
			if published {
				out, err = e.VerifyPublishedDateCatalogStep(ctx, p, retainFrom, usage.DefaultDateCatalogVerifyLimits())
			} else {
				out, err = e.VerifyRetainedDateCatalogStep(ctx, p, retainFrom, usage.DefaultDateCatalogVerifyLimits())
			}
			if err != nil {
				return err
			}
			report.ComparisonSteps++
			if out.Phase == "ready" {
				compared = out.Report
				return nil
			}
		}
	}, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter, scope usage.ReconciliationReport) error {
		var err error
		report.Activation, err = e.ActivateRetainedDateCatalog(ctx, p, retainFrom)
		if err != nil {
			return err
		}
		if afterActivate != nil {
			return afterActivate()
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
