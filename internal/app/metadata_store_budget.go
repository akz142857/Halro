package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func metadataStoreDiskCeiling(cfg config.Config) int64 {
	if cfg.ResourceBudget == nil {
		return 0
	}
	return cfg.ResourceBudget.MetadataStoreDiskBytes
}

func metadataLegacyFileCeiling(cfg config.Config) int64 {
	if cfg.ResourceBudget == nil {
		return 0
	}
	return cfg.ResourceBudget.LegacyMetadataFileCeiling()
}

// This claim covers the persistent bbolt DATA FILE and a conservative full-file
// mapping/freelist RAM estimate, not native journal buffers, temporary copies or
// physical blocks. Actual Store.Close retires RAM; the file's Disk claim remains.
func acquireMetadataStoreDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := metadataStoreDiskCeiling(cfg)
	if limit == 0 {
		return nil, nil
	}
	manager := pressure.logicalBudget()
	if limit < 0 || manager == nil {
		return nil, errors.New("Metadata data-file ceiling requires its configured shared disk budget")
	}
	ram, err := metadatalimits.MappingRAMBytes(metadataLegacyFileCeiling(cfg))
	if err != nil {
		return nil, err
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ram, Disk: limit})
	if err != nil {
		return nil, err
	}
	return &runtimeProjectionDiskPermit{permit: permit, retirableRAM: ram}, nil
}

func (r *Runtime) closeMetadataStore() error {
	return r.metadataStoreDiskPermit.retireRAMAfterClose(r.store.Close())
}
