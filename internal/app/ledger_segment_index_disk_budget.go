package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/config"
)

// A one-shot Ledger maintenance command owns the configured aggregate index
// ceiling before opening the native log. The claim lasts until the Log closes;
// unlike Runtime it has no live owner after the command exits.
func newOneShotLedgerSegmentIndexDiskOwner(ctx context.Context, cfg config.Config) (*runtimeProjectionDiskPermit, int64, error) {
	return newOneShotLedgerSegmentIndexDiskOwnerWithPressure(ctx, cfg, nil)
}

func newOneShotLedgerSegmentIndexDiskOwnerWithPressure(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeProjectionDiskPermit, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	var scopeErr error
	pressure, scopeErr = commandPressure(ctx, cfg, pressure)
	if scopeErr != nil {
		return nil, 0, scopeErr
	}
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.LedgerSegmentIndexDiskBytes == 0 {
		return nil, 0, nil
	}
	if pressure == nil {
		var err error
		pressure, err = newResourcePressure(cfg)
		if err != nil {
			return nil, 0, err
		}
	}
	owner, limit, err := acquireLedgerSegmentIndexDiskFromManager(cfg, pressure.logicalBudget())
	if err != nil {
		return nil, 0, err
	}
	if owner == nil {
		return nil, 0, errors.New("one-shot Ledger segment index ceiling has no disk owner")
	}
	return owner, limit, nil
}
