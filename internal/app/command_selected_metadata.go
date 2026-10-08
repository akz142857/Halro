package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type commandDataLockKey struct{}

// Uncertain descriptor retirement keeps the actual process exclusion alive.
// The error owns this guard; returning an error must not expose surviving
// writers to a second offline command or Runtime.
type commandDirectoryLockFailure struct {
	guard *lock.Lock
	cause error
}

func (e *commandDirectoryLockFailure) Error() string { return e.cause.Error() }
func (e *commandDirectoryLockFailure) Unwrap() error { return e.cause }

func finishCommandDataLock(guard *lock.Lock, cause error) error {
	if errors.Is(cause, ownedfile.ErrRetirement) || errors.Is(cause, ledger.ErrSegmentRetirement) || errors.Is(cause, boltstore.ErrMetadataFileRetirement) {
		return &commandDirectoryLockFailure{guard: guard, cause: cause}
	}
	return errors.Join(cause, guard.Close())
}

func withCommandDataLock(ctx context.Context, guard *lock.Lock) context.Context {
	return context.WithValue(ctx, commandDataLockKey{}, guard)
}

type selectedCommandMetadataBackend struct {
	boltstore.FacadeBackend
	closeOnce       sync.Once
	closeErr        error
	publishOnClose  bool
	closing         bool // guarded by scope.selectedMu; blocks new native loans
	state           *runtimeSelectedState
	guard           *lock.Lock
	cfg             config.Config
	scope           *commandResourceScope
	native          *commandNativeDiskOwner
	slotMaintenance *selectedSlotMaintenance
}

func (b *selectedCommandMetadataBackend) Close() error {
	b.closeOnce.Do(func() {
		b.scope.selectedMu.Lock()
		b.closing = true
		for _, loan := range b.scope.selectedLoans {
			if loan != nil {
				b.closeErr = errors.Join(b.closeErr, loan.Close())
			}
		}
		b.scope.selectedMu.Unlock()
		if b.closeErr == nil && b.slotMaintenance != nil {
			b.closeErr = syncSelectedUnlockDescriptor(context.Background(), b.cfg, b.state, b.guard, b.slotMaintenance)
		}
		// The outer Store has already canceled and joined its whole DAO lifetime.
		// The selected services have no command-external producers; retire their
		// actual tickets and original DAO before publishing the common successor.
		b.state.services.Accounting.WithdrawAdmission()
		b.closeErr = errors.Join(b.closeErr, b.state.services.Governance.Retire(context.Background()), b.state.services.Accounting.Retire(context.Background()), b.state.services.Metadata.RetireFacade(context.Background()))
		if b.closeErr == nil && b.publishOnClose {
			b.closeErr = checkpointDrainedSelectedState(context.Background(), b.cfg, b.state)
		}
		b.closeErr = errors.Join(b.closeErr, b.state.close())
		b.closeErr = b.native.finish(b.closeErr)
		if b.closeErr == nil {
			b.scope.selectedMu.Lock()
			b.scope.selected = nil
			b.scope.selectedMu.Unlock()
		}
	})
	return b.closeErr
}

func openSelectedCommandMetadata(ctx context.Context, cfg config.Config, path string) (result *boltstore.Store, resultErr error) {
	if err := checkSelectedRotationPending(cfg, nil); err != nil {
		return nil, err
	}
	guard, _ := ctx.Value(commandDataLockKey{}).(*lock.Lock)
	if guard == nil && keyScope(ctx) != nil {
		guard, _ = keyScope(ctx).lock.(*lock.Lock)
	}
	slotMaintenance, _ := ctx.Value(selectedSlotMaintenanceKey{}).(*selectedSlotMaintenance)
	scope, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) || scope == nil || cfg.ResourceBudget == nil || cfg.Replication != nil || path != cfg.MetadataPath() {
		return nil, errors.New("selected command Metadata requires the original offline lock, Standalone configuration and shared budget")
	}
	if keyScope(ctx) != nil && slotMaintenance == nil {
		return nil, errors.New("whole-file key maintenance requires explicit memory-state generation migration")
	}
	scope.selectedMu.Lock()
	occupied := scope.selected != nil || scope.selectedOpening || scope.selectedReader != nil
	if !occupied {
		scope.selectedOpening = true
	}
	scope.selectedMu.Unlock()
	if occupied {
		return nil, errors.New("selected command already owns its Metadata generation")
	}
	defer func() { scope.selectedMu.Lock(); scope.selectedOpening = false; scope.selectedMu.Unlock() }()
	old, err := boltstore.OpenReadOnlyWithMappingBudget(ctx, path, metadataLegacyFileCeiling(cfg), scope.pressure.logicalBudget())
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, old.Close()) }()
	var masterKey []byte
	if slotMaintenance != nil {
		masterKey, err = unlockKMSMasterKey(ctx, cfg, old, slotMaintenance.sourcePurpose, slotMaintenance.factory)
	} else {
		masterKey, err = unlockMasterKey(ctx, cfg, old)
	}
	if err != nil {
		return nil, err
	}
	defer clear(masterKey)
	v, err := vault.New(masterKey)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	if err := verifyVaultKeyCheck(old, v); err != nil {
		return nil, err
	}
	if err := old.Close(); err != nil {
		return nil, err
	}
	native, err := prepareCommandNativeDisk(ctx, cfg, guard)
	if err != nil {
		return nil, err
	}
	state, err := tryOpenSelectedRuntimeState(ctx, cfg, v, guard, scope.pressure, native.growth, nil)
	if err != nil {
		return nil, err
	}
	backend := &selectedCommandMetadataBackend{state: state, guard: guard, cfg: cfg, scope: scope, native: native, slotMaintenance: slotMaintenance}
	scope.selectedMu.Lock()
	scope.selected = backend
	scope.selectedMu.Unlock()
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, backend.Close())
		}
	}()
	if err := state.generation.WaitStartupAudit(ctx); err != nil {
		return nil, err
	}
	reference, err := state.generation.Owners.Metadata.SealReference(ctx)
	if err != nil {
		return nil, err
	}
	backend.FacadeBackend, err = state.generation.Owners.Metadata.StandaloneFacade(ctx, scope.pressure.logicalBudget(), state.bootstrap.Authority.Instance, state.bootstrap.InstanceID, state.bootstrap.KeyGeneration, reference)
	if err != nil {
		return nil, err
	}
	_, selectedJournalPath := memoryStateMetadataPaths(cfg, state.bootstrap.MetadataSlot)
	if filepath.Clean(backend.JournalPath()) != selectedJournalPath {
		return nil, errors.New("selected command Metadata journal path changed")
	}
	result, err = boltstore.NewFacadeStore(ctx, backend)
	if err == nil {
		if configureErr := configurePricingResources(cfg, scope.pressure.logicalBudget(), result); configureErr != nil {
			err = errors.Join(configureErr, result.Close())
			result = nil
		}
	}
	backend.publishOnClose = err == nil
	if err == nil && keyScope(ctx) != nil {
		if trackErr := keyScope(ctx).trackCloser(result); trackErr != nil {
			err = errors.Join(trackErr, result.Close())
			result = nil
		}
	}
	return result, err
}
