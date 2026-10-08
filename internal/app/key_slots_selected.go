package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/masterkey"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// Only a DEK-preserving slot operation receives this private authority. It
// carries the independent source purpose, never a caller-provided descriptor.
type selectedSlotMaintenanceKey struct{}
type selectedSlotMaintenance struct {
	sourcePurpose masterkey.KeySlotPurpose
	factory       kmsWrapperFactory
}

type selectedUnlockRetirement struct {
	store *boltstore.Store
	cause error
}

func (e *selectedUnlockRetirement) Error() string { return e.cause.Error() }
func (e *selectedUnlockRetirement) Unwrap() error { return e.cause }

// The legacy database remains the pre-bootstrap unlock source. Copy only the
// actual selected descriptor, after checking the same unchanged DEK, through
// its original native journal. Publishing a selected common checkpoint cannot
// race this offline operation because the original directory guard is held.
func syncSelectedUnlockDescriptor(ctx context.Context, cfg config.Config, state *runtimeSelectedState, guard *lock.Lock, operation *selectedSlotMaintenance) (resultErr error) {
	if state == nil || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) || cfg.Replication != nil || operation == nil || operation.factory == nil {
		return errors.New("selected unlock synchronization requires its original Standalone generation and offline lock")
	}
	old, err := boltstore.OpenWithMappingBudget(ctx, cfg.MetadataPath(), metadataLegacyFileCeiling(cfg), state.generation.Owners.Budget)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := old.Close(); closeErr != nil {
			resultErr = &selectedUnlockRetirement{store: old, cause: errors.Join(resultErr, closeErr)}
		}
	}()
	key, err := unlockKMSMasterKey(ctx, cfg, old, operation.sourcePurpose, operation.factory)
	if err != nil {
		return err
	}
	defer clear(key)
	v, err := vault.New(key)
	if err != nil {
		return err
	}
	defer v.Close()
	if err := verifyVaultKeyCheck(state.services.Metadata, v); err != nil {
		return err
	}
	if _, err := attachConfiguredMetadataJournal(old, v, key, "selected key slot unlock synchronization", cfg); err != nil {
		return err
	}
	return old.CopyKeySlotUnlockDescriptorFromSelected(ctx, state.services.Metadata)
}
