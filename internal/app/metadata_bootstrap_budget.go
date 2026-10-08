package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/config"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

type runtimeMetadataBootstrapKey struct{}

type runtimeMetadataBootstrap struct {
	pressure *resourcePressureRuntime
	owner    *runtimeProjectionDiskPermit
	budget   config.ResourceBudget
	policy   config.ResourcePressure
	path     string
}

// The exact Runtime owner covers the sequential key-slot inspection BEFORE its
// main database opens. A command gets an independent handle fee from its root
// scope. Neither path can silently create a substitute manager.
func openMasterKeyMetadata(ctx context.Context, cfg config.Config, readOnly bool) (*boltstore.Store, error) {
	if bootstrap, ok := ctx.Value(runtimeMetadataBootstrapKey{}).(runtimeMetadataBootstrap); ok && cfg.ResourceBudget != nil {
		if active, err := memoryRuntimeActivated(cfg); err != nil {
			return nil, err
		} else if active {
			readOnly = true
		}
		if bootstrap.budget != *cfg.ResourceBudget || cfg.ResourcePressure == nil || bootstrap.policy != *cfg.ResourcePressure || bootstrap.path != cfg.MetadataPath() {
			return nil, errors.New("Runtime key-slot inspection changed its shared Metadata budget")
		}
		bootstrap.owner.mu.Lock()
		defer bootstrap.owner.mu.Unlock()
		if bootstrap.owner.released || bootstrap.owner.retirableRAM == 0 {
			return nil, errors.New("Runtime key-slot inspection has no live Metadata mapping reservation")
		}
		if readOnly {
			return boltstore.OpenReadOnlyWithMappingReservation(ctx, cfg.MetadataPath(), metadataLegacyFileCeiling(cfg), bootstrap.pressure.logicalBudget(), bootstrap.owner.permit)
		}
		return boltstore.OpenWithMappingReservation(ctx, cfg.MetadataPath(), metadataLegacyFileCeiling(cfg), bootstrap.pressure.logicalBudget(), bootstrap.owner.permit)
	}
	ctx, _, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if readOnly {
		return openCommandMetadataReadOnly(ctx, cfg, cfg.MetadataPath())
	}
	return openCommandMetadataStore(ctx, cfg, cfg.MetadataPath())
}
