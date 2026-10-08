package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/diskset"
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

const providerResourceReapInterval = time.Hour

func (r *Runtime) runProviderResourceMaintenance(ctx context.Context) {
	r.reclaimUnsealedProviderObjects(ctx)
	r.reapProviderResources(ctx)
	r.sweepOrphanedProviderObjects(ctx)
	ticker := time.NewTicker(providerResourceReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reapProviderResources(ctx)
			r.sweepOrphanedProviderObjects(ctx)
		}
	}
}

// orphanSettlingPeriod is how long a sealed object nothing names is left alone
// before it is treated as an orphan.
//
// The sweep cannot ask the directory whether a file is in use: an object is
// written, fsynced and renamed into place before its record is stored, so for a
// moment every legitimate write looks exactly like an orphan. Age is what
// separates them. No write stays in that window for a day; a file that has been
// unnamed for one is a file whose record was never written, or was written and
// then removed without its object.
const orphanSettlingPeriod = 24 * time.Hour

// sweepOrphanedProviderObjects deletes sealed objects no record names.
//
// Two paths produce them, and neither is exotic. A submission writes its object
// and then fails to store the record — the compensating delete is best effort
// and can itself fail. A settlement writes the answer and then fails to save
// the record, in which case there is no compensation at all. Both leave a
// sealed prompt or a sealed answer on disk with nothing pointing at it.
//
// Until this existed nothing removed them. The startup reclamation walks the
// records, so it cannot see a file with no record, and the one scan that does
// walk the directory removes unsealed objects only — it is the migration that
// cleared plaintext left by releases before sealing, and it skips sealed files
// deliberately, because at that point in startup it cannot tell a live write
// from an orphan. So the material outlived every TTL the resource model
// promises, invisible to doctor, to backup and to the reaper.
//
// The bound this restores is the one ADR 0024 states: material a caller wrote
// has a life measured in hours, not in the lifetime of the installation.
func (r *Runtime) sweepOrphanedProviderObjects(ctx context.Context) {
	var swept uint64
	err := r.withProviderMaintenance(ctx, true, func(ctx context.Context, references *diskset.Set) error {
		var err error
		swept, err = r.sweepProviderObjects(ctx, references)
		return err
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		r.logger.Error("provider object sweep refused or interrupted", "error", err)
	}
	if swept > 0 {
		r.logger.Warn("swept provider objects no record names", "count", swept)
	}
}

func (r *Runtime) reapProviderResources(ctx context.Context) {
	now := time.Now().UTC()
	err := r.withProviderMaintenance(ctx, false, func(ctx context.Context, _ *diskset.Set) error {
		return r.store.WalkProviderResourcesMaintenance(ctx, func(resource domain.ProviderResource) error {
			if !resource.ExpiresAt.After(now) && resource.ExpiryReapable() {
				if err := r.gatewayService.CleanupExpiredProviderResource(ctx, resource); err != nil {
					r.logger.Error("provider resource cleanup failed", "resource_id", resource.ID, "error", err)
				}
			}
			return ctx.Err()
		})
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		r.logger.Error("provider resource TTL reaper refused or interrupted", "error", err)
	}
}

// reclaimUnsealedProviderObjects drops the records whose object was written
// before provider objects were sealed.
//
// Those bytes cannot be served: the reader now expects an envelope, and there
// is no key that opens a file that was never sealed. Keeping the record would
// leave a resource that answers metadata queries and fails every read, and
// keeping the file would leave a plaintext prompt or model output on disk for
// as long as the install lives — which is the thing sealing exists to end. One
// summary line is logged rather than one per record: the count is the operator's
// business, the identifiers are not.
func (r *Runtime) reclaimUnsealedProviderObjects(ctx context.Context) {
	directory := filepath.Join(r.config.Storage.DataDir, "provider-objects")
	reclaimed := uint64(0)
	err := r.withProviderMaintenance(ctx, true, func(ctx context.Context, references *diskset.Set) error {
		if err := r.store.WalkProviderResourcesMaintenance(ctx, func(resource domain.ProviderResource) error {
			if resource.ObjectPath == "" || filepath.Base(resource.ObjectPath) != resource.ObjectPath {
				return nil
			}
			path := filepath.Join(directory, resource.ObjectPath)
			header, observed, err := inspectObjectHeader(path)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					r.logger.Error("provider object could not be read", "resource_id", resource.ID, "error", err)
				}
				if dirscan.Missing(err) {
					return ctx.Err()
				}
				return errors.Join(err, ctx.Err())
			}
			if vault.SealedEnvelope(header) {
				return nil
			}
			if err := r.store.DeleteProviderResourceIfUnchanged(ctx, resource, func() error { return r.retireUnsealedProviderObject(ctx, directory, resource.ObjectPath, observed) }); err != nil {
				if errors.Is(err, boltstore.ErrNotFound) || errors.Is(err, boltstore.ErrRevisionConflict) {
					return ctx.Err()
				}
				return err
			}
			reclaimed++
			return nil
		}); err != nil {
			return err
		}
		removed, err := r.sweepProviderObjectsWithMode(ctx, references, true)
		reclaimed += removed
		return err
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		r.logger.Error("provider object reclamation refused or interrupted", "error", err)
	}
	if reclaimed > 0 {
		r.logger.Warn("reclaimed provider objects written before sealing", "count", reclaimed)
	}
}

// readObjectHeader reads only as much as SealedEnvelope inspects. A batch result
// can be large and nothing here needs its contents.
func readObjectHeader(path string) ([]byte, error) {
	header, _, err := inspectObjectHeader(path)
	return header, err
}

func inspectObjectHeader(path string) (header []byte, identity os.FileInfo, resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, dirscan.ErrIdentity
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errProviderObjectRetirement, closeErr)
		}
		if resultErr != nil {
			header = nil
			identity = nil
		}
	}()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || !actual.Mode().IsRegular() {
		return nil, nil, errors.Join(dirscan.ErrIdentity, err)
	}
	header = make([]byte, 6)
	n, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	named, err := os.Lstat(path)
	final, finalErr := file.Stat()
	if err != nil || finalErr != nil || !os.SameFile(info, named) || !os.SameFile(info, final) || !info.ModTime().Equal(named.ModTime()) || info.Size() != named.Size() || final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) {
		return nil, nil, errors.Join(dirscan.ErrIdentity, err, finalErr)
	}
	return header[:n], info, nil
}
