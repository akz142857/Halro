package app

import (
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func auditLogOptions(cfg config.Config, options audit.Options) audit.Options {
	if cfg.ResourceBudget != nil {
		options.MaxDiskBytes = cfg.ResourceBudget.AuditLogDiskBytes
	}
	return options
}

func openConfiguredAuditLog(cfg config.Config, path string, key []byte, options audit.Options, managers ...*resourcebudget.Manager) (*audit.Log, error) {
	if len(managers) > 0 && managers[0] != nil && cfg.ResourceBudget != nil {
		options.CacheBudget = managers[0]
		options.CacheRAMBytes = cfg.ResourceBudget.AuditLogRAMBytes
	}
	return audit.OpenWithOptions(path, key, auditLogOptions(cfg, options))
}
