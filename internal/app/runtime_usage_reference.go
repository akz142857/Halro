package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// The query window can be trimmed; date verification/reclamation must instead
// reconstruct complete history. Each job owns two fresh private disk reducers,
// independent of query rows/checkpoints, and retires them before returning.
// Native scans temporarily freeze physical appends, but never transfer semantic
// maintenance ownership or retire the business accounting Manager.
func (r *Runtime) withUsageMaintenanceReference(ctx context.Context, work func(context.Context, *usage.DiskProjection, *usage.Exporter) error) (err error) {
	if work == nil || r.usage == nil || r.usage.owner == nil {
		return errors.New("Usage maintenance requires a selected disk owner")
	}
	o := r.usage.owner
	if !o.maintenanceMu.TryLock() {
		return usage.ErrDiskUsageBusy
	}
	defer o.maintenanceMu.Unlock()
	if o.maintenanceRetired.Load() || o.maintenanceCtx == nil || o.native != r.ledger || r.usage.disk != o.disk || r.usageExporter != o.archive || o.archive == nil {
		return errors.New("Usage maintenance requires a live Standalone native and archive owner")
	}
	member := r.config.Replication != nil || r.replication != nil
	if member && o.checkRole == nil {
		return errors.New("Usage maintenance requires authenticated Primary ownership")
	}
	if o.checkRole != nil {
		if err := o.checkRole(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	stop := context.AfterFunc(o.maintenanceCtx, cancel)
	defer stop()
	// Observe cancellation after physical DB/scratch cleanup, not just after
	// the last archive callback. Close waits for this whole operation.
	defer func() {
		err = errors.Join(err, ctx.Err())
		if err != nil {
			o.archive.RevokeWindowAuthority()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return err
	}
	dataDir, err := filepath.EvalSymlinks(r.config.Storage.DataDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(dataDir, parent)
	if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return errors.Join(err, errors.New("Usage maintenance scratch must be outside native data"))
	}
	referenceBudget, err := acquireUsageReferenceBudget(ctx, r.config, o.resourcePressure, resourcebudget.IO)
	if err != nil {
		return err
	}
	var cleanupErr error
	defer func() { err = errors.Join(err, referenceBudget.retire(withRetirementDebt(cleanupErr, err))) }()
	directory, err := os.MkdirTemp(parent, "halro-usage-maintenance-")
	if err != nil {
		return err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return err
	}
	defer func() {
		actual, statErr := os.Lstat(directory)
		if statErr != nil || !os.SameFile(identity, actual) {
			cleanupErr = errors.Join(cleanupErr, statErr, errors.New("Usage maintenance scratch identity changed"))
			err = errors.Join(err, cleanupErr)
			return
		}
		cleanupErr = errors.Join(cleanupErr, os.Remove(directory))
		err = errors.Join(err, cleanupErr)
	}()
	var accounting *ledger.LedgerProjection
	if member {
		cut, _, ok := o.native.ChainHead()
		if !ok {
			return usage.ErrArchiveDateProof
		}
		if referenceBudget.ledgerBytes > 0 {
			accounting, _, err = o.native.BuildProjectionFromCursorWithLimit(ctx, cut.Generation, cut.Sequence, filepath.Join(directory, "ledger.db"), referenceBudget.ledgerBytes)
		} else {
			accounting, _, err = o.native.BuildProjectionFromCursor(ctx, cut.Generation, cut.Sequence, filepath.Join(directory, "ledger.db"))
		}
	} else {
		if referenceBudget.ledgerBytes > 0 {
			cut, _, ok := o.native.ChainHead()
			if !ok {
				return usage.ErrArchiveDateProof
			}
			accounting, _, err = o.native.BuildProjectionFromCursorWithLimit(ctx, cut.Generation, cut.Sequence, filepath.Join(directory, "ledger.db"), referenceBudget.ledgerBytes)
		} else {
			accounting, err = o.native.BuildFreshProjection(ctx, filepath.Join(directory, "ledger.db"))
		}
	}
	if err != nil {
		return err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, accounting.Discard()); err = errors.Join(err, cleanupErr) }()
	head, hash, err := accounting.ReadPosition(ctx)
	if err != nil {
		return err
	}
	derivative, disk, err := usage.NewDiskLedgerDerivativeWithLimit(accounting, filepath.Join(directory, "usage.db"), referenceBudget.usageBytes)
	if err != nil {
		return err
	}
	defer func() {
		derivative.Close()
		cleanupErr = errors.Join(cleanupErr, disk.Discard())
		err = errors.Join(err, cleanupErr)
	}()
	// A concurrently appended suffix is authenticated but excluded from this
	// fixed private cut. The independent Ledger owner resolves every historical
	// finalization, so live query pruning cannot change these rows or totals.
	if _, err := replayDiskUsageBatches(ctx, o.native, ledger.ReplayAnchor{}, derivative, &head); err != nil {
		return err
	}
	anchor, err := disk.ReadReplayAnchor(ctx)
	if err != nil || anchor.Head.Sequence != head.Sequence || anchor.Hash != hash {
		return errors.Join(err, usage.ErrCollectorHead)
	}
	if o.checkRole != nil {
		if err := o.checkRole(); err != nil {
			return err
		}
	}
	if err := work(ctx, disk, o.archive); err != nil {
		return err
	}
	prefix, _, err := ledger.InspectProjectionCursor(ctx, r.config.LedgerPath(), o.nativeKey[:], head.Generation, head.Sequence)
	if err != nil || prefix.Head != head || prefix.Hash != hash {
		return errors.Join(err, usage.ErrArchiveDateProof)
	}
	_, err = o.native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, nil)
	if o.checkRole != nil {
		err = errors.Join(err, o.checkRole())
	}
	return err
}
