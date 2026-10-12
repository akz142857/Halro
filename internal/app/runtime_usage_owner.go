package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// This owner is rebuilt under Standalone native maintenance admission. Saved
// bytes are never a restart checkpoint. Collector retirement and public-reader
// drain precede close; the selected Ledger must outlive the derivative.
type runtimeUsageOwner struct {
	selected             bool // Physical roots belong to the common selected generation.
	retirementErr        error
	closeOnce            sync.Once
	closeErr             error
	derivative           *usage.LedgerDerivative
	disk                 *usage.DiskProjection
	collector            *usage.Collector
	archive              *usage.Exporter
	native               *ledger.Log
	nativeKey            [32]byte
	accounting           *ledger.LedgerProjection
	accountingPath       string
	accountingFile       os.FileInfo
	diskPermit           *runtimeProjectionDiskPermit
	archiveDiskPermit    *runtimeProjectionDiskPermit
	archiveDiskBudget    *runtimeUsageArchiveDiskBudget
	accountingDiskPermit *runtimeProjectionDiskPermit
	resourcePressure     *resourcePressureRuntime
	checkRole            func() error
	path                 string
	file                 os.FileInfo
	maintenanceMu        sync.Mutex
	maintenanceRetired   atomic.Bool
	maintenanceCtx       context.Context
	maintenanceCancel    context.CancelFunc
}

func standaloneUsageProjectionPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "usage-projection.db")
}

// bindArchive copies the independent native key while the startup caller still
// owns it. The resolver authenticates physical native bytes, never a derivative
// checksum. Its key and borrowed native source outlive actual archive callbacks;
// close joins the Exporter before clearing them or closing the reducer.
func (o *runtimeUsageOwner) bindArchive(ctx context.Context, cfg config.Config, native *ledger.Log, key []byte) (*usage.Exporter, error) {
	if cfg.Replication != nil {
		return nil, errors.New("Standalone Usage archive cannot bind a member")
	}
	return o.bindNativeArchive(ctx, cfg, native, key)
}

func (o *runtimeUsageOwner) bindNativeArchive(ctx context.Context, cfg config.Config, native *ledger.Log, key []byte) (*usage.Exporter, error) {
	if o == nil || o.disk == nil || native == nil || o.native != native || !o.selected && o.path != standaloneUsageProjectionPath(cfg) || len(key) != len(o.nativeKey) || o.archive != nil || cfg.Replication != nil && o.checkRole == nil {
		return nil, errors.New("Usage archive requires a fresh Standalone owner and independent native key")
	}
	if o.checkRole != nil {
		if err := o.checkRole(); err != nil {
			return nil, err
		}
	}
	if _, err := o.disk.ReadReplayAnchor(ctx); err != nil {
		return nil, err
	}
	copy(o.nativeKey[:], key)
	archivePermit, err := acquireUsageArchiveDisk(ctx, cfg, o.resourcePressure)
	if err != nil {
		clear(o.nativeKey[:])
		return nil, err
	}
	archiveBudget := newRuntimeUsageArchiveDiskBudget(cfg, o.resourcePressure)
	var reserveArchiveDisk func(context.Context, int64) (func() error, error)
	if archiveBudget != nil {
		reserveArchiveDisk = archiveBudget.reserve
	}
	reserveArchiveRAM := archiveBudget.readerReservation(cfg)
	options := usage.Options{ReclaimCrashTemporaries: true, Format: cfg.Usage.ExportFormat, ArchiveDiskBytes: usageArchiveDiskCeiling(cfg), ReserveArchiveDisk: reserveArchiveDisk, ReserveArchiveRAM: reserveArchiveRAM, ReserveArchiveWorkRAM: archiveBudget.workReservation(), DatePolicyKey: o.nativeKey[:], DatePolicyPrefix: func(ctx context.Context, head ledger.Watermark) (ledger.ProjectionPrefix, error) {
		if o.checkRole != nil {
			if err := o.checkRole(); err != nil {
				return ledger.ProjectionPrefix{}, err
			}
		}
		prefix, actual, err := ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), o.nativeKey[:], head.Generation, head.Sequence)
		current, hash, ok := native.ChainHead()
		// ChainHead's bool means that at least one MAC frame exists. A fully
		// inspected empty genesis has no frames; retain its exact native roll
		// position instead of treating that valid absence as a failed audit.
		empty := !ok && !actual.ChainVerified && current.Sequence == 0 && current.Offset == 0 && hash == ([32]byte{}) && actual.ChainSequence == 0 && actual.ChainOffset == 0
		if err != nil || !ok && !empty || prefix.Head != head || actual.Head != current || actual.ChainHash != hash || head.After(current) {
			return ledger.ProjectionPrefix{}, errors.Join(err, usage.ErrArchiveDateProof)
		}
		if o.checkRole != nil {
			if err := o.checkRole(); err != nil {
				return ledger.ProjectionPrefix{}, err
			}
		}
		return prefix, ctx.Err()
	}}
	e, err := usage.NewExporterWithContext(ctx, cfg.UsagePath(), options)
	if err != nil {
		if archivePermit != nil {
			_ = archivePermit.Release()
		}
		clear(o.nativeKey[:])
		return nil, err
	}
	if archivePermit != nil {
		if err := archivePermit.Reclassify(resourcebudget.Background); err != nil {
			_ = e.Close()
			_ = archivePermit.Release()
			clear(o.nativeKey[:])
			return nil, fmt.Errorf("retain Usage archive disk ceiling: %w", err)
		}
	}
	o.archiveDiskPermit = archivePermit
	o.archiveDiskBudget = archiveBudget
	o.archive = e
	return e, nil
}

func (o *runtimeUsageOwner) activateArchiveIO() {
	if o != nil {
		o.archiveDiskBudget.activateIO()
	}
}

// prepareStandaloneUsage constructs a complete disk reducer and its live source
// before producers start. The caller holds the directory lock and native lease.
// Default Standalone startup uses this owner; both retention jobs and public
// queries share it. Member roles use their separate prefix and lifetime rules.
func prepareStandaloneUsage(ctx context.Context, cfg config.Config, native *ledger.Log, accounting usage.LedgerDerivativeOwner, lease *ledger.MaintenanceLease, pressure *resourcePressureRuntime) (view *runtimeUsage, collector *usage.Collector, err error) {
	if cfg.Replication != nil || native == nil || accounting == nil || lease == nil {
		return nil, nil, errors.New("disk Usage startup requires Standalone native and selected Ledger maintenance ownership")
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || replicated {
		return nil, nil, errors.Join(err, errors.New("Usage startup requires this Standalone native maintenance owner"))
	}
	path := standaloneUsageProjectionPath(cfg)
	if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
		return nil, nil, err
	}
	releaseWorkspace, err := acquireProjectionWorkspace(ctx, cfg, pressure)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if !errors.Is(err, ownedfile.ErrRetirement) {
			err = errors.Join(err, releaseWorkspace())
		}
	}()
	diskPermit, err := acquireUsageProjectionDisk(ctx, cfg, pressure)
	if err != nil {
		return nil, nil, err
	}
	o := &runtimeUsageOwner{path: path, native: native, diskPermit: diskPermit, resourcePressure: pressure}
	complete := false
	o.maintenanceCtx, o.maintenanceCancel = context.WithCancel(context.Background())
	defer func() {
		if !complete {
			if errors.Is(err, ownedfile.ErrRetirement) {
				o.retirementErr = errors.Join(o.retirementErr, err)
			}
			err = errors.Join(err, o.close())
			view, collector = nil, nil
		}
	}()
	err = lease.WithHead(ctx, func(ctx context.Context, frozen ledger.Watermark, hash [32]byte) error {
		actual, actualHash, err := accounting.ReadPosition(ctx)
		if err != nil || actual != frozen || actualHash != hash {
			return errors.Join(err, errors.New("Usage startup Ledger differs from exclusive native prefix"))
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("discard prior Usage derivative: %w", err)
		}
		maxDiskBytes := int64(0)
		if cfg.ResourceBudget != nil {
			maxDiskBytes = cfg.ResourceBudget.UsageProjectionDiskBytes
		}
		o.derivative, o.disk, err = usage.NewDiskLedgerDerivativeWithLimit(accounting, path, maxDiskBytes)
		if err != nil {
			return usageDiskCapacityLeaseError(err)
		}
		o.file, err = os.Lstat(path)
		if err != nil {
			return err
		}
		head, err := replayDiskUsageBatches(ctx, native, ledger.ReplayAnchor{}, o.derivative, nil)
		if err != nil || head != frozen {
			return errors.Join(usageDiskCapacityLeaseError(err), errors.New("Usage startup did not authenticate the entire frozen native prefix"))
		}
		anchor, err := o.disk.ReadReplayAnchor(ctx)
		if err != nil || anchor.Head.Sequence != frozen.Sequence || anchor.Hash != hash {
			return errors.Join(err, usage.ErrCollectorHead)
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	o.collector, err = usage.NewProjectedCollector(o.derivative, func(ctx context.Context, from ledger.Watermark, visit func(ledger.Record) error) (ledger.Watermark, error) {
		anchor, err := o.disk.ReadReplayAnchor(ctx)
		if err != nil {
			return ledger.Watermark{}, err
		}
		if from != anchor.Head {
			return ledger.Watermark{}, usage.ErrCollectorHead
		}
		return native.ReplayAuthenticated(ctx, anchor, visit)
	}, cfg.Usage.AnalyticsQueueCapacity)
	if err != nil {
		return nil, nil, err
	}
	if o.diskPermit != nil {
		if err := o.diskPermit.Reclassify(resourcebudget.Background); err != nil {
			return nil, nil, err
		}
	}
	complete = true
	return &runtimeUsage{disk: o.disk, owner: o}, o.collector, nil
}

func (o *runtimeUsageOwner) close() error {
	if o == nil {
		return nil
	}
	o.closeOnce.Do(func() { o.closeErr = o.closeOwned() })
	return o.closeErr
}

func (o *runtimeUsageOwner) closeOwned() error {
	err := o.retirementErr
	o.retireMaintenance()
	if o.collector != nil {
		err = errors.Join(err, o.collector.Retire(context.Background()))
	}
	if o.archive != nil {
		err = errors.Join(err, o.archive.Close())
	}
	clear(o.nativeKey[:])
	o.native = nil
	if o.selected {
		// The common owner joins and closes this derivative/root. Preserve all
		// physical pages and their lifetime fees for the next selected startup.
		return err
	}
	if o.derivative != nil {
		o.derivative.Close()
	}
	if o.disk != nil {
		discardErr := o.disk.Discard()
		err = errors.Join(err, discardErr)
		if discardErr == nil && o.diskPermit != nil && o.retirementErr == nil {
			err = errors.Join(err, o.diskPermit.Release())
			o.diskPermit = nil
		}
	} else if o.diskPermit != nil && o.retirementErr == nil {
		removed, statErr := derivedFileRemoved(o.path)
		err = errors.Join(err, statErr)
		if statErr == nil && removed {
			err = errors.Join(err, o.diskPermit.Release())
			o.diskPermit = nil
		} else if statErr == nil {
			err = errors.Join(err, errors.New("Usage file remains; retain its Disk claim"))
		}
	}
	if o.accounting != nil && o.accountingPath != "" {
		discardErr := o.accounting.Discard()
		err = errors.Join(err, discardErr)
		if discardErr == nil && o.accountingDiskPermit != nil && o.retirementErr == nil {
			err = errors.Join(err, o.accountingDiskPermit.Release())
			o.accountingDiskPermit = nil
		}
	} else if o.accountingDiskPermit != nil && o.retirementErr == nil {
		removed, statErr := derivedFileRemoved(o.accountingPath)
		err = errors.Join(err, statErr)
		if statErr == nil && removed {
			err = errors.Join(err, o.accountingDiskPermit.Release())
			o.accountingDiskPermit = nil
		} else if statErr == nil {
			err = errors.Join(err, errors.New("accounting Ledger file remains; retain its Disk claim"))
		}
	}
	o.accounting = nil
	return err
}

func (o *runtimeUsageOwner) retireMaintenance() {
	if o == nil {
		return
	}
	o.maintenanceRetired.Store(true)
	if o.maintenanceCancel != nil {
		o.maintenanceCancel()
	}
	o.maintenanceMu.Lock()
	o.maintenanceMu.Unlock()
}

func (u *runtimeUsage) closeOwner() error {
	if u == nil {
		return nil
	}
	return u.owner.close()
}
