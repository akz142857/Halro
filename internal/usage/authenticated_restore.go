package usage

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

// RestoreDiskLedgerDerivative binds the restored owner to the same live-event
// bridge used during fresh startup, preserving derivative-before-Ledger-Apply
// ordering. Retire the returned derivative before closing/discarding its DB.
func RestoreDiskLedgerDerivative(ctx context.Context, source *AuthenticatedUsageProjection, owner *ledger.AuthenticatedLedgerProjection, path string, maxBytes int64, reservation resourcebudget.Permit, budget *resourcebudget.Manager) (*LedgerDerivative, *DiskProjection, error) {
	p, err := source.RestoreDiskProjection(ctx, path, owner, maxBytes, reservation, budget)
	if err != nil {
		return nil, p, err
	}
	d := &LedgerDerivative{owner: owner, disk: p}
	p.reader = d
	return d, p, nil
}

// RestoreDiskProjection materializes the selected, authenticated image into a
// fresh writable owner. owner must remain at the exact independently selected
// Ledger cut through publication; no ordinary saved bbolt file is trusted.
// Close joins live readers; Discard also removes this fresh inode and releases
// its retained disk reservation. Runtime role/common-cut selection is external.
func (a *AuthenticatedUsageProjection) RestoreDiskProjection(ctx context.Context, path string, owner *ledger.AuthenticatedLedgerProjection, maxBytes int64, reservation resourcebudget.Permit, budget *resourcebudget.Manager) (result *DiskProjection, resultErr error) {
	if a == nil || a.store == nil || owner == nil || budget == nil || maxBytes <= 0 || uint64(maxBytes) > uint64(^uint(0)>>1)-uint64(usageSealWorkspaceRAMBytes) {
		return nil, authenticatedstate.ErrRecovery
	}
	workspace, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes + maxBytes})
	if err != nil {
		return nil, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, workspace.Release())
		}
	}()
	ref, err := owner.SealReference(ctx)
	if err != nil || ref != a.source {
		return nil, errors.Join(authenticatedstate.ErrRecovery, err)
	}
	// RetainCovering rejects a reservation from a different manager before IO.
	credit, err := reservation.RetainCovering(budget, resourcebudget.Cost{Disk: maxBytes})
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, credit.Release()) }()
	p := &DiskProjection{reader: owner, readers: make(chan struct{}, 2), summaries: make(chan struct{}, 1)}
	defer func() {
		if result == nil {
			if cleanupErr := p.Discard(); cleanupErr != nil {
				result, resultErr = p, errors.Join(resultErr, cleanupErr)
			}
		}
	}()
	p.image, err = a.store.MaterializeImage(ctx, path, a.reference.Schema, a.reference.Root, usageSealChunkBytes, maxBytes, credit)
	if err != nil {
		return nil, err
	}
	p.db, err = bbolt.Open(path, 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType, MaxSize: int(maxBytes), OpenFile: p.image.OpenFile})
	if err != nil {
		return nil, p.image.EngineOpenFailed(normalizeDiskUsageDiskError(err))
	}
	p.engine = wrapUsageDatabase(p.db)
	p.liveRAM, err = workspace.Retain()
	if err != nil {
		return nil, err
	}
	if _, err := rand.Read(p.generation[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(p.cursorKey[:]); err != nil {
		return nil, err
	}
	if err := p.view(ctx, func(tx *bbolt.Tx) error {
		for _, name := range diskUsageBuckets {
			if tx.Bucket([]byte(name)) == nil {
				return ErrDiskUsageCorrupt
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	anchor, err := p.ReadReplayAnchor(ctx)
	if err != nil || anchor != ref.Anchor {
		return nil, errors.Join(ErrDiskUsageCorrupt, err)
	}
	final, err := owner.SealReference(ctx)
	if err != nil || final != ref {
		return nil, errors.Join(authenticatedstate.ErrRecovery, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p, nil
}
