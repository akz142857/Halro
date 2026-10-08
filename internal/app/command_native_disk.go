package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// One offline source tree, one immutable command Manager. Every native IO
// consumer receives this original growth owner; private staging is separate.
type commandNativeDiskOwner struct {
	mu       sync.Mutex
	ready    bool
	closed   bool
	root     string
	guard    *lock.Lock
	baseline *diskinventory.Baseline
	growth   *runtimeLedgerAppendDiskOwner
}

type commandNativeDiskFailure struct {
	owner *commandNativeDiskOwner
	cause error
}

func (e *commandNativeDiskFailure) Error() string { return e.cause.Error() }
func (e *commandNativeDiskFailure) Unwrap() error { return e.cause }

// One actual native reader and its existing one-shot index permit stay
// reachable on uncertain retirement. Never retain a historical reader list.
type commandLedgerDiskFailure struct {
	index  *runtimeProjectionDiskPermit
	native any
	cause  error
}

func (e *commandLedgerDiskFailure) Error() string { return e.cause.Error() }
func (e *commandLedgerDiskFailure) Unwrap() error { return e.cause }

func retireCommandLedgerIndex(index *runtimeProjectionDiskPermit, native any, cause error) error {
	if errors.Is(cause, ownedfile.ErrRetirement) || errors.Is(cause, ledger.ErrSegmentRetirement) {
		return &commandLedgerDiskFailure{index: index, native: native, cause: cause}
	}
	return errors.Join(cause, index.Release())
}

func prepareCommandNativeDisk(ctx context.Context, cfg config.Config, guard *lock.Lock) (*commandNativeDiskOwner, error) {
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.PersistentInventoryDiskBytes == 0 && cfg.ResourceBudget.LedgerAppendDiskBytes == 0 {
		return nil, nil
	}
	pressure, err := commandPressure(ctx, cfg, nil)
	if err != nil {
		return nil, err
	}
	scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if !ok || pressure == nil || pressure.logicalBudget() == nil || guard == nil {
		return nil, resourcebudget.ErrInvalid
	}
	root, err := filepath.Abs(cfg.Storage.DataDir)
	if err != nil {
		return nil, err
	}
	scope.nativeMu.Lock()
	defer scope.nativeMu.Unlock()
	if owner := scope.nativeDisk; owner != nil {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		if owner.root != root || owner.guard != guard {
			return nil, errors.New("offline native source replaced its original tree or lock")
		}
		if !owner.ready || owner.closed {
			return nil, resourcebudget.ErrClosed
		}
		return owner, nil
	}
	owner := &commandNativeDiskOwner{root: root, guard: guard}
	scope.nativeDisk = owner // Keep failed/unknown preparation reachable too.
	owner.growth, err = prepareRuntimeLedgerAppendDisk(ctx, cfg, pressure)
	if err == nil {
		owner.baseline, err = preparePersistentInventory(ctx, cfg, guard, pressure)
	}
	if err == nil {
		err = owner.growth.publish()
	}
	if err != nil {
		return owner, &commandNativeDiskFailure{owner: owner, cause: err}
	}
	owner.ready = true
	return owner, nil
}

func commandNativeOptions(ctx context.Context, options ledger.Options) ledger.Options {
	if scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope); ok {
		scope.nativeMu.Lock()
		defer scope.nativeMu.Unlock()
		if owner := scope.nativeDisk; owner != nil {
			owner.mu.Lock()
			defer owner.mu.Unlock()
			if !owner.ready || owner.closed {
				options.ReserveAppendDisk = func(int64) error { return resourcebudget.ErrClosed }
				options.ReserveNativeFileDisk = func(int64) (ledger.NativeFileDiskReservation, error) { return nil, resourcebudget.ErrClosed }
			} else if owner.growth != nil {
				options.ReserveAppendDisk = owner.growth.reserve
				options.ReserveNativeFileDisk = owner.growth.reserveNativeFile
			}
		}
	}
	return options
}

func (o *commandNativeDiskOwner) finish(cause error) error {
	if o == nil {
		return cause
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	cause = errors.Join(cause, o.growth.close(), o.baseline.Close())
	if cause != nil {
		return &commandNativeDiskFailure{owner: o, cause: cause}
	}
	return nil
}
