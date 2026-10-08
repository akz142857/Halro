package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/diskset"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
	"golang.org/x/sys/unix"
)

const providerMaintenanceRAMBytes int64 = 64 << 20

type providerMaintenanceOwner struct {
	manager  *resourcebudget.Manager
	limit    int64
	budgeted bool
	err      error
}

func newProviderMaintenanceOwner(cfg config.Config, pressure *resourcePressureRuntime) *providerMaintenanceOwner {
	owner := &providerMaintenanceOwner{manager: pressure.logicalBudget(), limit: config.DefaultProviderObjectReferenceDiskBytes, budgeted: cfg.ResourceBudget != nil}
	if cfg.ResourceBudget != nil {
		limit := cfg.ResourceBudget.ProviderObjectReferenceDiskBytes
		if owner.manager == nil || limit < 0 || limit > 1<<40 || limit > 0 && limit < 480 {
			owner.err = resourcebudget.ErrInvalid
		} else if limit > 0 {
			owner.limit = limit
		}
	} else if owner.manager != nil {
		owner.err = resourcebudget.ErrInvalid
	}
	return owner
}

// Includes copied native pages, one <=8MiB fixed-field legacy resource decode,
// the fixed radix path and directory batch. Online work always contends with
// the original IO manager; it may not create a fresh pool or borrow Recovery.
func (r *Runtime) withProviderMaintenance(ctx context.Context, index bool, work func(context.Context, *diskset.Set) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	var permit resourcebudget.Permit
	var cleanupErr error
	// Published Runtime pins its original policy/Manager before background
	// admission. A later caller's pressure pointer cannot substitute this owner.
	var owner *providerMaintenanceOwner
	if r.maintenanceOwners != nil {
		owner = r.maintenanceOwners.provider
	}
	if owner == nil {
		owner = newProviderMaintenanceOwner(r.config, r.resourcePressure)
	}
	if owner.err != nil {
		return owner.err
	}
	limit := owner.limit
	if owner.budgeted {
		cost := resourcebudget.Cost{RAM: providerMaintenanceRAMBytes}
		if index {
			cost.Disk = limit
		}
		var err error
		permit, err = owner.manager.TryAcquireContext(ctx, resourcebudget.IO, cost)
		if err != nil {
			return err
		}
	}
	defer func() {
		if errors.Is(resultErr, errProviderObjectRetirement) || errors.Is(resultErr, ownedfile.ErrRetirement) {
			cleanupErr = errors.Join(cleanupErr, resultErr)
		}
		if cleanupErr == nil && permit != (resourcebudget.Permit{}) {
			resultErr = errors.Join(resultErr, permit.Release())
		}
		resultErr = errors.Join(resultErr, cleanupErr, ctx.Err())
	}()
	if !index {
		return work(ctx, nil)
	}
	directory, err := os.MkdirTemp("", "halro-provider-references-*")
	if err != nil {
		return err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, removeOwnedReferenceDirectory(directory, identity)) }()
	set, err := diskset.Create(ctx, filepath.Join(directory, "references"), limit)
	if err != nil {
		if errors.Is(err, diskset.ErrRetirement) || errors.Is(err, ownedfile.ErrRetirement) {
			cleanupErr = err
		}
		return err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, set.Discard()) }()
	return work(ctx, set)
}

// The reference set is complete at one live Metadata TxID. Each unlink pins
// that same TxID under writer exclusion; any newer reference revokes the sweep.
// A changed source never licenses a best-effort deletion from stale absence.
func (r *Runtime) sweepProviderObjects(ctx context.Context, references *diskset.Set) (swept uint64, resultErr error) {
	return r.sweepProviderObjectsWithMode(ctx, references, false)
}

func (r *Runtime) sweepProviderObjectsWithMode(ctx context.Context, references *diskset.Set, plaintextOnly bool) (swept uint64, resultErr error) {
	info, err := r.store.Info()
	if err != nil {
		return 0, err
	}
	if err := r.store.WalkProviderObjectReferences(ctx, info.TxID, func(reference boltstore.ProviderObjectReference) error {
		for _, path := range [2]string{reference.ObjectPath, reference.InputObjectPath} {
			if path == "" {
				continue
			}
			// Invalid saved paths confer no deletion authority, even if an
			// unrelated basename happens to exist inside the object directory.
			if filepath.Base(path) != path || path == "." || path == ".." {
				return errors.New("provider object reference escapes its directory")
			}
			if err := references.Add(ctx, sha256.Sum256([]byte(path))); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	directory := filepath.Join(r.config.Storage.DataDir, "provider-objects")
	directoryInfo, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil || !directoryInfo.IsDir() {
		return 0, errors.Join(dirscan.ErrIdentity, err)
	}
	parent, err := os.Open(directory)
	if err != nil {
		return 0, err
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errProviderObjectRetirement, closeErr)
		}
	}()
	actual, err := parent.Stat()
	if err != nil || !os.SameFile(directoryInfo, actual) {
		return 0, errors.Join(dirscan.ErrIdentity, err)
	}
	cutoff := time.Now().UTC().Add(-orphanSettlingPeriod)
	err = dirscan.Walk(ctx, directory, func(entry os.DirEntry) error {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || plaintextOnly && strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil || !fileInfo.Mode().IsRegular() || !plaintextOnly && fileInfo.ModTime().UTC().After(cutoff) {
			return err
		}
		named, err := references.Contains(ctx, sha256.Sum256([]byte(entry.Name())))
		if err != nil || named {
			return err
		}
		return r.store.WithProviderObjectReferenceSnapshot(ctx, info.TxID, func() error {
			return r.withProviderObjectMutation(ctx, func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				currentParent, err := os.Lstat(directory)
				if err != nil || !os.SameFile(directoryInfo, currentParent) {
					return errors.Join(dirscan.ErrIdentity, err)
				}
				current, err := os.Lstat(filepath.Join(directory, entry.Name()))
				if err != nil || !os.SameFile(fileInfo, current) || !current.Mode().IsRegular() || !current.ModTime().Equal(fileInfo.ModTime()) || current.Size() != fileInfo.Size() {
					return errors.Join(errors.New("provider object changed before unlink"), err)
				}
				if plaintextOnly {
					header, inspected, err := inspectObjectHeader(filepath.Join(directory, entry.Name()))
					if err != nil {
						return err
					}
					if vault.SealedEnvelope(header) {
						return nil
					}
					if !os.SameFile(fileInfo, inspected) || inspected.Size() != fileInfo.Size() || !inspected.ModTime().Equal(fileInfo.ModTime()) {
						return dirscan.ErrIdentity
					}
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := unix.Unlinkat(int(parent.Fd()), entry.Name(), 0); err != nil {
					return err
				}
				swept++
				return parent.Sync()
			})
		})
	})
	return swept, err
}
