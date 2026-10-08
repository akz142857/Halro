package app

import "github.com/akz142857/Halro/internal/config"

// Published maintenance consumers pin the same original Runtime manager.
// Fixed component owners carry their own policy, not per-file registries.
type runtimeMaintenanceOwners struct {
	provider *providerMaintenanceOwner
	ledger   *ledgerMaintenanceOwner
}

func newRuntimeMaintenanceOwners(cfg config.Config, pressure *resourcePressureRuntime) *runtimeMaintenanceOwners {
	return &runtimeMaintenanceOwners{provider: newProviderMaintenanceOwner(cfg, pressure), ledger: newLedgerMaintenanceOwner(cfg, pressure)}
}
