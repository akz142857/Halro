package app

import (
	"errors"
	"fmt"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/ledger"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/vault"
)

const (
	vaultKeyCheckID        = "system:key-check"
	vaultKeyCheckProvider  = "system"
	vaultKeyCheckAudience  = "heimdall:metadata"
	vaultKeyCheckPlaintext = "heimdall-key-check-v1"
)

func Initialize(cfg config.Config) error {
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()

	if err := vault.InitMasterKey(cfg.Storage.MasterKeyFile); err != nil {
		return err
	}
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return fmt.Errorf("initialize metadata: %w", err)
	}
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKeyFile)
	if err != nil {
		metadata.Close()
		return err
	}
	auditKey, err := vault.DeriveAuditHMACKey(masterKey)
	if err != nil {
		clear(masterKey)
		metadata.Close()
		return err
	}
	masterKeyFingerprint := keyFingerprint(masterKey)
	secretVault, err := vault.New(masterKey)
	clear(masterKey)
	if err != nil {
		clear(auditKey)
		metadata.Close()
		return err
	}
	envelope, err := secretVault.EncryptCredential(
		vaultKeyCheckID,
		vaultKeyCheckProvider,
		vaultKeyCheckAudience,
		[]byte(vaultKeyCheckPlaintext),
	)
	if err != nil {
		secretVault.Close()
		clear(auditKey)
		metadata.Close()
		return fmt.Errorf("create vault key check: %w", err)
	}
	auditEnvelope, err := encryptAuditHMACKey(secretVault, auditKey)
	secretVault.Close()
	if err != nil {
		clear(auditKey)
		metadata.Close()
		return fmt.Errorf("protect audit HMAC key: %w", err)
	}
	if err := metadata.PutVaultKeyCheck(envelope); err != nil {
		clear(auditKey)
		metadata.Close()
		return fmt.Errorf("store vault key check: %w", err)
	}
	if err := metadata.PutAuditHMACEnvelope(auditEnvelope); err != nil {
		clear(auditKey)
		metadata.Close()
		return fmt.Errorf("store audit HMAC envelope: %w", err)
	}
	if err := metadata.PutVaultKeyring(boltstore.VaultKeyring{
		FormatVersion: 1, ActiveKeyVersion: 1, ActiveFingerprint: masterKeyFingerprint,
	}); err != nil {
		clear(auditKey)
		metadata.Close()
		return fmt.Errorf("store vault keyring: %w", err)
	}
	if err := metadata.PutAuditCheckpoint(boltstore.AuditCheckpoint{}); err != nil {
		clear(auditKey)
		metadata.Close()
		return fmt.Errorf("store initial audit checkpoint: %w", err)
	}
	ledgerLog, ledgerErr := ledger.Open(cfg.LedgerPath(), ledger.NewStatus())
	auditLog, auditErr := audit.Open(cfg.AuditPath(), auditKey)
	clear(auditKey)
	return errors.Join(ledgerErr, func() error {
		if ledgerLog != nil {
			return ledgerLog.Close()
		}
		return nil
	}(), auditErr, func() error {
		if auditLog != nil {
			return auditLog.Close()
		}
		return nil
	}(), metadata.Close())
}
