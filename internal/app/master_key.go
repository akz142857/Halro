package app

import (
	"context"
	"errors"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/vault"
)

func unlockMasterKey(ctx context.Context, cfg config.Config, stores ...*boltstore.Store) ([]byte, error) {
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeFile {
		unlocker, err := masterkey.NewUnlocker(cfg.Storage.MasterKey)
		if err != nil {
			return nil, err
		}
		return unlocker.Unlock(ctx)
	}
	if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return nil, errors.New("unsupported Master Key mode")
	}
	if len(stores) > 1 {
		return nil, errors.New("at most one metadata store may be supplied")
	}
	if len(stores) == 1 && stores[0] != nil {
		return unlockKMSMasterKey(ctx, cfg, stores[0], masterkey.KeySlotPrimary, defaultKMSWrapperFactory)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return unlockKMSMasterKey(ctx, cfg, store, masterkey.KeySlotPrimary, defaultKMSWrapperFactory)
}

func fileMasterKeyStore(cfg config.Config) (*masterkey.FileStore, error) {
	return masterkey.FileStoreFromConfig(cfg.Storage.MasterKey)
}

// vaultCandidateVerifier makes the encrypted Vault Key Check the final trust
// anchor for every candidate Master Key. A matching fingerprint is only a fast
// rejection mechanism and is never sufficient to activate a Key Slot.
type vaultCandidateVerifier struct {
	store *boltstore.Store
}

func (v vaultCandidateVerifier) VerifyCandidate(ctx context.Context, candidate []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.store == nil {
		return errors.New("metadata store is required")
	}
	secretVault, err := vault.New(candidate)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	return verifyVaultKeyCheck(v.store, secretVault)
}
