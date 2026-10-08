package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

const commandMetadataFileSlots = 8
const commandMetadataFileOwnerRAMBytes int64 = 8 << 10

type commandMetadataFileOwner struct {
	path   string
	permit resourcebudget.Permit
}

// A command has a fixed set of source/stage files, never a historical registry.
// A full ceiling is retained per distinct path throughout this job, including
// closed files and failed publications. This conservatively overcharges removed
// scratch; it does not authorise inode deletion or establish a durable inventory.
func reserveCommandMetadataFile(ctx context.Context, cfg config.Config, path string) (*resourcebudget.Manager, error) {
	_, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return nil, err
	}
	limit := metadataStoreDiskCeiling(cfg)
	if limit == 0 {
		return pressure.logicalBudget(), nil
	}
	scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if !ok {
		return nil, errors.New("Metadata command must retain its root shared resource context")
	}
	abs, err := filepath.Abs(path)
	if err != nil || len(abs) > 4096 {
		return nil, errors.Join(err, errors.New("Metadata command path exceeds bounded owner input"))
	}
	scope.metadataMu.Lock()
	defer scope.metadataMu.Unlock()
	for _, slot := range scope.metadataFiles {
		if slot.path == abs {
			return pressure.logicalBudget(), nil
		}
	}
	for i := range scope.metadataFiles {
		if scope.metadataFiles[i].path != "" {
			continue
		}
		permit, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: commandMetadataFileOwnerRAMBytes, Disk: limit})
		if err != nil {
			return nil, err
		}
		scope.metadataFiles[i] = commandMetadataFileOwner{path: abs, permit: permit}
		return pressure.logicalBudget(), nil
	}
	return nil, errors.New("Metadata command exceeded its fixed physical-file owner slots")
}

func openCommandMetadata(ctx context.Context, cfg config.Config, path string, open func(context.Context, string, int64, *resourcebudget.Manager) (*boltstore.Store, error)) (*boltstore.Store, error) {
	if active, err := memoryRuntimeActivated(cfg); err != nil {
		return nil, err
	} else if active {
		return nil, errors.New("this command requires the selected memory-state backend; refusing to open the retired legacy Metadata database")
	}
	manager, err := reserveCommandMetadataFile(ctx, cfg, path)
	if err != nil {
		return nil, err
	}
	scope := keyScope(ctx)
	if scope != nil {
		if err := scope.captureSource(cfg); err != nil {
			return nil, err
		}
		if scope.expected(path) == nil {
			return nil, errors.New("key maintenance Metadata path has no original physical identity")
		}
	}
	store, err := open(ctx, path, metadataLegacyFileCeiling(cfg), manager)
	if scope == nil {
		return store, err
	}
	if errors.Is(err, boltstore.ErrMetadataFileRetirement) || errors.Is(err, ownedfile.ErrRetirement) {
		scope.debt = errors.Join(scope.debt, err)
	}
	if err != nil {
		return nil, err
	}
	if err := scope.trackCloser(store); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if err := store.VerifyPhysicalIdentity(scope.expected(path)); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

func openCommandMetadataStore(ctx context.Context, cfg config.Config, path string) (*boltstore.Store, error) {
	if active, err := memoryRuntimeActivated(cfg); err != nil {
		return nil, err
	} else if active {
		return openSelectedCommandMetadata(ctx, cfg, path)
	}
	return openCommandMetadata(ctx, cfg, path, boltstore.OpenWithMappingBudget)
}

func openCommandMetadataReadOnly(ctx context.Context, cfg config.Config, path string) (*boltstore.Store, error) {
	if active, err := memoryRuntimeActivated(cfg); err != nil {
		return nil, err
	} else if active {
		return openSelectedCommandMetadataReadOnly(ctx, cfg, path)
	}
	return openCommandMetadata(ctx, cfg, path, boltstore.OpenReadOnlyWithMappingBudget)
}

func openCommandMetadataPrimary(ctx context.Context, cfg config.Config, path string) (*boltstore.Store, error) {
	return openCommandMetadata(ctx, cfg, path, boltstore.OpenPrimaryWithMappingBudget)
}

func openCommandMetadataReplica(ctx context.Context, cfg config.Config, path string) (*boltstore.Store, error) {
	return openCommandMetadata(ctx, cfg, path, boltstore.OpenReplicaWithMappingBudget)
}

func openCommandMetadataPublish(ctx context.Context, cfg config.Config, path string) (*boltstore.Store, error) {
	return openCommandMetadata(ctx, cfg, path, boltstore.OpenForWholeFilePublishWithMappingBudget)
}

func openCommandMetadataDerivedMaintenance(ctx context.Context, cfg config.Config, path string, expected os.FileInfo) (*boltstore.Store, error) {
	manager, err := reserveCommandMetadataFile(ctx, cfg, path)
	if err != nil {
		return nil, err
	}
	return boltstore.OpenExistingDerivedMaintenanceWithMappingBudget(ctx, path, expected, metadataLegacyFileCeiling(cfg), manager)
}

func snapshotCommandMetadata(ctx context.Context, cfg config.Config, store *boltstore.Store, path string) (boltstore.MetadataInfo, error) {
	if _, err := reserveCommandMetadataFile(ctx, cfg, path); err != nil {
		return boltstore.MetadataInfo{}, err
	}
	if scope := keyScope(ctx); scope != nil {
		info, owner, err := store.SnapshotCreateOwnedContext(ctx, path)
		err = scope.trackStage(path, owner, err)
		if err != nil {
			return boltstore.MetadataInfo{}, err
		}
		return info, nil
	}
	return store.SnapshotContext(ctx, path)
}

func compactCommandMetadata(ctx context.Context, cfg config.Config, store *boltstore.Store, path string) error {
	if _, err := reserveCommandMetadataFile(ctx, cfg, path); err != nil {
		return err
	}
	if scope := keyScope(ctx); scope != nil {
		owner, err := store.CompactCreateOwnedContext(ctx, path)
		return scope.trackStage(path, owner, err)
	}
	return store.CompactSnapshotContext(ctx, path)
}
