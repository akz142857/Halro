package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func snapshotSelectedBackupMetadata(ctx context.Context, cfg config.Config, store *boltstore.Store, path string, owner *backupStagingDiskOwner) (result boltstore.MetadataInfo, resultErr error) {
	scope, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if owner == nil || owner.metadataImage != nil || scope == nil || scope.selected == nil || cfg.Replication != nil || !scope.selected.guard.CoversDataDirectory(cfg.Storage.DataDir) || !store.AuthenticatedMetadata() {
		return result, boltstore.ErrFacadeMaintenance
	}
	source := scope.selected.state.generation.Owners.Metadata
	reference, err := source.SealReference(ctx)
	if err != nil {
		return result, err
	}
	limit := metadataLegacyFileCeiling(cfg)
	// The complete image is a distinct owner until its actual inode is retired.
	credit, err := scope.pressure.logicalBudget().TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{Disk: limit})
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, credit.Release()) }()
	owner.metadataImage, err = source.MaterializeMetadata(ctx, path, reference, limit, credit)
	if err != nil {
		return result, err
	}
	return owner.metadataImage.SnapshotInfo(ctx)
}
