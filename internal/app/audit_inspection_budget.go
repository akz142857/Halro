package app

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const auditInspectionRAMBytes int64 = 32 << 20

func inspectCommandAudit(ctx context.Context, cfg config.Config, key []byte, visit func(audit.Record) error) (result audit.Summary, resultErr error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return audit.Summary{}, err
	}
	var permit resourcebudget.Permit
	if cfg.ResourceBudget != nil {
		permit, err = pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: auditInspectionRAMBytes})
		if err != nil {
			return audit.Summary{}, err
		}
	}
	defer func() {
		// Failed actual FD Close cannot return its live decoding envelope.
		if !errors.Is(resultErr, audit.ErrInspectionRetirement) && permit != (resourcebudget.Permit{}) {
			resultErr = errors.Join(resultErr, permit.Release())
		}
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			result = audit.Summary{}
		}
	}()
	maximumBytes := int64(0)
	if scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok {
		path, err := filepath.Abs(cfg.AuditPath())
		if err != nil {
			return audit.Summary{}, err
		}
		scope.auditMu.Lock()
		if scope.auditPath != "" && scope.auditPath != path {
			err = errors.New("nested command substituted its Audit source path")
		} else {
			scope.auditPath = path
			if scope.auditDisk == nil {
				scope.auditDisk, err = acquireAuditLogDisk(ctx, cfg, pressure)
			}
		}
		scope.auditMu.Unlock()
		if err != nil {
			return audit.Summary{}, err
		}
		maximumBytes = scope.budget.AuditLogDiskBytes
		// Native bytes survive descriptor Close. The fixed source Disk claim
		// stays in the original command scope, rather than being refunded.
	}
	var selected authenticatedstate.SealPosition
	hasSelected := false
	if scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok && scope.selectedReader != nil {
		reader := scope.selectedReader
		if !reader.guard.CoversNativePath(cfg.AuditPath()) {
			return audit.Summary{}, authenticatedstate.ErrSealIneligible
		}
		seal, found, err := reader.state.publisher.Current()
		if err != nil || !found {
			return audit.Summary{}, errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		for _, position := range seal.Positions[:seal.PositionCount] {
			if position.Component == authenticatedstate.SealComponentAudit {
				selected, hasSelected = position, true
			}
		}
		if !hasSelected {
			return audit.Summary{}, authenticatedstate.ErrSealIneligible
		}
	}
	matched := selected.Sequence == 0 && selected.Offset == 0 && selected.Hash == ([32]byte{})
	summary, err := audit.VerifyWithVisitorContext(ctx, cfg.AuditPath(), key, maximumBytes, func(record audit.Record) error {
		if hasSelected && record.Sequence == selected.Sequence {
			matched = record.Hash == selected.Hash && uint64(record.Offset) == selected.Offset
		}
		if visit != nil {
			return visit(record)
		}
		return nil
	})
	if err == nil && hasSelected && !matched {
		err = errors.New("audit source does not match its selected common-cut prefix")
	}
	return summary, err
}
