package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// selectedBackupMetadataPhaseKey carries a test-only phase hook; production
// contexts never hold one. "materialize" runs before the image credit is
// taken, "materialize-done" after every fee of this step was returned.
type selectedBackupMetadataPhaseKey struct{}
type selectedBackupMetadataPhase func(phase string, budget *resourcebudget.Manager, err error) error

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
	if phase, _ := ctx.Value(selectedBackupMetadataPhaseKey{}).(selectedBackupMetadataPhase); phase != nil {
		if err := phase("materialize", scope.pressure.logicalBudget(), nil); err != nil {
			return result, err
		}
		defer func() {
			resultErr = errors.Join(resultErr, phase("materialize-done", scope.pressure.logicalBudget(), resultErr))
		}()
	}
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
