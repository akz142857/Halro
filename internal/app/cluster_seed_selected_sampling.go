package app

import (
	"context"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// A target directory is absent until atomic publication. Sample its existing
// ancestor and the actual temp filesystem, retaining the original policy and
// manager. This supplies OS evidence, never selected-state or seed authority.
func seedBudgetContext(ctx context.Context, cfg config.Config) (context.Context, *resourcePressureRuntime, error) {
	if ctx == nil || ctx.Err() != nil || cfg.ResourceBudget == nil {
		return commandBudgetContext(ctx, cfg)
	}
	if _, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok {
		return commandBudgetContext(ctx, cfg)
	}
	directory, err := existingOperationDirectory(cfg.Storage.DataDir)
	if err != nil {
		return ctx, nil, err
	}
	sampler, err := resourcebudget.NewSampler(directory)
	if err != nil {
		return ctx, nil, err
	}
	pressure, err := newResourcePressureWithSource(cfg, sampler)
	if err != nil {
		return ctx, nil, err
	}
	scope := &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure}
	return context.WithValue(ctx, commandResourceScopeKey{}, scope), pressure, nil
}

func withSelectedSeedSampling(ctx context.Context, cfg config.Config, visit func(context.Context) error) error {
	pressure, err := commandPressure(ctx, cfg, nil)
	if err != nil {
		return err
	}
	if pressure == nil || pressure.logicalBudget() == nil {
		return resourcebudget.ErrInvalid
	}
	if pressure.monitor != nil {
		return pressure.monitor.WithSampling(ctx, visit)
	}
	// An already supplied command scope may own its own sampling lifetime.
	// Every actual manager claim still enforces its original pressure evidence.
	return visit(ctx)
}
