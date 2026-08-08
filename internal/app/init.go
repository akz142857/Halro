package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type InitializationState string

const (
	InitializationEmpty        InitializationState = "empty"
	InitializationSystemReady  InitializationState = "system_ready"
	InitializationInconsistent InitializationState = "inconsistent"
)

// InspectInitialization distinguishes a new instance from an initialized one
// without modifying state or initializing a KMS Adapter. Key-Slot mode opens
// metadata read-only so file presence alone can never imply production-ready.
func InspectInitialization(cfg config.Config) (InitializationState, error) {
	masterKeyExists := false
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeFile {
		keyStore, err := fileMasterKeyStore(cfg)
		if err != nil {
			return InitializationInconsistent, err
		}
		masterKeyExists, err = keyStore.Exists(context.Background())
		if err != nil {
			return InitializationInconsistent, err
		}
	} else if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return InitializationInconsistent, errors.New("unsupported Master Key mode")
	}
	metadataExists, err := pathExists(cfg.MetadataPath())
	if err != nil {
		return InitializationInconsistent, err
	}
	ledgerExists, err := pathExists(cfg.LedgerPath())
	if err != nil {
		return InitializationInconsistent, err
	}
	auditExists, err := pathExists(cfg.AuditPath())
	if err != nil {
		return InitializationInconsistent, err
	}
	rootExists := masterKeyExists
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		rootExists = metadataExists
	}
	if rootExists && metadataExists && ledgerExists && auditExists {
		if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
			metadata, err := boltstore.OpenReadOnly(cfg.MetadataPath())
			if err != nil {
				return InitializationInconsistent, nil
			}
			descriptor, descriptorErr := metadata.KeySlotDescriptor(context.Background())
			keyring, keyringErr := metadata.VaultKeyring()
			_, keyCheckErr := metadata.VaultKeyCheck()
			_, auditEnvelopeErr := metadata.AuditHMACEnvelope()
			_, auditCheckpointErr := metadata.AuditCheckpoint()
			closeErr := metadata.Close()
			if descriptorErr != nil || keyringErr != nil || keyCheckErr != nil || auditEnvelopeErr != nil || auditCheckpointErr != nil || closeErr != nil ||
				!descriptor.ProductionReady() || keyring.ActiveFingerprint != descriptor.MasterKeyFingerprint {
				return InitializationInconsistent, nil
			}
		}
		return InitializationSystemReady, nil
	}
	if rootExists || metadataExists || ledgerExists || auditExists {
		return InitializationInconsistent, nil
	}
	entries, err := os.ReadDir(cfg.Storage.DataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return InitializationInconsistent, fmt.Errorf("inspect data directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != ".halro.lock" {
			return InitializationInconsistent, nil
		}
	}
	return InitializationEmpty, nil
}

// InitializeIfNeeded initializes only a provably empty instance. Partial state
// is never repaired or overwritten automatically.
func InitializeIfNeeded(cfg config.Config) (bool, error) {
	state, err := InspectInitialization(cfg)
	if err != nil {
		return false, err
	}
	switch state {
	case InitializationSystemReady:
		return false, nil
	case InitializationInconsistent:
		return false, errors.New("Halro initialization is incomplete; restore the matching master key and data directory or move the partial state aside")
	case InitializationEmpty:
		if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
			return false, errors.New("empty key_slots instance requires explicit offline `halro init`; automatic Runtime initialization is disabled")
		}
		if err := Initialize(cfg); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("unknown initialization state %q", state)
	}
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect %s: %w", path, err)
}

const (
	vaultKeyCheckID        = "system:key-check"
	vaultKeyCheckProvider  = "system"
	vaultKeyCheckAudience  = "halro:metadata"
	vaultKeyCheckPlaintext = "halro-key-check-v1"
)

func Initialize(cfg config.Config) error {
	switch cfg.Storage.MasterKey.Mode {
	case config.MasterKeyModeFile:
		return initializeFile(cfg)
	case config.MasterKeyModeKeySlots:
		return initializeKMS(context.Background(), cfg, kmsInitializationOptions{})
	default:
		return errors.New("unsupported Master Key mode")
	}
}

func initializeFile(cfg config.Config) error {
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()

	keyStore, err := fileMasterKeyStore(cfg)
	if err != nil {
		return err
	}
	if err := keyStore.Initialize(context.Background()); err != nil {
		return err
	}
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return fmt.Errorf("initialize metadata: %w", err)
	}
	masterKey, err := keyStore.Unlock(context.Background())
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
	ledgerKey, err := vault.DeriveLedgerHMACKey(masterKey)
	if err != nil {
		clear(masterKey)
		clear(auditKey)
		metadata.Close()
		return err
	}
	masterKeyFingerprint := keyFingerprint(masterKey)
	secretVault, err := vault.New(masterKey)
	clear(masterKey)
	if err != nil {
		clear(auditKey)
		clear(ledgerKey)
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
		clear(ledgerKey)
		metadata.Close()
		return fmt.Errorf("create vault key check: %w", err)
	}
	auditEnvelope, err := encryptAuditHMACKey(secretVault, auditKey)
	if err != nil {
		secretVault.Close()
		clear(auditKey)
		clear(ledgerKey)
		metadata.Close()
		return fmt.Errorf("protect audit HMAC key: %w", err)
	}
	ledgerEnvelope, err := encryptLedgerHMACKey(secretVault, ledgerKey)
	secretVault.Close()
	if err != nil {
		clear(auditKey)
		clear(ledgerKey)
		metadata.Close()
		return fmt.Errorf("protect ledger HMAC key: %w", err)
	}
	if err := metadata.PutVaultKeyCheck(envelope); err != nil {
		clear(auditKey)
		clear(ledgerKey)
		metadata.Close()
		return fmt.Errorf("store vault key check: %w", err)
	}
	if err := metadata.PutAuditHMACEnvelope(auditEnvelope); err != nil {
		clear(auditKey)
		clear(ledgerKey)
		metadata.Close()
		return fmt.Errorf("store audit HMAC envelope: %w", err)
	}
	if err := metadata.PutLedgerHMACEnvelope(ledgerEnvelope); err != nil {
		clear(auditKey)
		clear(ledgerKey)
		metadata.Close()
		return fmt.Errorf("store ledger HMAC envelope: %w", err)
	}
	if err := metadata.PutVaultKeyring(boltstore.VaultKeyring{
		FormatVersion: 1, ActiveKeyVersion: 1, ActiveFingerprint: masterKeyFingerprint,
	}); err != nil {
		clear(auditKey)
		clear(ledgerKey)
		metadata.Close()
		return fmt.Errorf("store vault keyring: %w", err)
	}
	if err := metadata.PutAuditCheckpoint(boltstore.AuditCheckpoint{}); err != nil {
		clear(auditKey)
		clear(ledgerKey)
		metadata.Close()
		return fmt.Errorf("store initial audit checkpoint: %w", err)
	}
	ledgerLog, ledgerErr := ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{ChainKey: ledgerKey})
	clear(ledgerKey)
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
