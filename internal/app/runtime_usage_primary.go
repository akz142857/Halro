package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

func primaryUsageLedgerPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "usage-ledger-projection.db")
}

// The independent construction helper keeps its evidence on disk. Default
// Primary startup calls preparePrimaryUsageOwner with the selected business
// projection and its lease, borrowing evidence without a second Ledger DB.
func preparePrimaryUsage(ctx context.Context, cfg config.Config, native *ledger.Log, member *replicationRuntime, pressure *resourcePressureRuntime) (view *runtimeUsage, collector *usage.Collector, err error) {
	return preparePrimaryUsageOwner(ctx, cfg, native, member, nil, nil, pressure)
}

func preparePrimaryUsageOwner(ctx context.Context, cfg config.Config, native *ledger.Log, member *replicationRuntime, accounting *ledger.LedgerProjection, borrowed *ledger.MaintenanceLease, pressure *resourcePressureRuntime) (view *runtimeUsage, collector *usage.Collector, err error) {
	if cfg.Replication == nil || native == nil || member == nil || member.role != replication.RolePrimary || member.publisher == nil {
		return nil, nil, errors.New("Primary Usage requires authenticated member/native owners")
	}
	before := member.publisher.Snapshot()
	o := &runtimeUsageOwner{path: standaloneUsageProjectionPath(cfg), accountingPath: primaryUsageLedgerPath(cfg), native: native, resourcePressure: pressure}
	if accounting != nil {
		if borrowed == nil {
			return nil, nil, errors.New("Primary Usage requires the selected accounting lease")
		}
		o.accounting, o.accountingPath = accounting, "" // Borrowed from the business owner; never closed here.
	}
	o.checkRole = func() error {
		current := member.publisher.Snapshot()
		if current.Role != replication.RolePrimary || before.Role != replication.RolePrimary || current.ClusterID != before.ClusterID || current.Incarnation != before.Incarnation || current.NodeID != before.NodeID || current.Term != before.Term || current.PromisedTerm != before.PromisedTerm {
			return errors.New("Primary Usage member identity or term changed")
		}
		return nil
	}
	if err := o.checkRole(); err != nil {
		return nil, nil, fmt.Errorf("validate Primary role for Usage: %w", err)
	}
	for _, path := range []string{o.path, o.accountingPath} {
		if path == "" {
			continue
		}
		if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
			return nil, nil, fmt.Errorf("validate Primary Usage path: %w", err)
		}
	}
	releaseWorkspace, err := acquireProjectionWorkspace(ctx, cfg, pressure)
	if err != nil {
		return nil, nil, fmt.Errorf("reserve Usage startup workspace: %w", err)
	}
	defer func() {
		if !errors.Is(err, ownedfile.ErrRetirement) {
			err = errors.Join(err, releaseWorkspace())
		}
	}()
	complete := false
	lease := borrowed
	defer func() {
		if !complete {
			if errors.Is(err, ownedfile.ErrRetirement) {
				o.retirementErr = errors.Join(o.retirementErr, err)
			}
			err = errors.Join(err, o.close())
			view, collector = nil, nil
		}
		if borrowed == nil && lease != nil {
			err = errors.Join(err, lease.Close())
		}
	}()
	diskPermit, err := acquireUsageProjectionDisk(ctx, cfg, pressure)
	if err != nil {
		return nil, nil, fmt.Errorf("reserve Usage projection disk ceiling: %w", err)
	}
	o.diskPermit = diskPermit
	if accounting == nil {
		o.accountingDiskPermit, err = acquireLedgerProjectionDisk(ctx, cfg, pressure)
		if err != nil {
			return nil, nil, fmt.Errorf("reserve Primary Ledger projection disk ceiling: %w", err)
		}
	}
	o.maintenanceCtx, o.maintenanceCancel = context.WithCancel(context.Background())
	// Startup has not published producers. Semantic maintenance freezes the
	// durable Primary cut during the two fresh builds; it supplies no HA authority.
	if lease == nil {
		lease, err = native.BeginMaintenance(ctx)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("freeze Primary Usage source: %w", err)
	}
	err = lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		for _, path := range []string{o.path, o.accountingPath} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove prior Usage or Ledger derivative: %w", err)
			}
		}
		if accounting == nil {
			var prefix ledger.ProjectionPrefix
			maxDiskBytes := int64(0)
			if cfg.ResourceBudget != nil {
				maxDiskBytes = cfg.ResourceBudget.LedgerProjectionDiskBytes
			}
			if maxDiskBytes > 0 {
				o.accounting, prefix, err = native.BuildProjectionFromCursorWithLimit(ctx, head.Generation, head.Sequence, o.accountingPath, maxDiskBytes)
			} else {
				o.accounting, prefix, err = native.BuildProjectionFromCursor(ctx, head.Generation, head.Sequence, o.accountingPath)
			}
			if err != nil {
				return fmt.Errorf("build Primary Ledger projection: %w", err)
			}
			o.accountingFile, err = os.Lstat(o.accountingPath)
			if err != nil || prefix.Head != head || prefix.Hash != hash {
				return errors.Join(err, usage.ErrCollectorHead)
			}
		}
		actualHead, actualHash, err := o.accounting.ReadPosition(ctx)
		if err != nil || actualHead != head || actualHash != hash {
			return errors.Join(fmt.Errorf("read selected Primary Ledger position: %w", err), usage.ErrCollectorHead)
		}
		usageDiskBytes := int64(0)
		if cfg.ResourceBudget != nil {
			usageDiskBytes = cfg.ResourceBudget.UsageProjectionDiskBytes
		}
		o.derivative, o.disk, err = usage.NewDiskLedgerDerivativeWithLimit(o.accounting, o.path, usageDiskBytes)
		if err != nil {
			return fmt.Errorf("build Primary Usage derivative: %w", usageDiskCapacityLeaseError(err))
		}
		o.file, err = os.Lstat(o.path)
		if err != nil {
			return fmt.Errorf("stat Primary Usage derivative: %w", err)
		}
		actual, err := replayDiskUsageBatches(ctx, native, ledger.ReplayAnchor{}, o.derivative, nil)
		if err != nil || actual != head {
			return errors.Join(fmt.Errorf("replay Primary Usage derivative: %w", usageDiskCapacityLeaseError(err)), usage.ErrCollectorHead)
		}
		return o.checkRole()
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build Primary Usage projection: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("check Primary Usage startup context: %w", err)
	}
	o.collector, err = usage.NewProjectedCollector(o.derivative, func(ctx context.Context, from ledger.Watermark, visit func(ledger.Record) error) (ledger.Watermark, error) {
		if err := o.checkRole(); err != nil {
			return ledger.Watermark{}, err
		}
		anchor, err := o.disk.ReadReplayAnchor(ctx)
		if err != nil || anchor.Head != from {
			return ledger.Watermark{}, errors.Join(err, usage.ErrCollectorHead)
		}
		actual, err := native.ReplayAuthenticated(ctx, anchor, func(record ledger.Record) error {
			if err := o.checkRole(); err != nil {
				return err
			}
			if accounting == nil {
				if err := o.accounting.Apply(ctx, record); err != nil {
					return err
				}
			}
			return visit(record)
		})
		return actual, errors.Join(err, o.checkRole())
	}, cfg.Usage.AnalyticsQueueCapacity)
	if err != nil {
		return nil, nil, fmt.Errorf("start Primary Usage collector: %w", err)
	}
	if borrowed == nil {
		if err := lease.Close(); err != nil {
			return nil, nil, fmt.Errorf("release Primary Usage startup lease: %w", err)
		}
		lease = nil
	}
	if o.diskPermit != nil {
		if err := o.diskPermit.Reclassify(resourcebudget.Background); err != nil {
			return nil, nil, fmt.Errorf("retain Usage projection disk ceiling: %w", err)
		}
	}
	if o.accountingDiskPermit != nil {
		if err := o.accountingDiskPermit.Reclassify(resourcebudget.Background); err != nil {
			return nil, nil, fmt.Errorf("retain Primary Ledger projection disk ceiling: %w", err)
		}
	}
	complete = true
	return &runtimeUsage{disk: o.disk, owner: o}, o.collector, nil
}
