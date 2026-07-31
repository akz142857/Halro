package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/vault"
)

type KeyRotationResult struct {
	OldFingerprint   string `json:"old_fingerprint"`
	NewFingerprint   string `json:"new_fingerprint"`
	OldKeyVersion    uint64 `json:"old_key_version"`
	NewKeyVersion    uint64 `json:"new_key_version"`
	Credentials      int    `json:"credentials"`
	RecoveredPending bool   `json:"recovered_pending"`
}

func RotateMasterKey(ctx context.Context, cfg config.Config, newKeyFile string) (KeyRotationResult, error) {
	return rotateMasterKeyWithHook(ctx, cfg, newKeyFile, nil)
}

func rotateMasterKeyWithHook(
	ctx context.Context,
	cfg config.Config,
	newKeyFile string,
	hook func(string) error,
) (KeyRotationResult, error) {
	if err := ctx.Err(); err != nil {
		return KeyRotationResult{}, err
	}
	currentPath, err := filepath.Abs(cfg.Storage.MasterKeyFile)
	if err != nil {
		return KeyRotationResult{}, err
	}
	newPath, err := filepath.Abs(newKeyFile)
	if err != nil {
		return KeyRotationResult{}, err
	}
	if currentPath == newPath {
		return KeyRotationResult{}, errors.New("new master key file must differ from the active master key file")
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return KeyRotationResult{}, fmt.Errorf("acquire offline key rotation lock: %w", err)
	}
	defer dataLock.Close()

	currentKey, err := vault.LoadMasterKey(currentPath)
	if err != nil {
		return KeyRotationResult{}, err
	}
	defer clear(currentKey)
	newKey, err := vault.LoadMasterKey(newPath)
	if err != nil {
		return KeyRotationResult{}, fmt.Errorf("load new master key: %w", err)
	}
	defer clear(newKey)
	result := KeyRotationResult{
		OldFingerprint: keyFingerprint(currentKey), NewFingerprint: keyFingerprint(newKey),
	}
	currentVault, err := vault.New(currentKey)
	if err != nil {
		return KeyRotationResult{}, err
	}
	defer currentVault.Close()
	newVault, err := vault.New(newKey)
	if err != nil {
		return KeyRotationResult{}, err
	}
	defer newVault.Close()

	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return KeyRotationResult{}, err
	}
	currentAuthenticates := verifyVaultKeyCheck(metadata, currentVault) == nil
	newAuthenticates := verifyVaultKeyCheck(metadata, newVault) == nil
	keysEqual := bytes.Equal(currentKey, newKey)
	keyring, keyringErr := metadata.VaultKeyring()
	if errors.Is(keyringErr, boltstore.ErrNotFound) {
		keyring = boltstore.VaultKeyring{FormatVersion: 1, ActiveKeyVersion: 1}
		if currentAuthenticates {
			keyring.ActiveFingerprint = keyFingerprint(currentKey)
		} else if newAuthenticates {
			keyring.ActiveFingerprint = keyFingerprint(newKey)
		}
		keyringErr = keyring.Validate()
	}
	if keyringErr != nil {
		metadata.Close()
		return KeyRotationResult{}, keyringErr
	}
	if (currentAuthenticates && keyring.ActiveFingerprint != keyFingerprint(currentKey)) ||
		(newAuthenticates && keyring.ActiveFingerprint != keyFingerprint(newKey)) {
		metadata.Close()
		return KeyRotationResult{}, errors.New("vault keyring active fingerprint does not match authenticated metadata")
	}
	result.OldKeyVersion = keyring.ActiveKeyVersion
	result.NewKeyVersion = keyring.ActiveKeyVersion
	_, bridgeErr := metadata.VaultRotationBridge()
	bridgePresent := bridgeErr == nil
	if bridgeErr != nil && !errors.Is(bridgeErr, boltstore.ErrNotFound) {
		metadata.Close()
		return KeyRotationResult{}, bridgeErr
	}

	if currentAuthenticates && keysEqual {
		credentials, listErr := metadata.ListCredentials(ctx)
		result.Credentials = len(credentials)
		metadata.Close()
		if listErr != nil {
			return KeyRotationResult{}, listErr
		}
		if !bridgePresent {
			return result, nil
		}
		result.RecoveredPending = true
		if err := finalizeMasterKeyRotation(ctx, cfg, newVault, newKey, hook); err != nil {
			return KeyRotationResult{}, err
		}
		return result, nil
	}

	if !currentAuthenticates {
		if keysEqual || !newAuthenticates {
			metadata.Close()
			return KeyRotationResult{}, errors.New("neither active nor requested new master key authenticates metadata")
		}
		if err := verifyRotationBridge(metadata, currentVault, newKey); err != nil {
			metadata.Close()
			return KeyRotationResult{}, err
		}
		credentials, listErr := metadata.ListCredentials(ctx)
		result.Credentials = len(credentials)
		metadata.Close()
		if listErr != nil {
			return KeyRotationResult{}, listErr
		}
		result.RecoveredPending = true
		if err := callRotationHook(hook, "before_master_key_publish"); err != nil {
			return KeyRotationResult{}, err
		}
		if err := vault.ReplaceMasterKey(currentPath, newKey); err != nil {
			return KeyRotationResult{}, err
		}
		if err := callRotationHook(hook, "after_master_key_publish"); err != nil {
			return KeyRotationResult{}, err
		}
		if err := finalizeMasterKeyRotation(ctx, cfg, newVault, newKey, hook); err != nil {
			return KeyRotationResult{}, err
		}
		return result, nil
	}

	if keysEqual || newAuthenticates {
		metadata.Close()
		return KeyRotationResult{}, errors.New("master key rotation state is inconsistent")
	}
	if keyring.ActiveKeyVersion == ^uint64(0) {
		metadata.Close()
		return KeyRotationResult{}, errors.New("vault keyring version is exhausted")
	}
	credentials, err := metadata.ListCredentials(ctx)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	result.Credentials = len(credentials)
	auditKey, err := loadAuditHMACKey(metadata, currentVault, currentKey)
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	defer clear(auditKey)
	if err := appendRotationAudit(metadata, cfg.AuditPath(), auditKey, "security.master_key_rotation.started"); err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(hook, "after_started_audit"); err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}

	stagePath, err := newMetadataStagePath(cfg.Storage.DataDir, "rewrite")
	if err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	defer os.Remove(stagePath)
	if _, err := metadata.Snapshot(stagePath); err != nil {
		metadata.Close()
		return KeyRotationResult{}, err
	}
	if err := metadata.Close(); err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(hook, "after_metadata_snapshot"); err != nil {
		return KeyRotationResult{}, err
	}
	stage, err := boltstore.Open(stagePath)
	if err != nil {
		return KeyRotationResult{}, err
	}
	keyCheck, err := newVault.EncryptCredential(
		vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, []byte(vaultKeyCheckPlaintext),
	)
	if err != nil {
		stage.Close()
		return KeyRotationResult{}, err
	}
	auditEnvelope, err := encryptAuditHMACKey(newVault, auditKey)
	if err != nil {
		stage.Close()
		return KeyRotationResult{}, err
	}
	bridge, err := encryptRotationBridge(currentVault, newKey)
	if err != nil {
		stage.Close()
		return KeyRotationResult{}, err
	}
	err = stage.RewriteVaultMaterial(boltstore.VaultRewrite{
		VaultKeyCheck: keyCheck, AuditHMACEnvelope: auditEnvelope,
		Keyring: boltstore.VaultKeyring{
			FormatVersion: 1, ActiveKeyVersion: keyring.ActiveKeyVersion + 1,
			ActiveFingerprint: keyFingerprint(newKey), PreviousFingerprint: keyFingerprint(currentKey),
			RecoveryEnvelope: bridge,
		},
		Transform: func(credential domain.Credential) (domain.Credential, error) {
			plaintext, err := currentVault.DecryptCredential(
				credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext,
			)
			if err != nil {
				return domain.Credential{}, err
			}
			defer clear(plaintext)
			ciphertext, err := newVault.EncryptCredential(
				credential.ID, string(credential.Type), credential.Audience, plaintext,
			)
			if err != nil {
				return domain.Credential{}, err
			}
			if credential.KeyVersion == ^uint16(0) {
				return domain.Credential{}, errors.New("credential key version is exhausted")
			}
			credential.Ciphertext = ciphertext
			credential.KeyVersion++
			return credential, nil
		},
	})
	compactPath, compactPathErr := newMetadataStagePath(cfg.Storage.DataDir, "compact")
	if compactPathErr != nil && err == nil {
		err = compactPathErr
	}
	if compactPath != "" {
		defer os.Remove(compactPath)
	}
	if err == nil {
		err = stage.CompactSnapshot(compactPath)
	}
	if closeErr := stage.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return KeyRotationResult{}, err
	}
	result.NewKeyVersion = keyring.ActiveKeyVersion + 1
	compacted, err := boltstore.Open(compactPath)
	if err != nil {
		return KeyRotationResult{}, err
	}
	err = verifyRotatedMetadata(ctx, compacted, newVault, currentVault, newKey, auditKey, true)
	if closeErr := compacted.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(hook, "after_rewrite_verification"); err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(hook, "before_metadata_publish"); err != nil {
		return KeyRotationResult{}, err
	}
	if err := publishMetadata(compactPath, cfg.MetadataPath()); err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(hook, "after_metadata_publish"); err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(hook, "before_master_key_publish"); err != nil {
		return KeyRotationResult{}, err
	}
	if err := vault.ReplaceMasterKey(currentPath, newKey); err != nil {
		return KeyRotationResult{}, err
	}
	if err := callRotationHook(hook, "after_master_key_publish"); err != nil {
		return KeyRotationResult{}, err
	}
	if err := finalizeMasterKeyRotation(ctx, cfg, newVault, newKey, hook); err != nil {
		return KeyRotationResult{}, err
	}
	return result, nil
}

func finalizeMasterKeyRotation(ctx context.Context, cfg config.Config, newVault *vault.Vault, newKey []byte, hook func(string) error) error {
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return err
	}
	if err := verifyVaultKeyCheck(metadata, newVault); err != nil {
		metadata.Close()
		return err
	}
	auditKey, err := loadAuditHMACKey(metadata, newVault, newKey)
	if err != nil {
		metadata.Close()
		return err
	}
	defer clear(auditKey)
	if err := appendRotationAudit(metadata, cfg.AuditPath(), auditKey, "security.master_key_rotation.completed"); err != nil {
		metadata.Close()
		return err
	}
	stagePath, err := newMetadataStagePath(cfg.Storage.DataDir, "cleanup")
	if err != nil {
		metadata.Close()
		return err
	}
	defer os.Remove(stagePath)
	if _, err := metadata.Snapshot(stagePath); err != nil {
		metadata.Close()
		return err
	}
	if err := metadata.Close(); err != nil {
		return err
	}
	stage, err := boltstore.Open(stagePath)
	if err != nil {
		return err
	}
	if err := stage.ClearVaultRotationBridge(); err != nil {
		stage.Close()
		return err
	}
	compactPath, err := newMetadataStagePath(cfg.Storage.DataDir, "cleanup-compact")
	if err == nil {
		err = stage.CompactSnapshot(compactPath)
	}
	if closeErr := stage.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if compactPath != "" {
			os.Remove(compactPath)
		}
		return err
	}
	defer os.Remove(compactPath)
	compacted, err := boltstore.Open(compactPath)
	if err != nil {
		return err
	}
	err = verifyRotatedMetadata(ctx, compacted, newVault, nil, newKey, auditKey, false)
	if closeErr := compacted.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := callRotationHook(hook, "before_bridge_cleanup_publish"); err != nil {
		return err
	}
	if err := publishMetadata(compactPath, cfg.MetadataPath()); err != nil {
		return err
	}
	return callRotationHook(hook, "after_bridge_cleanup_publish")
}

func verifyRotatedMetadata(ctx context.Context, store *boltstore.Store, newVault, oldVault *vault.Vault, newKey, auditKey []byte, expectBridge bool) error {
	if err := verifyVaultKeyCheck(store, newVault); err != nil {
		return err
	}
	keyring, err := store.VaultKeyring()
	if err != nil {
		return err
	}
	if keyring.ActiveFingerprint != keyFingerprint(newKey) {
		return fmt.Errorf("rotated keyring fingerprint %s does not match new master key %s", keyring.ActiveFingerprint, keyFingerprint(newKey))
	}
	if expectBridge != (len(keyring.RecoveryEnvelope) > 0) {
		return errors.New("rotated keyring recovery state is inconsistent")
	}
	credentials, err := store.ListCredentials(ctx)
	if err != nil {
		return err
	}
	for _, credential := range credentials {
		plaintext, err := newVault.DecryptCredential(
			credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext,
		)
		clear(plaintext)
		if err != nil {
			return fmt.Errorf("verify rotated credential %q: %w", credential.ID, err)
		}
	}
	loadedAuditKey, err := loadAuditHMACKey(store, newVault, newKey)
	if err != nil {
		return err
	}
	defer clear(loadedAuditKey)
	if !bytes.Equal(loadedAuditKey, auditKey) {
		return errors.New("rotated audit HMAC key changed unexpectedly")
	}
	if expectBridge {
		return verifyRotationBridge(store, oldVault, newKey)
	}
	if _, err := store.VaultRotationBridge(); !errors.Is(err, boltstore.ErrNotFound) {
		return errors.New("vault rotation bridge survived cleanup")
	}
	return nil
}

func appendRotationAudit(store *boltstore.Store, path string, key []byte, action string) error {
	log, err := audit.Open(path, key)
	if err != nil {
		return err
	}
	defer log.Close()
	if err := reconcileAuditCheckpoint(store, log.Summary()); err != nil {
		return err
	}
	return appendSystemAudit(log, store, action)
}

func newMetadataStagePath(dataDir, phase string) (string, error) {
	file, err := os.CreateTemp(dataDir, ".heimdall-key-rotate-"+phase+"-*.db")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func publishMetadata(stagePath, livePath string) error {
	if filepath.Dir(stagePath) != filepath.Dir(livePath) {
		return errors.New("metadata stage must share the live metadata directory")
	}
	if err := os.Rename(stagePath, livePath); err != nil {
		return fmt.Errorf("publish rotated metadata: %w", err)
	}
	return syncDirectoryPath(filepath.Dir(livePath))
}

func callRotationHook(hook func(string) error, point string) error {
	if hook == nil {
		return nil
	}
	if err := hook(point); err != nil {
		return fmt.Errorf("master key rotation interrupted at %s: %w", point, err)
	}
	return nil
}

func keyFingerprint(key []byte) string {
	digest := sha256.Sum256(key)
	return "sha256:" + hex.EncodeToString(digest[:])
}
