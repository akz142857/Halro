package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

type usageDateMaintenanceReport struct {
	Current                        usage.ArchivePosition
	Policies, DataSteps, PageSteps uint64
	OriginalChainComplete          bool
}

// Run before this tick's archive append: a saved unpublished intent must keep
// its old root until publication/retry finishes. Each admitted job owns a fresh
// untrimmed reference; all inner mutations retain their finite step limits.
func (r *Runtime) maintainUsageDiskDates(ctx context.Context, now time.Time) (report usageDateMaintenanceReport, err error) {
	defer func() {
		if err != nil {
			report = usageDateMaintenanceReport{}
			if r.usageExporter != nil {
				r.usageExporter.RevokeWindowAuthority()
			}
		}
	}()
	if _, err := r.usageExporter.ReadArchivePosition(ctx); errors.Is(err, os.ErrNotExist) {
		return report, nil
	} else if err != nil {
		return report, err
	}
	err = r.withUsageMaintenanceReference(ctx, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter) error {
		pending, err := e.ReadPendingDatePolicy(ctx, p)
		if err != nil {
			return err
		}
		desired := usage.RetentionCutoff(now, r.config.Usage.RetentionDays)
		if r.config.Usage.RetentionDays < 1 {
			desired = time.Time{}
		}
		compare := func(cutoff time.Time, published bool) error {
			for {
				var out usage.DateCatalogVerifyProgress
				var err error
				if published {
					out, err = e.VerifyPublishedDateCatalogStep(ctx, p, cutoff, usage.DefaultDateCatalogVerifyLimits())
				} else {
					out, err = e.VerifyRetainedDateCatalogStep(ctx, p, cutoff, usage.DefaultDateCatalogVerifyLimits())
				}
				if err != nil {
					return err
				}
				if out.Phase == "ready" {
					return nil
				}
			}
		}
		complete := func(cutoff time.Time) error {
			if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
				return err
			}
			if err := e.ResetDateCatalogReferences(ctx); err != nil {
				return err
			}
			if err := compare(cutoff, true); err != nil {
				return err
			}
			present, err := e.HasCurrentDatePageReclamation(ctx, p, cutoff)
			if err != nil {
				return err
			}
			if !present {
				// Discard only the derivative data hint. Actual prior deletions
				// remain idempotent; signed irreversible page progress is preserved.
				if err := e.ResetRetainedDateReclamation(ctx); err != nil {
					return err
				}
				for {
					if _, _, _, err := prepareUsageDateReclaim(ctx, p, e, cutoff); err != nil {
						return err
					}
					out, err := e.ReclaimRetainedDateStep(ctx, p, cutoff, usage.DefaultDateReclaimStepLimits())
					if err != nil {
						return err
					}
					report.DataSteps++
					if out.Phase == "ready" {
						break
					}
				}
			}
			for {
				if _, _, _, err := prepareUsageDateReclaim(ctx, p, e, cutoff); err != nil {
					return err
				}
				out, err := e.ReclaimRetainedDatePagesStep(ctx, p, cutoff, usage.DefaultDatePageReclaimLimits())
				if err != nil {
					return err
				}
				report.PageSteps++
				if out.Phase == "ready" {
					break
				}
			}
			if _, _, _, err := prepareUsageDateReclaim(ctx, p, e, cutoff); err != nil {
				return err
			}
			out, err := e.CompleteRetainedDateObligation(ctx, p, cutoff)
			if err != nil {
				return err
			}
			report.OriginalChainComplete = out.OriginalChainComplete
			return nil
		}
		position, err := e.ReadArchivePosition(ctx)
		if err != nil {
			return err
		}
		if position.Retention != (usage.ArchiveDateRetention{}) {
			cutoff, err := time.Parse("2006-01-02", position.Retention.RetainFromDate)
			if err != nil {
				return err
			}
			if err := complete(cutoff); err != nil {
				return err
			}
		}
		// At most the interrupted policy plus today's successor. There is no
		// historical policy queue. Each published original chain completes first.
		for round := 0; round < 2; round++ {
			cutoff := desired
			if !pending.IsZero() {
				cutoff, pending = pending, time.Time{}
			}
			if cutoff.IsZero() {
				break
			}
			position, err = e.ReadArchivePosition(ctx)
			if err != nil {
				return err
			}
			date := cutoff.UTC().Format("2006-01-02")
			if position.Retention != (usage.ArchiveDateRetention{}) && (date < position.Retention.RetainFromDate || date == position.Retention.RetainFromDate && position.Retention.PruneThroughSequence == position.LastSequence) {
				break
			}
			if _, err := e.MigrateArchiveCatalog(ctx, time.Minute); err != nil {
				return err
			}
			if err := e.ResetRetainedDateCatalogBuild(ctx); err != nil {
				return err
			}
			if err := e.ResetRetainedDateCatalogVerification(ctx); err != nil {
				return err
			}
			for {
				out, err := e.PrepareRetainedDateCatalogStep(ctx, cutoff, usage.DefaultDateCatalogBuildLimits())
				if err != nil {
					return err
				}
				if out.Phase == "ready" {
					break
				}
			}
			if err := compare(cutoff, false); err != nil {
				return err
			}
			if _, err := e.ActivateRetainedDateCatalog(ctx, p, cutoff); err != nil {
				return err
			}
			report.Policies++
			if err := complete(cutoff); err != nil {
				return err
			}
		}
		// Only full fresh published comparison mints this permission. Real
		// target/source native anchors and current root are checked at binding.
		position, err = e.ReadArchivePosition(ctx)
		if err != nil {
			return err
		}
		if position.Retention == (usage.ArchiveDateRetention{}) {
			if _, err := e.VerifyDiskArchive(ctx, p, usage.DefaultArchiveVerifyLimits()); err != nil {
				return err
			}
		} else {
			cutoff, err := time.Parse("2006-01-02", position.Retention.RetainFromDate)
			if err != nil {
				return err
			}
			if err := compare(cutoff, true); err != nil {
				return err
			}
		}
		if err := e.BindVerifiedWindow(ctx, p, r.usage.disk); err != nil {
			return err
		}
		report.Current = position
		return nil
	})
	return report, err
}

func (r *Runtime) maintainUsageDiskCycle(ctx context.Context, now time.Time) error {
	if r.replication != nil && r.replication.startupProducers != nil && !r.replication.startupReady.Load() {
		return errors.New("Primary Usage maintenance is waiting for confirmed accounting startup")
	}
	if r.usage == nil || r.usage.disk == nil || r.usage.owner == nil {
		return errors.New("disk Usage cycle requires a selected native owner")
	}
	if r.config.Replication != nil || r.replication != nil {
		if r.usage.owner.checkRole == nil {
			return errors.New("disk Usage cycle requires authenticated Primary ownership")
		}
		if err := r.usage.owner.checkRole(); err != nil {
			return err
		}
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		return err
	}
	if _, err := r.maintainUsageDiskDates(ctx, now); err != nil {
		return err
	}
	if _, err := r.usageExporter.ExportDiskStep(ctx, r.usage.disk, usage.DefaultDiskExportStepLimits()); err != nil {
		return err
	}
	if days := r.consoleWindowDays(); days > 0 {
		_, err := r.usageExporter.PruneVerifiedWindowStep(ctx, r.usage.disk, now.UTC().AddDate(0, 0, -days), usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20})
		return err
	}
	return nil
}
