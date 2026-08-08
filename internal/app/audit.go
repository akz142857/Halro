package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

func VerifyAudit(ctx context.Context, cfg config.Config) (audit.Summary, error) {
	if err := ctx.Err(); err != nil {
		return audit.Summary{}, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return audit.Summary{}, err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return audit.Summary{}, err
	}
	defer store.Close()
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return audit.Summary{}, err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return audit.Summary{}, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return audit.Summary{}, err
	}
	auditKey, err := loadAuditHMACKey(store, secretVault, masterKey)
	if err != nil {
		return audit.Summary{}, err
	}
	defer clear(auditKey)
	summary, err := audit.Verify(cfg.AuditPath(), auditKey)
	if err != nil {
		return audit.Summary{}, err
	}
	checkpoint, err := store.AuditCheckpoint()
	if err != nil {
		return audit.Summary{}, fmt.Errorf("load audit checkpoint: %w", err)
	}
	if checkpoint.Records != summary.Records || checkpoint.Bytes != summary.Bytes ||
		checkpoint.LastHash != summary.LastHash {
		return audit.Summary{}, errors.New("audit log does not match its trusted checkpoint")
	}
	return summary, nil
}
