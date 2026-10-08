package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

// The excluded staged native cut supplies one independent fresh reference for
// both checks. Nothing escapes this job, and activation still builds its own
// roots. Unpublished Usage must still perform the pricing check.
func withBackupPricingAndUsageReference(ctx context.Context, cfg config.Config, metadata *boltstore.Store, root, source string, key []byte, expected ledger.ChainReport, parent string, pressure *resourcePressureRuntime, work func(*usage.Exporter, usage.ArchivePosition) error) (position usage.ArchivePosition, err error) {
	defer func() {
		if err != nil {
			position = usage.ArchivePosition{}
		}
	}()
	if cfg.ResourceBudget == nil || !cfg.ResourceBudget.MemoryStateRequired {
		if err := validateBackupPricingProjection(ctx, cfg, metadata, source, key, expected, parent); err != nil {
			return position, fmt.Errorf("validate pricing references: %w", err)
		}
		return withBackupUsageReference(ctx, cfg, root, source, key, expected, usageArchiveDiskCeiling(cfg), pressure, work)
	}
	ctx, _, err = commandBudgetContext(ctx, cfg)
	if err != nil {
		return position, err
	}
	pressure, err = commandPressure(ctx, cfg, pressure)
	if err != nil {
		return position, err
	}
	err = withPackedBackupReference(ctx, cfg, source, key, expected, pressure, parent, true, func(ctx context.Context, d *packedBackupUsageFailure, directory string, identity [32]byte) (jobErr error) {
		// The original job has already acquired its source workspace. Keep an
		// actual FD across both checks so an identical-byte path replacement
		// cannot silently change which staged source is being borrowed.
		d.Source, jobErr = os.Open(source)
		if jobErr != nil {
			return jobErr
		}
		defer func() {
			if closeErr := ownedfile.CloseFile(d.Source); closeErr != nil {
				jobErr = errors.Join(jobErr, ownedfile.ErrRetirement, closeErr)
			} else {
				d.Source = nil
			}
		}()
		original, jobErr := d.Source.Stat()
		if jobErr != nil || !original.Mode().IsRegular() {
			return errors.Join(ledger.ErrProjectionSource, jobErr)
		}
		ref, jobErr := d.Ledger.ReadUsageSourceReference(ctx)
		if jobErr != nil {
			return jobErr
		}
		check := func(ctx context.Context) error {
			if err := checkBackupSharedSource(ctx, d, source, key, expected, original); err != nil {
				return err
			}
			if err := d.Ledger.ValidateResourceOwner(ctx, pressure.logicalBudget()); err != nil {
				return err
			}
			if err := d.Ledger.ValidateNativeSource(ctx, d.Native, d.Lease); err != nil {
				return err
			}
			current, err := d.Ledger.ReadUsageSourceReference(ctx)
			if err != nil || current != ref {
				return errors.Join(ledger.ErrProjectionSource, err)
			}
			return ctx.Err()
		}
		if jobErr = check(ctx); jobErr != nil {
			return jobErr
		}
		if jobErr = d.Ledger.WithFreshPricingView(ctx, func(reader ledger.AuthenticatedPricingView) error {
			return metadata.ValidateDeploymentPriceReferences(ctx, reader)
		}); jobErr != nil {
			return fmt.Errorf("validate pricing references: %w", jobErr)
		}
		if jobErr = check(ctx); jobErr != nil {
			return jobErr
		}
		build := func(ctx context.Context, work func(context.Context, *usage.DiskProjection) error) error {
			if err := check(ctx); err != nil {
				return err
			}
			return buildPackedBackupUsage(ctx, cfg, key, pressure, d, directory, identity, work)
		}
		position, jobErr = withBackupUsageReferenceBuilder(ctx, cfg, root, source, key, expected, usageArchiveDiskCeiling(cfg), pressure, build, work)
		return errors.Join(jobErr, check(ctx))
	})
	return position, err
}

func checkBackupSharedSource(ctx context.Context, d *packedBackupUsageFailure, source string, key []byte, expected ledger.ChainReport, original os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	identity := func() error {
		actual, err := d.Source.Stat()
		named, nameErr := os.Lstat(source)
		if err != nil || nameErr != nil || !os.SameFile(original, actual) || !os.SameFile(actual, named) || !named.Mode().IsRegular() || actual.Size() != original.Size() {
			return errors.Join(ledger.ErrProjectionSource, err, nameErr)
		}
		return ctx.Err()
	}
	if err := identity(); err != nil {
		return err
	}
	// Complete authentication covers rolled generations too. FD identity alone
	// cannot detect an in-place edit or a changed manifest/segment namespace.
	report, partial, err := ledger.InspectReplayAuthenticatedContext(ctx, source, key, nil)
	if err != nil || partial || report != expected {
		return errors.Join(ledger.ErrProjectionSource, err)
	}
	return identity()
}
