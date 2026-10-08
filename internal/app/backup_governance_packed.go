package app

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Native is a borrowed, independently qualified original owner. No cleanup
// path closes, repairs, deletes or changes its authority. Cause also retains
// any nested builder debt, including its actual source reader and workspace.
type packedBackupGovernanceFailure struct {
	Cause         error
	Native        *governance.Log
	Projection    *governance.GovernanceProjection
	Fee           resourcebudget.Permit
	Directory     *os.File
	Parent        *os.File
	Publication   *ownedfile.Owner
	PublicationFD *os.File
}

func (e *packedBackupGovernanceFailure) Error() string { return e.Cause.Error() }
func (e *packedBackupGovernanceFailure) Unwrap() error { return e.Cause }

type packedBackupGovernancePhase func(string, *packedBackupGovernanceFailure, string) error

func validatePackedBackupGovernanceProjection(ctx context.Context, cfg config.Config, native *governance.Log, key []byte, expected governance.Summary, pressure *resourcePressureRuntime, parent string, phase packedBackupGovernancePhase) error {
	if cfg.ResourceBudget == nil || !cfg.ResourceBudget.MemoryStateRequired || pressure == nil || pressure.monitor == nil || pressure.logicalBudget() == nil || native == nil || len(key) != 32 {
		return governance.ErrGovernanceSealSource
	}
	return pressure.monitor.WithSampling(ctx, func(ctx context.Context) error {
		return buildPackedBackupGovernanceProjection(ctx, cfg, native, key, expected, pressure, parent, phase)
	})
}

func buildPackedBackupGovernanceProjection(ctx context.Context, cfg config.Config, native *governance.Log, key []byte, expected governance.Summary, pressure *resourcePressureRuntime, parent string, phase packedBackupGovernancePhase) (err error) {
	d := &packedBackupGovernanceFailure{Native: native}
	var cleanupErr error
	var directory string
	var identity, parentIdentity os.FileInfo
	defer func() {
		// The actual descendants join before their captured namespace receipts
		// can retire. Unknown reader exit or foreign replacement retains the
		// complete remaining owners and fee; no recursive removal is attempted.
		if d.Projection != nil {
			cleanupErr = errors.Join(cleanupErr, d.Projection.Discard())
		}
		if errors.Is(err, ownedfile.ErrRetirement) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		if cleanupErr == nil && d.PublicationFD != nil {
			cleanupErr = ownedfile.CloseFile(d.PublicationFD)
			if cleanupErr == nil {
				d.PublicationFD = nil
			}
		}
		if cleanupErr == nil && d.Publication != nil {
			cleanupErr = d.Publication.Retire()
		}
		if cleanupErr == nil && d.Directory != nil {
			actual, statErr := d.Directory.Stat()
			named, nameErr := os.Lstat(directory)
			actualParent, parentErr := d.Parent.Stat()
			namedParent, parentNameErr := os.Lstat(filepath.Dir(directory))
			if statErr != nil || nameErr != nil || parentErr != nil || parentNameErr != nil || !actual.IsDir() || !named.IsDir() || !os.SameFile(actual, identity) || !os.SameFile(named, identity) || !os.SameFile(actualParent, parentIdentity) || !os.SameFile(namedParent, parentIdentity) {
				cleanupErr = errors.Join(ownedfile.ErrReplaced, statErr, nameErr, parentErr, parentNameErr)
			} else {
				cleanupErr = d.Directory.Sync()
				if cleanupErr == nil {
					cleanupErr = removeOwnedReferenceDirectory(directory, identity)
				}
				if cleanupErr == nil {
					cleanupErr = d.Parent.Sync()
				}
			}
		}
		for _, file := range []**os.File{&d.Directory, &d.Parent} {
			if cleanupErr == nil && *file != nil {
				cleanupErr = ownedfile.CloseFile(*file)
				if cleanupErr == nil {
					*file = nil
				}
			}
		}
		if cleanupErr != nil {
			d.Cause = errors.Join(err, ownedfile.ErrRetirement, cleanupErr)
			err = d
			return
		}
		if d.Fee != (resourcebudget.Permit{}) {
			if feeErr := d.Fee.Release(); feeErr != nil {
				d.Cause = errors.Join(err, ownedfile.ErrRetirement, feeErr)
				err = d
			}
		}
	}()
	manager, budget := pressure.logicalBudget(), cfg.ResourceBudget
	d.Fee, err = manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096, Disk: 8192})
	if err != nil {
		return err
	}
	if native.Summary() != expected {
		return governance.ErrGovernanceSealSource
	}
	if err = native.VerifyHead(ctx, expected); err != nil {
		return err
	}
	directory, err = os.MkdirTemp(parent, ".halro-governance-packed-check-*")
	if err != nil {
		return err
	}
	d.Directory, err = os.OpenFile(directory, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		cleanupErr = err
		return err
	}
	identity, err = d.Directory.Stat()
	if err != nil {
		cleanupErr = err
		return err
	}
	d.Parent, err = os.OpenFile(filepath.Dir(directory), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		cleanupErr = err
		return err
	}
	parentIdentity, err = d.Parent.Stat()
	if err != nil {
		cleanupErr = err
		return err
	}
	root := filepath.Join(directory, "governance")
	d.PublicationFD, d.Publication, err = ownedfile.Create(lock.PublicationPath(root), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if phase != nil {
		if err = phase("publication-created", d, directory); err != nil {
			return err
		}
	}
	if err = ownedfile.CloseFile(d.PublicationFD); err != nil {
		cleanupErr = err
		return err
	}
	d.PublicationFD = nil
	var identityKey [32]byte
	if _, err = rand.Read(identityKey[:]); err != nil {
		return err
	}
	d.Projection, _, err = governance.BuildPackedTypedGovernanceProjection(ctx, governance.AuthenticatedGovernanceOptions{
		Native: native, Identity: identityKey, RootKey: key, Budget: manager,
		RootDirectory: root, RootMaxDiskBytes: budget.GovernanceProjectionDiskBytes,
		RootMaxAppendBytes: min(budget.GovernanceProjectionDiskBytes/2, 16<<20),
		ProjectionPath:     filepath.Join(directory, "scratch"), ProjectionMaxDiskBytes: budget.GovernanceProjectionDiskBytes,
	})
	if err != nil {
		return err
	}
	if phase != nil {
		if err = phase("candidate-built", d, directory); err != nil {
			return err
		}
	}
	head, err := d.Projection.ReadHead(ctx)
	if err != nil {
		return err
	}
	if head != expected || native.Summary() != expected {
		return governance.ErrGovernanceSealSource
	}
	return errors.Join(native.VerifyHead(ctx, expected), ctx.Err())
}
