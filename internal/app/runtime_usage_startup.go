package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/usage"
)

type usageStartupWindowReport struct {
	Archive         usage.ArchivePosition
	Window          usage.DiskWindowState
	Verified        bool
	Steps           uint64
	ComparisonSteps uint64
}

// Called after the fresh reducer has reached recovered native history and before
// publishing producers/readiness. Rebuilding never inherits a saved window floor
// or archive permission. Complete native authentication remains a transitional
// cold-start cost; each date comparison and window mutation is a finite step.
func (r *Runtime) restoreUsageDiskWindow(ctx context.Context, lease *ledger.MaintenanceLease, now time.Time) (usageStartupWindowReport, error) {
	if r.usage == nil || r.usage.disk == nil {
		return usageStartupWindowReport{}, usage.ErrArchiveWindowProof
	}
	return r.restoreUsageDiskWindowFromReference(ctx, lease, now, r.usage.disk)
}

func (r *Runtime) restoreUsageDiskWindowFromReference(ctx context.Context, lease *ledger.MaintenanceLease, now time.Time, reference *usage.DiskProjection) (report usageStartupWindowReport, err error) {
	defer func() {
		if err != nil {
			report = usageStartupWindowReport{}
		}
	}()
	if reference == nil || r.usage == nil || r.usage.owner == nil || r.usage.disk != r.usage.owner.disk || r.usage.owner.native != r.ledger || r.usageExporter != r.usage.owner.archive || r.ledger == nil || lease == nil || now.IsZero() {
		return report, errors.New("Usage startup window requires fresh Standalone owners before publication")
	}
	var accounting interface {
		ReadPosition(context.Context) (ledger.Watermark, [32]byte, error)
	}
	if r.accountingState.projection != nil {
		accounting = r.accountingState.projection
	}
	if r.selectedState != nil {
		accounting = r.selectedState.generation.Owners.Ledger
	}
	member := r.config.Replication != nil || r.replication != nil
	if member {
		if r.usage.owner.checkRole == nil {
			return report, errors.New("Usage startup window requires an authenticated Primary owner")
		}
		if err := r.usage.owner.checkRole(); err != nil {
			return report, err
		}
		accounting = r.usage.owner.accounting
	}
	if accounting == nil {
		return report, errors.New("Usage startup window has no selected Ledger evidence")
	}
	if replicated, err := lease.ValidateOwner(r.ledger); err != nil || replicated != member {
		return report, errors.Join(err, errors.New("Usage startup window requires this Standalone native maintenance owner"))
	}
	releaseWorkspace, err := acquireProjectionWorkspace(ctx, r.config, r.resourcePressure)
	if err != nil {
		return report, err
	}
	defer func() {
		if !errors.Is(err, ownedfile.ErrRetirement) {
			err = errors.Join(err, releaseWorkspace())
		}
	}()
	var verifiedHead ledger.Watermark
	var verifiedHash [32]byte
	err = lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		verifiedHead, verifiedHash = head, hash
		checkSource := func() error {
			if member {
				if err := r.usage.owner.checkRole(); err != nil {
					return err
				}
			}
			actual, actualHash, err := accounting.ReadPosition(ctx)
			if err != nil || actual != head || actualHash != hash {
				return errors.Join(err, usage.ErrCollectorHead)
			}
			anchor, err := r.usage.disk.ReadReplayAnchor(ctx)
			if err != nil || anchor.Head.Sequence != head.Sequence || anchor.Hash != hash {
				return errors.Join(err, usage.ErrCollectorHead)
			}
			return ctx.Err()
		}
		if err := checkSource(); err != nil {
			return err
		}
		finishSource := func() error {
			if err := checkSource(); err != nil {
				return err
			}
			// A cached head cannot detect an external mutation of native bytes.
			if r.selectedState != nil {
				// Complete selected source loans own a separate lease operation.
				// Acquire it after this callback exits; the actual maintenance
				// lease continues to exclude ordinary native writers throughout.
				return nil
			}
			actual, err := r.ledger.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, nil)
			if err != nil || actual != head {
				return errors.Join(err, usage.ErrCollectorHead)
			}
			return ctx.Err()
		}
		stats, err := reference.ReadStats(ctx)
		if err != nil {
			return fmt.Errorf("read startup Usage window state: %w", err)
		}
		if stats.Window.AttemptAfter != 0 || stats.Window.SummaryAfter != 0 {
			return errors.New("Usage startup verification requires untrimmed fresh history")
		}
		position, err := r.usageExporter.ReadArchivePosition(ctx)
		if errors.Is(err, os.ErrNotExist) {
			targetStats, err := r.usage.disk.ReadStats(ctx)
			if err != nil {
				return err
			}
			if targetStats.Window.AttemptAfter != 0 || targetStats.Window.SummaryAfter != 0 {
				return usage.ErrArchiveWindowProof
			}
			// Nothing has been archived: all query rows remain. The publisher
			// can establish a new genesis permission on subsequent finite ticks.
			report.Window = stats.Window
			return finishSource()
		}
		if err != nil {
			return fmt.Errorf("read startup Usage archive position: %w", err)
		}
		if position.Retention == (usage.ArchiveDateRetention{}) {
			if _, err := r.usageExporter.VerifyDiskArchive(ctx, reference, usage.DefaultArchiveVerifyLimits()); err != nil {
				return fmt.Errorf("verify startup Usage archive: %w", err)
			}
		} else {
			cutoff, err := time.Parse("2006-01-02", position.Retention.RetainFromDate)
			if err != nil {
				return err
			}
			// Start an explicit fresh comparison for this new owner. Saved
			// Ready is a hint; resetting this hint cannot alter the root,
			// authenticated policy, original obligation or physical data.
			if err := r.usageExporter.ResetPublishedDateCatalogVerification(ctx); err != nil {
				return fmt.Errorf("reset startup Usage date comparison: %w", err)
			}
			for {
				out, err := r.usageExporter.VerifyPublishedDateCatalogStep(ctx, reference, cutoff, usage.DefaultDateCatalogVerifyLimits())
				if err != nil {
					return fmt.Errorf("verify startup Usage date catalog step: %w", err)
				}
				report.ComparisonSteps++
				if out.Phase == "ready" {
					break
				}
			}
		}
		if reference != r.usage.disk {
			if err := r.usageExporter.BindVerifiedWindow(ctx, reference, r.usage.disk); err != nil {
				return err
			}
		}
		report.Archive, report.Verified = position, true
		if days := r.consoleWindowDays(); days > 0 {
			cutoff := now.UTC().AddDate(0, 0, -days)
			for {
				out, err := r.usageExporter.PruneVerifiedWindowStepUnderMaintenance(ctx, r.usage.disk, lease, cutoff, usage.DiskWindowLimits{Rows: 128, InputBytes: 8 << 20})
				if err != nil {
					return fmt.Errorf("prune startup Usage query window: %w", err)
				}
				report.Steps++
				report.Window = out.Window
				if out.Done {
					break
				}
			}
		} else {
			report.Window = stats.Window
		}
		return finishSource()
	})
	if err == nil && r.selectedState != nil {
		err = r.verifySelectedUsageNativeReference(ctx, lease, verifiedHead, verifiedHash)
	}
	return report, err
}

// A saved query window cannot stand in for a complete archive comparison.
// Reconstruct a bounded disposable full-history reducer only when an archive
// exists, then bind that actual comparison to the selected authenticated owner.
func (r *Runtime) restoreSelectedUsageDiskWindow(ctx context.Context, now time.Time) (err error) {
	if r.selectedState == nil || r.usage == nil || r.usage.disk == nil {
		return usage.ErrArchiveWindowProof
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil {
		return err
	}
	archive, err := r.usageExporter.ReadArchivePosition(ctx)
	if errors.Is(err, os.ErrNotExist) {
		if stats.Window.AttemptAfter != 0 || stats.Window.SummaryAfter != 0 {
			return usage.ErrArchiveWindowProof
		}
		return nil
	}
	if err != nil {
		return err
	}
	// A parsed empty archive has no historical facts to compare. This shortcut
	// is valid only while the authenticated query window has discarded none.
	// Non-empty archives and every pruned window still require the full proof.
	if archive.Files == 0 && archive.LastSequence == 0 && stats.Window.Floor == 0 && stats.Window.AttemptAfter == 0 && stats.Window.SummaryAfter == 0 {
		return nil
	}
	// A full archive comparison must not borrow a selected startup source whose
	// historical native audit is still in progress. Join that actual audit first.
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		return err
	}
	lease, err := r.ledger.BeginMaintenance(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	// The actual selected Usage tree authenticates the complete key set while
	// no query rows have been discarded. Its common cut, native historical
	// audit and full archive comparison supply the reference without rebuilding
	// an already authenticated domain. Only a pruned window needs fresh history.
	if stats.Window.Floor == 0 && stats.Window.AttemptAfter == 0 && stats.Window.SummaryAfter == 0 && stats.Window.TrimmedAttempts == 0 {
		_, err = r.restoreUsageDiskWindowFromReference(ctx, lease, now, r.usage.disk)
		return err
	}
	reference, err := r.buildSelectedUsageWindowReference(ctx, lease)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, retireSelectedUsageWindowReference(reference)) }()
	_, err = r.restoreUsageDiskWindowFromReference(ctx, lease, now, reference)
	return err
}
