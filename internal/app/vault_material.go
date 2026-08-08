package app

import (
	"bytes"
	"errors"
	"fmt"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

const (
	auditKeyID       = "system:audit-hmac"
	auditKeyProvider = "system"
	auditKeyAudience = "halro:audit:v1"

	ledgerKeyID       = "system:ledger-hmac"
	ledgerKeyProvider = "system"
	ledgerKeyAudience = "halro:ledger:v1"

	rotationBridgeID       = "system:master-key-rotation"
	rotationBridgeProvider = "system"
	rotationBridgeAudience = "halro:master-key-rotation:v1"
)

func encryptAuditHMACKey(secretVault *vault.Vault, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("audit HMAC key must be 32 bytes")
	}
	return secretVault.EncryptCredential(
		auditKeyID, auditKeyProvider, auditKeyAudience, key,
	)
}

// requireUnrotatedForDerivedKey guards the bootstrap fallback that derives a
// chain key straight from the Master Key when no envelope is stored.
//
// That fallback is correct exactly once: before the first rotation, deriving
// reproduces the same bytes the envelope would have held. Rotation re-wraps
// the envelope without changing the key inside it, so after one has happened,
// deriving from the current Master Key yields a key that never signed
// anything. Falling back there does not fail — it succeeds with the wrong key
// and reports every historical frame as tampered, which reads as an attack on
// the log rather than a missing envelope. Refusing to derive turns "the
// envelope is gone" back into something an operator can act on.
func requireUnrotatedForDerivedKey(store *boltstore.Store, name string) error {
	keyring, err := store.VaultKeyring()
	if errors.Is(err, boltstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load vault keyring: %w", err)
	}
	if keyring.ActiveKeyVersion > 1 {
		return fmt.Errorf(
			"%s HMAC envelope is missing on an instance whose Master Key has been rotated (key version %d); "+
				"restore the metadata database from backup rather than starting with a re-derived key",
			name, keyring.ActiveKeyVersion,
		)
	}
	return nil
}

func loadAuditHMACKey(store *boltstore.Store, secretVault *vault.Vault, masterKey []byte) ([]byte, error) {
	envelope, err := store.AuditHMACEnvelope()
	if errors.Is(err, boltstore.ErrNotFound) {
		if err := requireUnrotatedForDerivedKey(store, "audit"); err != nil {
			return nil, err
		}
		return vault.DeriveAuditHMACKey(masterKey)
	}
	if err != nil {
		return nil, fmt.Errorf("load audit HMAC envelope: %w", err)
	}
	key, err := secretVault.DecryptCredential(
		auditKeyID, auditKeyProvider, auditKeyAudience, envelope,
	)
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, errors.New("audit HMAC envelope does not authenticate")
	}
	return key, nil
}

func encryptLedgerHMACKey(secretVault *vault.Vault, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("ledger HMAC key must be 32 bytes")
	}
	return secretVault.EncryptCredential(
		ledgerKeyID, ledgerKeyProvider, ledgerKeyAudience, key,
	)
}

func loadLedgerHMACKey(store *boltstore.Store, secretVault *vault.Vault, masterKey []byte) ([]byte, error) {
	envelope, err := store.LedgerHMACEnvelope()
	if errors.Is(err, boltstore.ErrNotFound) {
		if err := requireUnrotatedForDerivedKey(store, "ledger"); err != nil {
			return nil, err
		}
		return vault.DeriveLedgerHMACKey(masterKey)
	}
	if err != nil {
		return nil, fmt.Errorf("load ledger HMAC envelope: %w", err)
	}
	key, err := secretVault.DecryptCredential(
		ledgerKeyID, ledgerKeyProvider, ledgerKeyAudience, envelope,
	)
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, errors.New("ledger HMAC envelope does not authenticate")
	}
	return key, nil
}

func encryptRotationBridge(oldVault *vault.Vault, newMasterKey []byte) ([]byte, error) {
	if len(newMasterKey) != vault.MasterKeySize {
		return nil, errors.New("new master key must be 32 bytes")
	}
	return oldVault.EncryptCredential(
		rotationBridgeID, rotationBridgeProvider, rotationBridgeAudience, newMasterKey,
	)
}

func verifyRotationBridge(store *boltstore.Store, oldVault *vault.Vault, expectedNewKey []byte) error {
	envelope, err := store.VaultRotationBridge()
	if err != nil {
		return fmt.Errorf("load vault rotation bridge: %w", err)
	}
	key, err := oldVault.DecryptCredential(
		rotationBridgeID, rotationBridgeProvider, rotationBridgeAudience, envelope,
	)
	if err != nil {
		return errors.New("vault rotation bridge does not authenticate")
	}
	defer clear(key)
	if !bytes.Equal(key, expectedNewKey) {
		return errors.New("vault rotation bridge targets a different master key")
	}
	return nil
}
