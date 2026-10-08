package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/startupaudit"
	"github.com/akz142857/Halro/internal/statecut"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// One immutable source and one Monitor span a diagnostic's nested DAO opens.
// The original process guard owns their final retirement, including failures.
type selectedCommandMetadataReader struct {
	mu          sync.Mutex
	closeOnce   sync.Once
	closeErr    error
	guard       *lock.Lock
	path        string
	state       runtimeSelectedState
	metadata    *metadatastate.Baseline
	old         *boltstore.Store
	native      *commandNativeDiskOwner
	backend     boltstore.FacadeBackend
	aliases     [8]*boltstore.Store
	aliasCount  int
	scratch     string
	scratchInfo os.FileInfo
	scratchFee  resourcebudget.Permit
}

type selectedReadFacade struct {
	boltstore.FacadeBackend
	scratch string
}

// Alias Close retires its DAO only. The original directory guard joins every
// alias before retiring the shared physical source and its sampling loop.
func (b selectedReadFacade) Close() error { return nil }

// DAO-local price audit scratch belongs outside the application data tree.
// The authenticated backend retains the original source path and journal.
func (b selectedReadFacade) Path() string { return filepath.Join(b.scratch, "metadata-view") }

func (r *selectedCommandMetadataReader) close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, alias := range r.aliases {
			if alias != nil {
				r.closeErr = errors.Join(r.closeErr, alias.Close())
			}
		}
		if r.metadata != nil {
			r.closeErr = errors.Join(r.closeErr, r.metadata.Close())
		}
		if r.old != nil {
			r.closeErr = errors.Join(r.closeErr, r.old.Close())
		}
		if r.scratch != "" && r.closeErr == nil {
			actual, err := os.Lstat(r.scratch)
			if err != nil || r.scratchInfo == nil || !os.SameFile(actual, r.scratchInfo) {
				r.closeErr = errors.Join(r.closeErr, err, errors.New("diagnostic scratch directory was replaced"))
			} else {
				r.closeErr = os.Remove(r.scratch) // only the joined, empty namespace
				if r.closeErr == nil {
					r.closeErr = durable.SyncDirectory(filepath.Dir(r.scratch))
				}
				if r.closeErr == nil {
					r.closeErr = r.scratchFee.Release()
				}
			}
		}
		r.closeErr = errors.Join(r.closeErr, r.state.close())
		r.closeErr = r.native.finish(r.closeErr)
	})
	return r.closeErr
}

func (r *selectedCommandMetadataReader) alias(ctx context.Context) (*boltstore.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.backend == nil || r.closeErr != nil || r.aliasCount == len(r.aliases) || !r.guard.CoversDataDirectory(filepath.Dir(r.path)) {
		return nil, errors.New("selected read-only command source is unavailable or exhausted its eight DAO slots")
	}
	alias, err := boltstore.NewFacadeStore(ctx, selectedReadFacade{FacadeBackend: r.backend, scratch: r.scratch})
	if err != nil {
		return nil, err
	}
	if scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); !ok {
		return nil, errors.Join(resourcebudget.ErrInvalid, alias.Close())
	} else if err := alias.ConfigurePricingResources(scope.pressure.logicalBudget(), scope.budget.PricingLifetimeRAMBytes, scope.budget.PricingIORAMBytes); err != nil {
		return nil, errors.Join(err, alias.Close())
	}
	r.aliases[r.aliasCount], r.aliasCount = alias, r.aliasCount+1
	return alias, nil
}

func openSelectedCommandMetadataReadOnly(ctx context.Context, cfg config.Config, path string) (result *boltstore.Store, resultErr error) {
	if err := checkSelectedRotationPending(cfg, nil); err != nil {
		return nil, err
	}
	guard, _ := ctx.Value(commandDataLockKey{}).(*lock.Lock)
	scope, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if guard == nil || scope == nil || cfg.ResourceBudget == nil || cfg.Replication != nil || path != cfg.MetadataPath() || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return nil, errors.New("selected read-only Metadata requires the original command guard, Standalone configuration and shared budget")
	}
	scope.selectedMu.Lock()
	defer scope.selectedMu.Unlock()
	if scope.selected != nil || scope.selectedOpening {
		return nil, errors.New("read-only command cannot borrow a mutable selected generation")
	}
	if reader := scope.selectedReader; reader != nil {
		if reader.guard != guard || reader.path != path {
			return nil, errors.New("read-only command substituted its original source")
		}
		return reader.alias(ctx)
	}
	r := &selectedCommandMetadataReader{guard: guard, path: path}
	if err := guard.RetainCleanup(r.close); err != nil {
		return nil, err
	}
	scope.selectedReader = r
	// The guard now retains partial/failed actual sources too. Known factory
	// errors perform cleanup here; uncertain cleanup stays reachable until exit.
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, r.close())
		}
	}()
	manager := scope.pressure.logicalBudget()
	if manager == nil || scope.pressure.monitor == nil {
		return nil, resourcebudget.ErrInvalid
	}
	var err error
	r.old, err = boltstore.OpenReadOnlyWithMappingBudget(ctx, path, metadataLegacyFileCeiling(cfg), manager)
	if err != nil {
		return nil, err
	}
	master, err := unlockMasterKey(ctx, cfg, r.old)
	if err != nil {
		return nil, err
	}
	defer clear(master)
	v, err := vault.New(master)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	if err := verifyVaultKeyCheck(r.old, v); err != nil {
		return nil, err
	}
	if err := r.old.Close(); err != nil {
		return nil, err
	}
	if err := startSelectedPressureMonitor(ctx, cfg, manager, scope.pressure, &r.state); err != nil {
		return nil, err
	}
	r.scratchFee, err = manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4 << 10, Disk: 64 << 10})
	if err != nil {
		return nil, err
	}
	r.scratch, err = os.MkdirTemp("", "halro-selected-diagnostic-")
	if err != nil {
		_ = r.scratchFee.Release()
		return nil, err
	}
	r.scratchInfo, err = os.Lstat(r.scratch)
	if err != nil {
		return nil, err
	}
	b, err := readMemoryStateBootstrap(ctx, cfg, v, manager)
	if err != nil || b == nil {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	r.state.bootstrap = *b
	r.native, err = prepareCommandNativeDisk(ctx, cfg, guard)
	if err != nil {
		return nil, err
	}
	acquire := func(bytes int64) (resourcebudget.Permit, error) {
		fee, err := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bytes})
		if err == nil {
			r.state.fees = append(r.state.fees, fee)
		}
		return fee, err
	}
	sealFee, err := acquire(32 << 10)
	if err != nil {
		return nil, err
	}
	r.state.publisher, err = authenticatedstate.OpenSealCandidateReader(cfg.Storage.DataDir, guard, b.LedgerKey, statecut.HardLimits(), manager, sealFee)
	if err != nil {
		return nil, err
	}
	selected, found, err := r.state.publisher.Current()
	if err != nil || !found {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	authority := b.Authority
	copy(authority.Positions[:], selected.Positions[:])
	if err := authenticatedstate.ValidateSealEligibility(selected, authority); err != nil {
		return nil, err
	}
	rootFee, err := acquire(cfg.ResourceBudget.MetadataStoreDiskBytes)
	if err != nil {
		return nil, err
	}
	journalFee, err := acquire(cfg.ResourceBudget.MetadataJournalDiskBytes)
	if err != nil {
		return nil, err
	}
	o := metadatastate.SelectedOpenOptions{ReadOnly: true, BaselineOptions: metadatastate.BaselineOptions{Identity: authority.Instance, InstanceID: b.InstanceID, KeyGeneration: b.KeyGeneration, Budget: manager, MaxSourceBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxDiskBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxAppendBytes: min(cfg.ResourceBudget.MetadataStoreDiskBytes/2, 16<<20)}, Evidence: b.Evidence, Binding: b.Binding, Publisher: r.state.publisher, Guard: guard, Authority: authority, RootDiskReservation: rootFee, JournalDiskReservation: journalFee, MaxJournalBytes: cfg.ResourceBudget.MetadataJournalDiskBytes}
	metadataDirectory, metadataJournalPath := memoryStateMetadataPaths(cfg, b.MetadataSlot)
	r.metadata, _, err = metadatastate.OpenSelectedFromRoot(ctx, b.LedgerKey, metadataDirectory, metadataJournalPath, o)
	if err != nil {
		return nil, err
	}
	a, err := r.metadata.StartSelectedStartupAudit(ctx, startupaudit.Options{Deadline: 10 * time.Minute, ProgressTimeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := a.Wait(ctx); err != nil {
		return nil, err
	}
	r.backend, err = r.metadata.SelectedStartupFacade(ctx, manager, authority.Instance, b.InstanceID, b.KeyGeneration)
	if err != nil {
		return nil, err
	}
	return r.alias(ctx)
}
