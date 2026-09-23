package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/config"
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

	metadataJournalKeyID       = "system:metadata-journal-hmac"
	metadataJournalKeyProvider = "system"
	metadataJournalKeyAudience = "halro:metadata:v1"

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

func encryptMetadataJournalHMACKey(secretVault *vault.Vault, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("metadata journal HMAC key must be 32 bytes")
	}
	return secretVault.EncryptCredential(
		metadataJournalKeyID, metadataJournalKeyProvider, metadataJournalKeyAudience, key,
	)
}

// loadMetadataJournalHMACKey mirrors loadLedgerHMACKey, with one deliberate
// difference: there is no rotation guard on its bootstrap fallback.
//
// That guard exists for the Audit and Ledger keys because deriving after a
// rotation yields a key that never signed anything, and the chain then reports
// every historical frame as tampered — a missing envelope reading as an attack.
// The journal cannot fail that way. It is authenticated in full the moment it
// is opened, so a derived key that never signed it produces ErrCorrupt at the
// first frame rather than a plausible-looking misreading, and an instance that
// predates the journal has no frames to invalidate at all: attachMetadataJournal
// seeds the envelope and starts epoch 1.
//
// The envelope is class D and travels with the metadata for the reason that
// class exists: a node that cannot authenticate the journal cannot apply it,
// and a node that cannot apply it is not a projection of anything.
func loadMetadataJournalHMACKey(store *boltstore.Store, secretVault *vault.Vault, masterKey []byte) ([]byte, error) {
	envelope, err := store.MetadataJournalHMACEnvelope()
	if errors.Is(err, boltstore.ErrNotFound) {
		return vault.DeriveMetadataJournalHMACKey(masterKey)
	}
	if err != nil {
		return nil, fmt.Errorf("load metadata journal HMAC envelope: %w", err)
	}
	key, err := secretVault.DecryptCredential(
		metadataJournalKeyID, metadataJournalKeyProvider, metadataJournalKeyAudience, envelope,
	)
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, errors.New("metadata journal HMAC envelope does not authenticate")
	}
	return key, nil
}

// attachMetadataJournal brings the metadata projection level with its journal.
//
// Every path that opens the metadata store and writes authoritative state calls
// it, because the store refuses to commit a write that nothing would record.
// Paths that only touch node-derived state — `ledger seal` advancing a
// checkpoint, `usage rebuild-summary` rebuilding a rollup — need no journal and
// are not asked to attach one: their writes are class C and record nothing by
// construction.
//
// reason is written into the epoch header when this attach publishes a new
// epoch, so an operator reading a journal can see what replaced the one before.
func attachMetadataJournal(
	store *boltstore.Store, secretVault *vault.Vault, masterKey []byte, reason string,
) (boltstore.JournalState, error) {
	_, envelopeErr := store.MetadataJournalHMACEnvelope()
	key, err := loadMetadataJournalHMACKey(store, secretVault, masterKey)
	if err != nil {
		return boltstore.JournalState{}, err
	}
	defer clear(key)
	state, err := store.AttachMetadataJournal(key, reason)
	if err != nil {
		return state, fmt.Errorf("attach metadata journal: %w", err)
	}
	if errors.Is(envelopeErr, boltstore.ErrNotFound) {
		// A data directory that predates the journal. The key was derived just
		// now and the epoch it opened has no history, so sealing it is safe —
		// and it has to happen through the entry, after the attach, because
		// the envelope is itself a recorded write.
		sealed, sealErr := encryptMetadataJournalHMACKey(secretVault, key)
		if sealErr != nil {
			return state, fmt.Errorf("protect metadata journal HMAC key: %w", sealErr)
		}
		if err := store.PutMetadataJournalHMACEnvelope(sealed); err != nil &&
			!errors.Is(err, boltstore.ErrAlreadyExists) {
			return state, fmt.Errorf("store metadata journal HMAC envelope: %w", err)
		}
	} else if envelopeErr != nil {
		return state, envelopeErr
	}
	return state, nil
}

// attachMetadataJournalForCLI is the attach an offline command makes before it
// writes. It unwraps the Master Key the way every other command does, so the
// caller does not have to hold one just to record its own writes.
//
// The design is explicit that no bbolt write bypasses the journal, offline
// commands included (§6.1.4): a data directory edited by `halro admin` or
// `halro key` between two starts must be describable by the same log a running
// instance writes.
func attachMetadataJournalForCLI(
	ctx context.Context, cfg config.Config, store *boltstore.Store, reason string,
) error {
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	_, err = attachMetadataJournal(store, secretVault, masterKey, reason)
	return err
}
