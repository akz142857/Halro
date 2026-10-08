package app

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
)

type commandResourceScopeKey struct{}

// An offline command's nested native readers and disk workspaces must share
// one logical manager. The scope is request-local, never a process registry.
// All actual permits remain owned by their physical readers/writers.
type commandResourceScope struct {
	selectedMu      sync.Mutex
	selectedOpening bool
	selectedReader  *selectedCommandMetadataReader
	selected        *selectedCommandMetadataBackend
	selectedLoans   [4]*audit.Log
	pressure        *resourcePressureRuntime
	budget          config.ResourceBudget
	policy          config.ResourcePressure
	metadataMu      sync.Mutex
	metadataFiles   [commandMetadataFileSlots]commandMetadataFileOwner
	backupMu        sync.Mutex
	backupBudget    *backup.WorkspaceBudget
	stagingMu       sync.Mutex
	backupStaging   *backupStagingDiskOwner
	nativeMu        sync.Mutex
	nativeDisk      *commandNativeDiskOwner
	outputMu        sync.Mutex
	backupOutput    *backupOutputDiskOwner
	auditMu         sync.Mutex
	auditPath       string
	auditDisk       *runtimeProjectionDiskPermit
}

func commandBudgetContext(ctx context.Context, cfg config.Config) (context.Context, *resourcePressureRuntime, error) {
	if ctx == nil {
		return nil, nil, errors.New("command resource scope requires context")
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if saved, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok {
		if cfg.ResourceBudget == nil || cfg.ResourcePressure == nil || saved.budget != *cfg.ResourceBudget || saved.policy != *cfg.ResourcePressure {
			return ctx, nil, errors.New("nested command changed its shared resource configuration")
		}
		return ctx, saved.pressure, nil
	}
	if cfg.ResourceBudget == nil {
		return ctx, nil, nil
	}
	pressure, err := newResourcePressure(cfg)
	if err != nil {
		return ctx, nil, err
	}
	if pressure.logicalBudget() == nil {
		return ctx, nil, errors.New("command resource scope has no shared manager")
	}
	scope := &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure}
	return context.WithValue(ctx, commandResourceScopeKey{}, scope), pressure, nil
}

func commandPressure(ctx context.Context, cfg config.Config, explicit *resourcePressureRuntime) (*resourcePressureRuntime, error) {
	if ctx == nil {
		return nil, errors.New("command resource scope requires context")
	}
	if _, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok {
		_, shared, err := commandBudgetContext(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if explicit != nil && explicit.logicalBudget() != shared.logicalBudget() {
			return nil, errors.New("nested command substituted its shared manager")
		}
		return shared, nil
	}
	return explicit, nil
}

func openOneShotConfiguredAuditLog(ctx context.Context, cfg config.Config, path string, key []byte, options audit.Options) (*audit.Log, error) {
	if scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok {
		scope.selectedMu.Lock()
		if selected := scope.selected; selected != nil {
			if selected.closing {
				scope.selectedMu.Unlock()
				return nil, errors.New("selected command has withdrawn Audit borrower admission")
			}
			if path != cfg.AuditPath() || options.Replica || !selected.guard.CoversNativePath(path) {
				scope.selectedMu.Unlock()
				return nil, errors.New("selected command Audit path/role changed")
			}
			for slot := range scope.selectedLoans {
				if scope.selectedLoans[slot] == nil {
					loan, err := selected.state.generation.Owners.Audit.BorrowSelectedCommand(ctx, selected.guard, scope.pressure.logicalBudget(), key)
					if err == nil {
						scope.selectedLoans[slot] = loan
					}
					scope.selectedMu.Unlock()
					return loan, err
				}
			}
			scope.selectedMu.Unlock()
			return nil, errors.New("selected command exceeded four Audit borrower slots")
		}
		scope.selectedMu.Unlock()
	}
	_, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return nil, err
	}
	log, err := openConfiguredAuditLog(cfg, path, key, options, pressure.logicalBudget())
	if err != nil {
		return nil, err
	}
	if scope := keyScope(ctx); scope != nil {
		if err := scope.trackCloser(log); err != nil {
			return nil, errors.Join(err, log.Close())
		}
	}
	return log, nil
}
