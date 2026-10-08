package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskset"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

const doctorMaintenanceRAMBytes int64 = 96 << 20
const doctorReportBytes = 256 << 10
const doctorReportChecks = 64

var errDoctorRetirement = errors.New("doctor physical retirement is uncertain")

type doctorJobKey struct{}
type doctorJob struct {
	root   *commandResourceScope
	permit resourcebudget.Permit
	debt   error
}

// The native page, structural decoder, one joined provider/deployment, fixed
// proxy registry and bounded report share a whole-job envelope before source IO.
// Inner Ledger references retain their independent mapping/workspace fees.
func doctorBudgetContext(ctx context.Context, cfg config.Config) (context.Context, func() error, error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return ctx, nil, err
	}
	root, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if previous, ok := ctx.Value(doctorJobKey{}).(*doctorJob); ok {
		if previous.root != root {
			return ctx, nil, resourcebudget.ErrInvalid
		}
		return ctx, func() error { return nil }, nil
	}
	job := &doctorJob{root: root}
	if cfg.ResourceBudget != nil {
		if pressure.logicalBudget() == nil {
			return ctx, nil, resourcebudget.ErrInvalid
		}
		job.permit, err = pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: doctorMaintenanceRAMBytes})
		if err != nil {
			return ctx, nil, err
		}
	}
	finish := func() error {
		if job.debt != nil {
			return job.debt
		}
		if job.permit == (resourcebudget.Permit{}) {
			return nil
		}
		err := job.permit.Release()
		if err == nil {
			job.permit = resourcebudget.Permit{}
		}
		return err
	}
	return context.WithValue(ctx, doctorJobKey{}, job), finish, nil
}

func doctorClose(ctx context.Context, closer interface{ Close() error }) error {
	err := closer.Close()
	if err != nil {
		err = errors.Join(errDoctorRetirement, err)
		if job, ok := ctx.Value(doctorJobKey{}).(*doctorJob); ok {
			job.debt = errors.Join(job.debt, err)
		}
	}
	return err
}

// DoctorWithReport holds its root envelope through the caller's actual output
// operation. Its callback receives a finite value, after all native readers close.
func DoctorWithReport(ctx context.Context, cfg config.Config, options DoctorOptions, consume func(DoctorReport) error) (resultErr error) {
	if consume == nil {
		return resourcebudget.ErrInvalid
	}
	ctx, finish, err := doctorBudgetContext(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, finish(), ctx.Err()) }()
	report, err := DoctorWithOptions(ctx, cfg, options)
	return errors.Join(err, consume(report))
}

// This is a distinct paid derivative, even though it shares the configured
// disposable reference ceiling. It never borrows an online provider coupon.
func withDoctorFanOutCounter(ctx context.Context, cfg config.Config, work func(*diskset.Set) error) (resultErr error) {
	ctx, finish, err := doctorBudgetContext(ctx, cfg)
	if err != nil {
		return err
	}
	var cleanupErr error
	defer func() {
		if cleanupErr != nil {
			if job, ok := ctx.Value(doctorJobKey{}).(*doctorJob); ok {
				job.debt = errors.Join(job.debt, cleanupErr)
			}
		}
		resultErr = errors.Join(resultErr, cleanupErr, finish(), ctx.Err())
	}()
	_, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return err
	}
	owner := newProviderMaintenanceOwner(cfg, pressure)
	if owner.err != nil {
		return owner.err
	}
	var permit resourcebudget.Permit
	if owner.budgeted {
		permit, err = owner.manager.TryAcquireRecovery(resourcebudget.Cost{Disk: owner.limit})
		if err != nil {
			return err
		}
	}
	defer func() {
		if cleanupErr == nil && permit != (resourcebudget.Permit{}) {
			resultErr = errors.Join(resultErr, permit.Release())
		}
	}()
	directory, err := os.MkdirTemp("", "halro-doctor-fanout-*")
	if err != nil {
		return err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, removeDoctorReferenceDirectory(directory, identity)) }()
	set, err := diskset.Create(ctx, filepath.Join(directory, "counts"), owner.limit)
	if err != nil {
		if errors.Is(err, diskset.ErrRetirement) {
			cleanupErr = err
		}
		return err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, set.Discard()) }()
	return work(set)
}

func doctorRetirementDebt(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if job, ok := ctx.Value(doctorJobKey{}).(*doctorJob); ok {
		job.debt = errors.Join(job.debt, errDoctorRetirement, err)
	}
}
func doctorConstructorDebt(ctx context.Context, err error) {
	if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, boltstore.ErrMetadataFileRetirement) || errors.Is(err, vault.ErrMasterKeyRetirement) || errors.Is(err, ledger.ErrSegmentRetirement) {
		doctorRetirementDebt(ctx, err)
	}
}

// A successful rmdir is followed by an actual parent Sync and Close before fees
// return. A Close failure remains observable instead of being discarded by defer.
func removeDoctorReferenceDirectory(path string, identity os.FileInfo) error {
	if err := removeOwnedReferenceDirectory(path, identity); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
