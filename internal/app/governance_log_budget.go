package app

import (
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func governanceLogOptions(cfg config.Config, manager *resourcebudget.Manager, options governance.Options) governance.Options {
	if cfg.ResourceBudget != nil {
		options.MaxDiskBytes = cfg.ResourceBudget.GovernanceLogDiskBytes
	}
	if manager != nil {
		options.CacheBudget = manager
		options.CacheRAMBytes = cfg.ResourceBudget.GovernanceLogRAMBytes
	}
	return options
}
