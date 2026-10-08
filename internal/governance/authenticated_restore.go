package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

// RestoreProjection restores every byte of the selected image into a fresh
// writable owner only after reauthenticating its exact native journal prefix.
// The caller holds directory exclusion and supplies role/common-cut authority.
// Close joins transactions; Discard also removes the created inode and retires
// the retained disk credit. No saved ordinary projection is ever opened.
func (a *AuthenticatedGovernanceProjection) RestoreProjection(ctx context.Context, path string, native *Log, maxBytes int64, reservation resourcebudget.Permit, budget *resourcebudget.Manager) (result *GovernanceProjection, resultErr error) {
	if a == nil || a.store == nil || native == nil || budget == nil || maxBytes <= 0 || uint64(maxBytes) > uint64(^uint(0)>>1)-uint64(governanceSealWorkspaceRAMBytes) || a.Journal.Position.Offset > uint64(^uint64(0)>>1) {
		return nil, authenticatedstate.ErrRecovery
	}
	workspace, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: governanceSealWorkspaceRAMBytes + maxBytes})
	if err != nil {
		return nil, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, workspace.Release())
		}
	}()
	cut := Summary{Records: a.Journal.Position.Sequence, Bytes: int64(a.Journal.Position.Offset), LastHash: a.Journal.Position.Hash}
	ref, err := native.SealReference(ctx, budget, cut)
	if err != nil || ref != a.Journal {
		return nil, errors.Join(ErrGovernanceSealSource, err)
	}
	credit, err := reservation.RetainCovering(budget, resourcebudget.Cost{Disk: maxBytes})
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, credit.Release()) }()
	p := &GovernanceProjection{}
	defer func() {
		if result == nil {
			if cleanupErr := p.Discard(); cleanupErr != nil {
				result, resultErr = p, errors.Join(resultErr, cleanupErr)
			}
		}
	}()
	p.image, err = a.store.MaterializeImage(ctx, path, a.Projection.Schema, a.Projection.Root, governanceSealChunkBytes, maxBytes, credit)
	if err != nil {
		return nil, err
	}
	p.db, err = bbolt.Open(path, 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType, MaxSize: int(maxBytes), OpenFile: p.image.OpenFile})
	if err != nil {
		return nil, p.image.EngineOpenFailed(projectionCapacityError(err))
	}
	p.liveRAM, err = workspace.Retain()
	if err != nil {
		return nil, err
	}
	p.ready = true
	if err := p.read(ctx, func(tx *bbolt.Tx, head Summary) error {
		if head != cut {
			return ErrCorrupt
		}
		for _, name := range governanceProjectionBuckets {
			if tx.Bucket([]byte(name)) == nil {
				return ErrCorrupt
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p, nil
}
