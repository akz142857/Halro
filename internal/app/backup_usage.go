package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// Both the offline backup and its staged restore authenticate an independent
// fresh disk reference. Prior checkpoints and the live trimmed query DB supply
// no archive rows or authority. The default paged backend also streams its
// physical file catalog; this job does not reconstruct a historical file slice.
func withBackupUsageReference(ctx context.Context, cfg config.Config, root, source string, key []byte, expected ledger.ChainReport, archiveDiskBytes int64, pressure *resourcePressureRuntime, work func(*usage.Exporter, usage.ArchivePosition) error) (position usage.ArchivePosition, err error) {
	return withBackupUsageReferenceBuilder(ctx, cfg, root, source, key, expected, archiveDiskBytes, pressure, nil, work)
}

// A builder supplied by the combined offline job borrows its one fresh Ledger
// owner only inside that job's callback. It is never stored on the exporter.
type backupPackedUsageBuilder func(context.Context, func(context.Context, *usage.DiskProjection) error) error

func withBackupUsageReferenceBuilder(ctx context.Context, cfg config.Config, root, source string, key []byte, expected ledger.ChainReport, archiveDiskBytes int64, pressure *resourcePressureRuntime, build backupPackedUsageBuilder, work func(*usage.Exporter, usage.ArchivePosition) error) (position usage.ArchivePosition, err error) {
	ctx, _, err = commandBudgetContext(ctx, cfg)
	if err != nil {
		return position, err
	}
	pressure, err = commandPressure(ctx, cfg, pressure)
	if err != nil {
		return position, err
	}
	defer func() {
		err = errors.Join(err, ctx.Err())
		if err != nil {
			position = usage.ArchivePosition{}
		}
	}()
	archiveBudget, releaseArchive, err := newOneShotUsageArchiveDiskBudgetWithPressure(ctx, cfg, pressure)
	if err != nil {
		return position, err
	}
	var reserveArchiveDisk func(context.Context, int64) (func() error, error)
	if archiveBudget != nil {
		reserveArchiveDisk = archiveBudget.reserve
	}
	e, err := usage.NewExporterWithContext(ctx, root, usage.Options{ArchiveDiskBytes: archiveDiskBytes, ReserveArchiveDisk: reserveArchiveDisk, ReserveArchiveRAM: archiveBudget.readerReservation(cfg), ReserveArchiveWorkRAM: archiveBudget.workReservation(), DatePolicyKey: key, DatePolicyPrefix: func(ctx context.Context, head ledger.Watermark) (ledger.ProjectionPrefix, error) {
		prefix, report, err := ledger.InspectProjectionCursor(ctx, source, key, head.Generation, head.Sequence)
		if err != nil || report != expected || prefix.Head != head {
			return ledger.ProjectionPrefix{}, errors.Join(err, usage.ErrArchiveDateProof)
		}
		return prefix, ctx.Err()
	}})
	if err != nil {
		return position, errors.Join(err, releaseArchive())
	}
	defer func() {
		closeErr := e.Close()
		err = errors.Join(err, closeErr)
		if closeErr == nil {
			err = errors.Join(err, releaseArchive())
		}
	}()
	position, err = e.ReadArchivePosition(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return usage.ArchivePosition{}, verifyUnpublishedBackupUsage(ctx, root, source, key, expected, pressure)
	}
	if err != nil {
		return position, err
	}
	if cfg.ResourceBudget != nil && cfg.ResourceBudget.MemoryStateRequired {
		if build == nil {
			build = func(ctx context.Context, work func(context.Context, *usage.DiskProjection) error) error {
				return withPackedBackupUsageReference(ctx, cfg, source, key, expected, pressure, work)
			}
		}
		err = build(ctx, func(ctx context.Context, disk *usage.DiskProjection) error {
			return verifyBackupUsageArchive(ctx, e, disk, position, expected.Head, work)
		})
		return position, err
	}
	referenceBudget, err := acquireUsageReferenceBudget(ctx, cfg, pressure, resourcebudget.Recovery)
	if err != nil {
		return position, err
	}
	var cleanupErr error
	defer func() { err = errors.Join(err, referenceBudget.retire(withRetirementDebt(cleanupErr, err))) }()
	directory, err := os.MkdirTemp("", "halro-backup-usage-*")
	if err != nil {
		return position, err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return position, err
	}
	defer func() {
		cleanupErr = errors.Join(cleanupErr, removeOwnedReferenceDirectory(directory, identity))
		err = errors.Join(err, cleanupErr)
	}()
	var p *ledger.LedgerProjection
	if referenceBudget.ledgerBytes > 0 {
		p, err = ledger.BuildLedgerProjectionWithLimit(ctx, source, key, filepath.Join(directory, "ledger.db"), expected, referenceBudget.ledgerBytes)
	} else {
		p, err = ledger.BuildLedgerProjection(ctx, source, key, filepath.Join(directory, "ledger.db"), expected)
	}
	if err != nil {
		return position, err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, p.Discard()); err = errors.Join(err, cleanupErr) }()
	derivative, disk, err := usage.NewDiskLedgerDerivativeWithLimit(p, filepath.Join(directory, "usage.db"), referenceBudget.usageBytes)
	if err != nil {
		return position, err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, disk.Discard()); err = errors.Join(err, cleanupErr) }()
	defer derivative.Close()
	actual, partial, err := ledger.InspectReplayAuthenticatedContext(ctx, source, key, func(r ledger.Record) error { return derivative.ApplyContext(ctx, r) })
	if err != nil || partial || actual != expected {
		return position, errors.Join(err, usage.ErrCollectorHead)
	}
	err = verifyBackupUsageArchive(ctx, e, disk, position, expected.Head, work)
	return position, err
}

func verifyBackupUsageArchive(ctx context.Context, e *usage.Exporter, disk *usage.DiskProjection, position usage.ArchivePosition, head ledger.Watermark, work func(*usage.Exporter, usage.ArchivePosition) error) (err error) {
	if position.Retention == (usage.ArchiveDateRetention{}) {
		_, err = e.VerifyDiskArchive(ctx, disk, usage.DefaultArchiveVerifyLimits())
	} else {
		cutoff, parseErr := time.Parse("2006-01-02", position.Retention.RetainFromDate)
		if parseErr != nil {
			return parseErr
		}
		if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
			return err
		}
		for {
			out, stepErr := e.VerifyPublishedDateCatalogStep(ctx, disk, cutoff, usage.DefaultDateCatalogVerifyLimits())
			if stepErr != nil {
				err = stepErr
				break
			}
			if out.Phase == "ready" {
				break
			}
		}
	}
	if err != nil {
		return err
	}
	if work != nil {
		err = work(e, position)
	} else {
		err = e.WalkArchiveBackupFiles(ctx, head, func(string) error { return nil })
	}
	return err
}

// A missing manifest supplies no archive authority. Only an actually empty
// excluded directory (or an absent root) may omit reducers. Authenticate the
// complete native source anyway; an orphan file never becomes an empty archive.
func verifyUnpublishedBackupUsage(ctx context.Context, root, source string, key []byte, expected ledger.ChainReport, pressure *resourcePressureRuntime) (err error) {
	d := &packedBackupUsageFailure{}
	var closeErr error
	defer func() {
		if closeErr != nil || errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ledger.ErrSegmentRetirement) {
			d.Cause = errors.Join(err, closeErr, ownedfile.ErrRetirement)
			err = d
		} else if d.Fee != (resourcebudget.Permit{}) {
			err = errors.Join(err, d.Fee.Release())
		}
	}()
	if len(key) != 32 || expected.ChecksumOnly != 0 {
		return ledger.ErrProjectionSource
	}
	if manager := pressure.logicalBudget(); manager != nil {
		d.Fee, err = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ledger.AuthenticatedLedgerSourceRAMBytes})
		if err != nil {
			return err
		}
	}
	info, statErr := os.Lstat(root)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return usage.ErrArchiveDateProof
		}
		f, openErr := os.Open(root)
		if openErr != nil {
			return openErr
		}
		d.Directory = f
		defer func() { closeErr = ownedfile.CloseError(f, f.Close()); err = errors.Join(err, closeErr) }()
		actual, checkErr := f.Stat()
		if checkErr != nil || !os.SameFile(info, actual) {
			return errors.Join(checkErr, usage.ErrArchiveDateProof)
		}
		entries, readErr := f.ReadDir(1)
		if len(entries) != 0 || readErr != nil && !errors.Is(readErr, io.EOF) {
			return errors.Join(readErr, usage.ErrArchiveDateProof)
		}
		current, checkErr := os.Lstat(root)
		if checkErr != nil || !os.SameFile(info, current) {
			return errors.Join(checkErr, usage.ErrArchiveDateProof)
		}
	}
	actual, partial, authErr := ledger.InspectReplayAuthenticatedContext(ctx, source, key, nil)
	if authErr != nil || partial || actual != expected {
		return errors.Join(authErr, usage.ErrCollectorHead)
	}
	// The native authentication can outlast an external namespace change.
	// Recheck the same empty directory (or continued absence) before returning
	// the unpublished position; absence of a manifest alone is insufficient.
	current, checkErr := os.Lstat(root)
	if errors.Is(statErr, os.ErrNotExist) {
		if !errors.Is(checkErr, os.ErrNotExist) {
			return errors.Join(checkErr, usage.ErrArchiveDateProof)
		}
	} else {
		if checkErr != nil || !current.IsDir() || !os.SameFile(info, current) {
			return errors.Join(checkErr, usage.ErrArchiveDateProof)
		}
		if _, seekErr := d.Directory.Seek(0, io.SeekStart); seekErr != nil {
			return seekErr
		}
		entries, readErr := d.Directory.ReadDir(1)
		if len(entries) != 0 || readErr != nil && !errors.Is(readErr, io.EOF) {
			return errors.Join(readErr, usage.ErrArchiveDateProof)
		}
	}
	return ctx.Err()
}
