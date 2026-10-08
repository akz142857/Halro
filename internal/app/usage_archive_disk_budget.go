package app

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// runtimeUsageArchiveDiskBudget keeps startup scratch in the independent
// Recovery pool until the selected Runtime role completes its startup gate.
// Background operations then charge the normal IO class and fail closed when
// its measured disk envelope or current pressure state does not admit them.
type runtimeUsageArchiveDiskBudget struct {
	manager *resourcebudget.Manager
	startup atomic.Bool
}

func newRuntimeUsageArchiveDiskBudget(cfg config.Config, pressure *resourcePressureRuntime) *runtimeUsageArchiveDiskBudget {
	if cfg.ResourceBudget == nil {
		return nil
	}
	b := &runtimeUsageArchiveDiskBudget{manager: pressure.logicalBudget()}
	b.startup.Store(true)
	return b
}

// One-shot maintenance runs under the data-directory lock, so its private
// Manager owns the archive ceiling and every transient claim for that command
// lifetime. It remains in Recovery because there is no long-lived Runtime role
// transition or pressure-monitor owner to publish Background work.
func newOneShotUsageArchiveDiskBudget(ctx context.Context, cfg config.Config) (*runtimeUsageArchiveDiskBudget, func() error, error) {
	return newOneShotUsageArchiveDiskBudgetWithPressure(ctx, cfg, nil)
}

func newOneShotUsageArchiveDiskBudgetWithPressure(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*runtimeUsageArchiveDiskBudget, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var scopeErr error
	pressure, scopeErr = commandPressure(ctx, cfg, pressure)
	if scopeErr != nil {
		return nil, nil, scopeErr
	}
	if cfg.ResourceBudget == nil {
		return nil, func() error { return nil }, nil
	}
	if pressure == nil {
		var err error
		pressure, err = newResourcePressure(cfg)
		if err != nil {
			return nil, nil, err
		}
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return nil, nil, errors.New("one-shot Usage archive ceiling requires the configured shared budget")
	}
	var permit resourcebudget.Permit
	var err error
	releasePersistent := func() error { return nil }
	if cfg.ResourceBudget.UsageArchiveDiskBytes > 0 {
		permit, err = manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.UsageArchiveDiskBytes})
		if err != nil {
			return nil, nil, err
		}
		releasePersistent = permit.Release
	}
	budget := &runtimeUsageArchiveDiskBudget{manager: manager}
	budget.startup.Store(true)
	return budget, releasePersistent, nil
}

func (b *runtimeUsageArchiveDiskBudget) reserve(ctx context.Context, amount int64) (func() error, error) {
	if b == nil || b.manager == nil || amount <= 0 {
		return nil, resourcebudget.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var permit resourcebudget.Permit
	var err error
	startup := b.startup.Load()
	if startup {
		permit, err = b.manager.TryAcquireRecovery(resourcebudget.Cost{Disk: amount})
	} else {
		permit, err = b.manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{Disk: amount})
	}
	if err != nil {
		return nil, fmt.Errorf("reserve Usage archive Disk=%d startup=%t: %w", amount, startup, err)
	}
	return permit.Release, nil
}

func (b *runtimeUsageArchiveDiskBudget) reserveRAM(ctx context.Context, amount int64) (func() error, error) {
	if b == nil || b.manager == nil || amount <= 0 {
		return nil, resourcebudget.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var permit resourcebudget.Permit
	var err error
	startup := b.startup.Load()
	if startup {
		permit, err = b.manager.TryAcquireRecovery(resourcebudget.Cost{RAM: amount})
	} else {
		permit, err = b.manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: amount})
	}
	if err != nil {
		return nil, fmt.Errorf("reserve Usage archive RAM=%d startup=%t: %w", amount, startup, err)
	}
	return permit.Release, nil
}

func (b *runtimeUsageArchiveDiskBudget) readerReservation(cfg config.Config) func(context.Context) (func() error, error) {
	if b == nil || cfg.ResourceBudget == nil || cfg.ResourceBudget.UsageArchiveReadRAMBytes == 0 {
		return nil
	}
	return func(ctx context.Context) (func() error, error) {
		return b.reserveRAM(ctx, cfg.ResourceBudget.UsageArchiveReadRAMBytes)
	}
}

func (b *runtimeUsageArchiveDiskBudget) workReservation() func(context.Context, int64) (func() error, error) {
	if b == nil {
		return nil
	}
	return b.reserveRAM
}

func (b *runtimeUsageArchiveDiskBudget) activateIO() {
	if b != nil {
		b.startup.Store(false)
	}
}
