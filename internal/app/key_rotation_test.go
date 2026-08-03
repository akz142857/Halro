package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/adminauth"
	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/vault"
)

func TestMasterKeyRotationReencryptsCredentialsAndPreservesAuditChain(t *testing.T) {
	cfg, newKeyFile, credentialID, providerSecret, oldKey, oldAuditKey := rotationFixture(t)
	defer clear(oldKey)
	defer clear(oldAuditKey)

	beforeStore, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	fixtureVault, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	mfaSecret := []byte("12345678901234567890")
	mfaCiphertext, err := fixtureVault.EncryptAdminMFA("mfa_rotation", "admin", mfaSecret)
	fixtureVault.Close()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = beforeStore.PutAdminMFAAuthenticator(context.Background(), domain.AdminMFAAuthenticator{ID: "mfa_rotation", Username: "admin", Name: "rotation phone", Type: domain.AdminMFATypeTOTP, SecretCiphertext: mfaCiphertext, Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &now}, 0); err != nil {
		t.Fatal(err)
	}
	beforeUser, err := adminauth.NewUser("admin", []byte("correct horse battery staple"), now)
	if err != nil {
		t.Fatal(err)
	}
	beforeUser, err = beforeStore.PutAdminUser(context.Background(), beforeUser, 0)
	if err != nil {
		t.Fatal(err)
	}
	challengeHash := sha256.Sum256([]byte("rotation-challenge"))
	if err = beforeStore.PutAdminMFAChallenge(context.Background(), domain.AdminMFAChallenge{IDHash: challengeHash, Username: "admin", Purpose: domain.AdminMFAChallengeLogin, CreatedAt: now, ExpiresAt: now.Add(time.Minute), AttemptsRemaining: 5, SessionGeneration: beforeUser.SessionGeneration}); err != nil {
		t.Fatal(err)
	}
	beforeCredential, err := beforeStore.GetCredential(context.Background(), credentialID)
	if closeErr := beforeStore.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	retiredCiphertext := []byte(base64.StdEncoding.EncodeToString(beforeCredential.Ciphertext))
	result, err := RotateMasterKey(context.Background(), cfg, newKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if result.Credentials != 1 || result.RecoveredPending || result.OldKeyVersion != 1 || result.NewKeyVersion != 2 {
		t.Fatalf("rotation result=%#v", result)
	}
	newKey, err := vault.LoadMasterKey(newKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(newKey)
	activeKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(activeKey, newKey) {
		t.Fatal("active master key was not atomically replaced")
	}
	clear(activeKey)

	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	newVault, err := vault.New(newKey)
	if err != nil {
		t.Fatal(err)
	}
	defer newVault.Close()
	if err := verifyVaultKeyCheck(store, newVault); err != nil {
		t.Fatal(err)
	}
	credential, err := store.GetCredential(context.Background(), credentialID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := newVault.DecryptCredential(
		credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plaintext, providerSecret) || credential.KeyVersion != 2 {
		t.Fatalf("rotated credential version=%d plaintext_match=%t", credential.KeyVersion, bytes.Equal(plaintext, providerSecret))
	}
	clear(plaintext)
	rotatedMFA, err := store.GetAdminMFAAuthenticator(context.Background(), "admin", "mfa_rotation")
	if err != nil {
		t.Fatal(err)
	}
	mfaPlaintext, err := newVault.DecryptAdminMFA(rotatedMFA.ID, rotatedMFA.Username, rotatedMFA.SecretCiphertext)
	if err != nil || !bytes.Equal(mfaPlaintext, mfaSecret) {
		t.Fatalf("rotated MFA secret invalid: %v", err)
	}
	clear(mfaPlaintext)
	oldVault, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	defer oldVault.Close()
	if err := verifyVaultKeyCheck(store, oldVault); err == nil {
		t.Fatal("retired master key still authenticates cleaned metadata")
	}
	if _, err := store.VaultRotationBridge(); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("rotation bridge survived successful cleanup: %v", err)
	}
	if _, err := store.GetAdminSession(context.Background(), sha256.Sum256([]byte("rotation-session"))); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("admin session survived master key rotation: %v", err)
	}
	afterUser, err := store.GetAdminUser(context.Background(), "admin")
	if err != nil || afterUser.SessionGeneration != beforeUser.SessionGeneration+1 {
		t.Fatalf("rotation generation=%d err=%v", afterUser.SessionGeneration, err)
	}
	if _, err := store.GetAdminMFAChallenge(context.Background(), challengeHash); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("MFA challenge survived rotation: %v", err)
	}
	protectedAuditKey, err := loadAuditHMACKey(store, newVault, newKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(protectedAuditKey, oldAuditKey) {
		t.Fatal("audit HMAC key changed during master key rotation")
	}
	clear(protectedAuditKey)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Verify(cfg.AuditPath(), oldAuditKey); err != nil {
		t.Fatalf("historical audit chain no longer verifies: %v", err)
	}
	if _, err := VerifyAudit(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	rawMetadata, err := os.ReadFile(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawMetadata, retiredCiphertext) {
		t.Fatal("compacted metadata retained retired credential ciphertext")
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("runtime did not start with rotated key: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMasterKeyRotationRecoversFromEveryPublicationKillPoint(t *testing.T) {
	points := []string{
		"after_started_audit",
		"after_metadata_snapshot",
		"after_rewrite_verification",
		"before_metadata_publish",
		"after_metadata_publish",
		"before_master_key_publish",
		"after_master_key_publish",
		"before_bridge_cleanup_publish",
		"after_bridge_cleanup_publish",
	}
	for _, point := range points {
		point := point
		t.Run(point, func(t *testing.T) {
			cfg, newKeyFile, credentialID, providerSecret, oldKey, oldAuditKey := rotationFixture(t)
			defer clear(oldKey)
			defer clear(oldAuditKey)
			injected := errors.New("injected process death")
			var retiredBridge []byte
			_, err := rotateMasterKeyWithHook(context.Background(), cfg, newKeyFile, func(current string) error {
				if current == point {
					if current == "after_metadata_publish" {
						store, openErr := boltstore.Open(cfg.MetadataPath())
						if openErr != nil {
							return openErr
						}
						retiredBridge, openErr = store.VaultRotationBridge()
						if closeErr := store.Close(); openErr == nil {
							openErr = closeErr
						}
						if openErr != nil {
							return openErr
						}
					}
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("kill point %s returned %v", point, err)
			}
			result, err := RotateMasterKey(context.Background(), cfg, newKeyFile)
			if err != nil {
				t.Fatalf("recover %s: %v", point, err)
			}
			if point == "after_metadata_publish" || point == "before_master_key_publish" ||
				point == "after_master_key_publish" || point == "before_bridge_cleanup_publish" {
				if !result.RecoveredPending {
					t.Fatalf("kill point %s was not reported as pending recovery", point)
				}
			}
			assertCompletedRotation(t, cfg, newKeyFile, credentialID, providerSecret, oldAuditKey)
			if len(retiredBridge) > 0 {
				raw, err := os.ReadFile(cfg.MetadataPath())
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(raw, retiredBridge) {
					t.Fatal("compacted metadata retained the retired rotation bridge")
				}
			}
		})
	}
}

func TestMasterKeyRotationAdvancesPersistentKeyringAcrossRotations(t *testing.T) {
	cfg, secondKeyFile, credentialID, _, oldKey, oldAuditKey := rotationFixture(t)
	defer clear(oldKey)
	defer clear(oldAuditKey)
	first, err := RotateMasterKey(context.Background(), cfg, secondKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	thirdKeyFile := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "third-master.key")
	if err := vault.InitMasterKey(thirdKeyFile); err != nil {
		t.Fatal(err)
	}
	second, err := RotateMasterKey(context.Background(), cfg, thirdKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if first.OldKeyVersion != 1 || first.NewKeyVersion != 2 ||
		second.OldKeyVersion != 2 || second.NewKeyVersion != 3 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keyring, err := store.VaultKeyring()
	if err != nil {
		t.Fatal(err)
	}
	thirdKey, err := vault.LoadMasterKey(thirdKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(thirdKey)
	secondKey, err := vault.LoadMasterKey(secondKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(secondKey)
	if keyring.ActiveKeyVersion != 3 || keyring.ActiveFingerprint != keyFingerprint(thirdKey) ||
		keyring.PreviousFingerprint != keyFingerprint(secondKey) || len(keyring.RecoveryEnvelope) != 0 {
		t.Fatalf("keyring=%#v", keyring)
	}
	credential, err := store.GetCredential(context.Background(), credentialID)
	if err != nil || credential.KeyVersion != 3 {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	protectedAuditKey, err := loadAuditHMACKey(store, mustVault(t, thirdKey), thirdKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(protectedAuditKey)
	if !bytes.Equal(protectedAuditKey, oldAuditKey) {
		t.Fatal("audit key changed across repeated master key rotations")
	}
}

func mustVault(t *testing.T, key []byte) *vault.Vault {
	t.Helper()
	value, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(value.Close)
	return value
}

func rotationFixture(t *testing.T) (config.Config, string, string, []byte, []byte, []byte) {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	providerSecret := []byte("rotation-provider-secret")
	_, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Rotation",
	}, providerSecret)
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := store.ListCredentials(context.Background())
	if err != nil || len(credentials) != 1 {
		store.Close()
		t.Fatalf("credentials=%d err=%v", len(credentials), err)
	}
	now := time.Now().UTC()
	session := domain.AdminSession{
		IDHash: sha256.Sum256([]byte("rotation-session")), Username: "admin", Generation: 1,
		CreatedAt: now, LastSeenAt: now, AbsoluteExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(time.Hour),
	}
	if err := store.PutAdminSession(context.Background(), session); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	oldKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	oldAuditKey, err := vault.DeriveAuditHMACKey(oldKey)
	if err != nil {
		clear(oldKey)
		t.Fatal(err)
	}
	newKeyFile := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "new-master.key")
	if err := vault.InitMasterKey(newKeyFile); err != nil {
		clear(oldKey)
		clear(oldAuditKey)
		t.Fatal(err)
	}
	return cfg, newKeyFile, credentials[0].ID, providerSecret, oldKey, oldAuditKey
}

func assertCompletedRotation(t *testing.T, cfg config.Config, newKeyFile, credentialID string, providerSecret, auditKey []byte) {
	t.Helper()
	newKey, err := vault.LoadMasterKey(newKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(newKey)
	active, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(active, newKey) {
		clear(active)
		t.Fatal("recovered active master key differs from requested key")
	}
	clear(active)
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secretVault, err := vault.New(newKey)
	if err != nil {
		t.Fatal(err)
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		t.Fatal(err)
	}
	credential, err := store.GetCredential(context.Background(), credentialID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := secretVault.DecryptCredential(
		credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plaintext, providerSecret) {
		clear(plaintext)
		t.Fatal("credential plaintext changed during recovery")
	}
	clear(plaintext)
	if _, err := store.VaultRotationBridge(); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("rotation bridge remains after recovery: %v", err)
	}
	if _, err := audit.Verify(cfg.AuditPath(), auditKey); err != nil {
		t.Fatalf("audit chain failed after recovery: %v", err)
	}
}

func TestReplaceMasterKeyRejectsSamePathAtAppBoundary(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateMasterKey(context.Background(), cfg, cfg.Storage.MasterKey.File); err == nil {
		t.Fatal("same master key path was accepted")
	}
	if _, err := os.Stat(cfg.Storage.MasterKey.File); err != nil {
		t.Fatal(err)
	}
}
