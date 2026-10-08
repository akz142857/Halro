package bolt

import (
	"context"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// These named constructors charge mapping/freelist capacity before any file
// open, migration or mmap. Persistent file Disk ownership belongs to the caller;
// a read-only handle also requires its own mapping fee while physically alive.
func openWithMappingBudget(ctx context.Context, maxBytes int64, budget *resourcebudget.Manager, open func() (*Store, error)) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("Metadata mapping budget requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ram, err := metadatalimits.MappingRAMBytes(maxBytes)
	if err != nil {
		return nil, err
	}
	if ram == 0 {
		return open()
	}
	if budget == nil {
		return nil, errors.New("bounded Metadata mapping requires its shared manager")
	}
	permit, err := budget.TryAcquireRecovery(resourcebudget.Cost{RAM: ram})
	if err != nil {
		return nil, err
	}
	store, err := open()
	if err != nil {
		if errors.Is(err, ErrMetadataFileRetirement) || errors.Is(err, ownedfile.ErrRetirement) {
			return nil, err
		}
		return nil, errors.Join(err, permit.Release())
	}
	store.mappingPermit = permit
	store.mappingBudget = budget
	return store, nil
}

func OpenWithMappingBudget(ctx context.Context, path string, maxBytes int64, budget *resourcebudget.Manager) (*Store, error) {
	return openWithMappingBudget(ctx, maxBytes, budget, func() (*Store, error) { return OpenWithDiskLimit(path, maxBytes) })
}

func OpenPrimaryWithMappingBudget(ctx context.Context, path string, maxBytes int64, budget *resourcebudget.Manager) (*Store, error) {
	return openWithMappingBudget(ctx, maxBytes, budget, func() (*Store, error) { return OpenPrimaryWithDiskLimit(path, maxBytes) })
}

func OpenReplicaWithMappingBudget(ctx context.Context, path string, maxBytes int64, budget *resourcebudget.Manager) (*Store, error) {
	return openWithMappingBudget(ctx, maxBytes, budget, func() (*Store, error) { return OpenReplicaWithDiskLimit(path, maxBytes) })
}

func OpenReadOnlyWithMappingBudget(ctx context.Context, path string, maxBytes int64, budget *resourcebudget.Manager) (*Store, error) {
	return openWithMappingBudget(ctx, maxBytes, budget, func() (*Store, error) { return OpenReadOnlyWithDiskLimit(path, maxBytes) })
}

func OpenForWholeFilePublishWithMappingBudget(ctx context.Context, path string, maxBytes int64, budget *resourcebudget.Manager) (*Store, error) {
	return openWithMappingBudget(ctx, maxBytes, budget, func() (*Store, error) { return OpenForWholeFilePublishWithDiskLimit(path, maxBytes) })
}

func OpenExistingDerivedMaintenanceWithMappingBudget(ctx context.Context, path string, expected os.FileInfo, maxBytes int64, budget *resourcebudget.Manager) (*Store, error) {
	return openWithMappingBudget(ctx, maxBytes, budget, func() (*Store, error) { return OpenExistingDerivedMaintenanceWithDiskLimit(path, expected, maxBytes) })
}

func openWithMappingReservation(ctx context.Context, maxBytes int64, budget *resourcebudget.Manager, credit resourcebudget.Permit, open func() (*Store, error)) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("Metadata mapping reservation requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ram, err := metadatalimits.MappingRAMBytes(maxBytes)
	if err != nil || ram == 0 {
		return nil, errors.Join(err, errors.New("Metadata mapping reservation requires a positive file ceiling"))
	}
	permit, err := credit.RetainCovering(budget, resourcebudget.Cost{RAM: ram})
	if err != nil {
		return nil, err
	}
	store, err := open()
	if err != nil {
		if errors.Is(err, ErrMetadataFileRetirement) || errors.Is(err, ownedfile.ErrRetirement) {
			return nil, err
		}
		return nil, errors.Join(err, permit.Release())
	}
	store.mappingPermit = permit
	store.mappingBudget = budget
	return store, nil
}

func acquireMetadataMapping(budget *resourcebudget.Manager, maxBytes int64) (resourcebudget.Permit, error) {
	ram, err := metadatalimits.MappingRAMBytes(maxBytes)
	if err != nil {
		return resourcebudget.Permit{}, err
	}
	return budget.TryAcquireRecovery(resourcebudget.Cost{RAM: ram})
}

func releaseMetadataMapping(permit resourcebudget.Permit) error {
	if permit == (resourcebudget.Permit{}) {
		return nil
	}
	return permit.Release()
}

func OpenWithMappingReservation(ctx context.Context, path string, maxBytes int64, budget *resourcebudget.Manager, credit resourcebudget.Permit) (*Store, error) {
	return openWithMappingReservation(ctx, maxBytes, budget, credit, func() (*Store, error) { return OpenWithDiskLimit(path, maxBytes) })
}

func OpenReadOnlyWithMappingReservation(ctx context.Context, path string, maxBytes int64, budget *resourcebudget.Manager, credit resourcebudget.Permit) (*Store, error) {
	return openWithMappingReservation(ctx, maxBytes, budget, credit, func() (*Store, error) { return OpenReadOnlyWithDiskLimit(path, maxBytes) })
}
