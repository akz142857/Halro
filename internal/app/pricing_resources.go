package app

import (
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func configurePricingResources(cfg config.Config, manager *resourcebudget.Manager, metadata *boltstore.Store) error {
	if manager == nil {
		return nil
	}
	return metadata.ConfigurePricingResources(manager, cfg.ResourceBudget.PricingLifetimeRAMBytes, cfg.ResourceBudget.PricingIORAMBytes)
}
