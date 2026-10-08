package app

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
)

type packedBackupUsageFailure struct {
	Cause         error
	Native        *ledger.Log
	Lease         *ledger.MaintenanceLease
	Ledger        *ledger.AuthenticatedLedgerProjection
	Usage         *usage.DiskProjection
	Fee           resourcebudget.Permit
	Directory     *os.File
	Publication   [2]*ownedfile.Owner
	PublicationFD [2]*os.File
	Source        *os.File // combined job's captured, read-only native identity
}

func (e *packedBackupUsageFailure) Error() string { return e.Cause.Error() }
func (e *packedBackupUsageFailure) Unwrap() error { return e.Cause }

// These are fresh disposable roots built from the excluded authenticated
// source, never a checkpoint or query-window authority. The actual Replica
// lease prevents repair and semantic mutation of staged/native backup inputs.
func withPackedBackupReference(ctx context.Context, cfg config.Config, source string, key []byte, expected ledger.ChainReport, pressure *resourcePressureRuntime, parent string, needNative bool, work func(context.Context, *packedBackupUsageFailure, string, [32]byte) error) (err error) {
	b, manager := cfg.ResourceBudget, pressure.logicalBudget()
	if b == nil || !b.MemoryStateRequired || manager == nil || pressure.monitor == nil || work == nil || len(key) != 32 || expected.ChecksumOnly != 0 {
		return ledger.ErrProjectionSource
	}
	return pressure.monitor.WithSampling(ctx, func(ctx context.Context) error {
		return buildPackedBackupReference(ctx, cfg, source, key, expected, pressure, parent, needNative, work)
	})
}

func buildPackedBackupReference(ctx context.Context, cfg config.Config, source string, key []byte, expected ledger.ChainReport, pressure *resourcePressureRuntime, parent string, needNative bool, work func(context.Context, *packedBackupUsageFailure, string, [32]byte) error) (err error) {
	b, manager := cfg.ResourceBudget, pressure.logicalBudget()
	d := &packedBackupUsageFailure{}
	var cleanupErr error
	defer func() {
		if cleanupErr != nil || errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ledger.ErrSegmentRetirement) {
			d.Cause = errors.Join(err, ownedfile.ErrRetirement, cleanupErr)
			err = d
		} else if d.Fee != (resourcebudget.Permit{}) {
			err = errors.Join(err, d.Fee.Release())
		}
	}()
	var acquireErr error
	d.Fee, acquireErr = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ledger.AuthenticatedLedgerSourceRAMBytes, Disk: 2 * 4096})
	if acquireErr != nil {
		return acquireErr
	}
	actual, partial, authErr := ledger.InspectReplayAuthenticatedContext(ctx, source, key, nil)
	if authErr != nil || partial || actual != expected {
		return errors.Join(authErr, usage.ErrCollectorHead)
	}
	directory, err := os.MkdirTemp(parent, "halro-backup-reference-packed-*")
	if err != nil {
		return err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return err
	}
	defer func() {
		cleanupErr = errors.Join(cleanupErr, removeOwnedReferenceDirectory(directory, identity))
		err = errors.Join(err, cleanupErr)
	}()
	// Acquire creates sibling publication guards that intentionally outlive
	// normal storage handles. Capture their actual creation before builders;
	// they can be retired only after every descendant closes successfully.
	defer func() {
		if cleanupErr == nil {
			for _, owner := range d.Publication {
				if owner != nil {
					cleanupErr = errors.Join(cleanupErr, owner.Retire())
				}
			}
		}
		err = errors.Join(err, cleanupErr)
	}()
	roots := []string{"ledger"}
	if needNative {
		roots = append(roots, "usage")
	}
	for slot, root := range roots {
		path := lock.PublicationPath(filepath.Join(directory, root))
		file, owner, createErr := ownedfile.Create(path, os.O_RDWR, 0600)
		if createErr != nil {
			return createErr
		}
		d.Publication[slot], d.PublicationFD[slot] = owner, file
		if closeErr := ownedfile.CloseFile(file); closeErr != nil {
			cleanupErr = errors.Join(cleanupErr, closeErr)
			return closeErr
		}
	}
	if needNative {
		d.Native, err = ledger.OpenWithOptions(source, ledger.NewStatus(), ledger.Options{ChainKey: key, Replica: true, RequireExisting: true, MaxSegmentDirectoryIndexBytes: b.LedgerSegmentIndexDiskBytes})
		if err != nil {
			return err
		}
		defer func() { cleanupErr = errors.Join(cleanupErr, d.Native.Close()); err = errors.Join(err, cleanupErr) }()
		d.Lease, err = d.Native.BeginReplicaReadMaintenance(ctx)
		if err != nil {
			return err
		}
		defer func() { cleanupErr = errors.Join(cleanupErr, d.Lease.Close()); err = errors.Join(err, cleanupErr) }()
	}
	var identityKey [32]byte
	if _, err := rand.Read(identityKey[:]); err != nil {
		return err
	}
	appendBytes := func(n int64) int64 { return min(n/2, 16<<20) }
	d.Ledger, err = ledger.BuildPackedAuthenticatedLedgerProjection(ctx, source, key, filepath.Join(directory, "ledger"), expected, ledger.AuthenticatedLedgerOptions{Identity: identityKey, Budget: manager, MaxDiskBytes: b.LedgerProjectionDiskBytes, MaxAppendBytes: appendBytes(b.LedgerProjectionDiskBytes)})
	if err != nil {
		return err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, d.Ledger.Discard()); err = errors.Join(err, cleanupErr) }()
	defer func() {
		if d.Usage != nil {
			cleanupErr = errors.Join(cleanupErr, d.Usage.Discard())
			err = errors.Join(err, cleanupErr)
		}
	}()
	return work(ctx, d, directory, identityKey)
}

func withPackedBackupUsageReference(ctx context.Context, cfg config.Config, source string, key []byte, expected ledger.ChainReport, pressure *resourcePressureRuntime, work func(context.Context, *usage.DiskProjection) error) error {
	return withPackedBackupReference(ctx, cfg, source, key, expected, pressure, "", true, func(ctx context.Context, d *packedBackupUsageFailure, directory string, identity [32]byte) error {
		return buildPackedBackupUsage(ctx, cfg, key, pressure, d, directory, identity, work)
	})
}

func buildPackedBackupUsage(ctx context.Context, cfg config.Config, key []byte, pressure *resourcePressureRuntime, d *packedBackupUsageFailure, directory string, identity [32]byte, work func(context.Context, *usage.DiskProjection) error) error {
	b := cfg.ResourceBudget
	var err error
	d.Usage, _, err = usage.BuildPackedTypedHAUsageProjection(ctx, usage.AuthenticatedUsageOptions{Native: d.Native, Owner: d.Ledger, Identity: identity, UsageKey: key, Budget: pressure.logicalBudget(), RootDirectory: filepath.Join(directory, "usage"), RootMaxDiskBytes: b.UsageProjectionDiskBytes, RootMaxAppendBytes: min(b.UsageProjectionDiskBytes/2, 16<<20), ScratchPath: filepath.Join(directory, "usage-scratch"), ScratchMaxDiskBytes: b.UsageProjectionDiskBytes}, d.Lease)
	if err != nil {
		return err
	}
	return errors.Join(work(ctx, d.Usage), ctx.Err())
}
