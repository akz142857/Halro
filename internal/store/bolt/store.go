package bolt

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

const schemaVersion uint64 = 8

func CurrentSchemaVersion() uint64 { return schemaVersion }

var (
	ErrNotFound         = errors.New("record not found")
	ErrAlreadyExists    = errors.New("record already exists")
	ErrRevisionConflict = errors.New("record revision conflict")
	ErrKeyHashConflict  = errors.New("gateway key hash already exists")
	ErrCredentialInUse  = errors.New("credential is still referenced")
	ErrAdminInitialized = errors.New("an admin user already exists")
	ErrMFARequired      = errors.New("MFA is required")
	ErrMFALimit         = errors.New("MFA authenticator limit reached")
	ErrMFAClaimed       = errors.New("MFA challenge is already claimed")
	errStopIteration    = errors.New("stop iteration")
)

var (
	bucketMeta                   = []byte("meta")
	bucketCredentials            = []byte("credentials")
	bucketProjects               = []byte("projects")
	bucketGatewayKeys            = []byte("gateway_keys")
	bucketGatewayKeyHash         = []byte("gateway_key_hash")
	bucketProviders              = []byte("providers")
	bucketDeployments            = []byte("deployments")
	bucketRoutes                 = []byte("routes")
	bucketRedactionPolicies      = []byte("redaction_policies")
	bucketTokenGuardPolicies     = []byte("token_guard_policies")
	bucketAlertWebhooks          = []byte("alert_webhooks")
	bucketAdminUsers             = []byte("admin_users")
	bucketAdminSessions          = []byte("admin_sessions")
	bucketAdminMFAAuthenticators = []byte("admin_mfa_authenticators")
	bucketAdminMFARecoveryCodes  = []byte("admin_mfa_recovery_codes")
	bucketAdminMFAChallenges     = []byte("admin_mfa_challenges")
	bucketMigrationHistory       = []byte("migration_history")
	bucketProviderResources      = []byte("provider_resources")
	keySchemaVersion             = []byte("schema_version")
	keyVaultCheck                = []byte("vault_key_check")
	keyUsageCheckpoint           = []byte("usage_checkpoint")
	keyTokenGuardCheckpoint      = []byte("token_guard_checkpoint")
	keyAuditCheckpoint           = []byte("audit_checkpoint")
	keyAuditHMACEnvelope         = []byte("audit_hmac_envelope")
	keyVaultKeyring              = []byte("vault_keyring")
	keyRuntimeSettings           = []byte("runtime_settings")
	keyInstanceUISettings        = []byte("instance_ui_settings")
)

type MigrationRecord struct {
	Version uint64 `json:"version"`
	Name    string `json:"name"`
}

type migration struct {
	version uint64
	name    string
	up      func(*bbolt.Tx, func(string) error) error
}

var migrations = []migration{
	{version: 1, name: "initial_schema", up: createInitialBuckets},
	{version: 2, name: "migration_history", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_create_migration_history"); err != nil {
			return err
		}
		history, err := tx.CreateBucketIfNotExists(bucketMigrationHistory)
		if err != nil {
			return err
		}
		if err := migrationStep(step, "after_create_migration_history"); err != nil {
			return err
		}
		record, err := json.Marshal(MigrationRecord{Version: 1, Name: "initial_schema"})
		if err != nil {
			return err
		}
		if err := migrationStep(step, "before_seed_migration_history"); err != nil {
			return err
		}
		if err := history.Put(versionKey(1), record); err != nil {
			return err
		}
		return migrationStep(step, "after_seed_migration_history")
	}},
	{version: 3, name: "deployments", up: migrateDeployments},
	{version: 4, name: "provider_profiles", up: migrateProviderProfiles},
	{version: 5, name: "provider_resources", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_create_provider_resources"); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketProviderResources); err != nil {
			return err
		}
		return migrationStep(step, "after_create_provider_resources")
	}},
	{version: 6, name: "phase2_capability_evidence", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_phase2_capability_evidence"); err != nil {
			return err
		}
		for _, bucketName := range [][]byte{bucketProviders, bucketDeployments} {
			if err := rewriteBucket(tx.Bucket(bucketName), func(raw []byte) ([]byte, error) {
				if bytes.Equal(bucketName, bucketProviders) {
					var value domain.ProviderInstance
					if err := json.Unmarshal(raw, &value); err != nil {
						return nil, err
					}
					value.CapabilityEvidence = domain.NormalizeCapabilityEvidence(value.Capabilities, value.CapabilityEvidence, domain.EvidenceLegacy)
					return json.Marshal(value)
				}
				var value domain.Deployment
				if err := json.Unmarshal(raw, &value); err != nil {
					return nil, err
				}
				value.CapabilityEvidence = domain.NormalizeCapabilityEvidence(value.Capabilities, value.CapabilityEvidence, domain.EvidenceLegacy)
				return json.Marshal(value)
			}); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_phase2_capability_evidence")
	}},
	{version: 7, name: "provider_resource_creation_status", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_provider_resource_creation_status"); err != nil {
			return err
		}
		if err := rewriteBucket(tx.Bucket(bucketProviderResources), func(raw []byte) ([]byte, error) {
			var value domain.ProviderResource
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, err
			}
			if value.CreationStatus == "" {
				if value.UpstreamID == "" {
					value.CreationStatus = "unknown"
				} else {
					value.CreationStatus = "completed"
				}
			}
			return json.Marshal(value)
		}); err != nil {
			return err
		}
		return migrationStep(step, "after_provider_resource_creation_status")
	}},
	{version: 8, name: "admin_mfa", up: func(tx *bbolt.Tx, step func(string) error) error {
		for _, name := range [][]byte{bucketAdminMFAAuthenticators, bucketAdminMFARecoveryCodes, bucketAdminMFAChallenges} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_create_admin_mfa_buckets")
	}},
}

type usageCheckpoint struct {
	Watermark ledger.Watermark `json:"watermark"`
	Payload   []byte           `json:"payload"`
}

type AuditCheckpoint struct {
	Records  uint64   `json:"records"`
	Bytes    int64    `json:"bytes"`
	LastHash [32]byte `json:"last_hash"`
}

type Store struct {
	db *bbolt.DB
}

type MetadataInfo struct {
	SchemaVersion uint64 `json:"schema_version"`
	TxID          uint64 `json:"txid"`
}

type BootstrapRecords struct {
	Credential domain.Credential
	Provider   domain.ProviderInstance
	Deployment domain.Deployment
	Route      domain.Route
	Project    domain.Project
	GatewayKey domain.GatewayKey
}

func (s *Store) RuntimeSettings() (domain.RuntimeSettings, error) {
	var settings domain.RuntimeSettings
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyRuntimeSettings)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("decode runtime settings: %w", err)
		}
		return settings.Validate()
	})
	return settings, err
}

func (s *Store) PutRuntimeSettings(settings domain.RuntimeSettings, expectedRevision uint64) (domain.RuntimeSettings, error) {
	if err := settings.Validate(); err != nil {
		return domain.RuntimeSettings{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		currentRevision := uint64(0)
		if raw := bucket.Get(keyRuntimeSettings); raw != nil {
			var current domain.RuntimeSettings
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode runtime settings: %w", err)
			}
			currentRevision = current.Revision
		}
		if currentRevision != expectedRevision {
			return ErrRevisionConflict
		}
		settings.Revision = currentRevision + 1
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		return bucket.Put(keyRuntimeSettings, encoded)
	})
	return settings, err
}

func (s *Store) InstanceUISettings() (domain.InstanceUISettings, error) {
	var settings domain.InstanceUISettings
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyInstanceUISettings)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("decode instance UI settings: %w", err)
		}
		return settings.Validate()
	})
	return settings, err
}

func (s *Store) PutInstanceUISettings(settings domain.InstanceUISettings, expectedRevision uint64) (domain.InstanceUISettings, error) {
	if err := settings.Validate(); err != nil {
		return domain.InstanceUISettings{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		currentRevision := uint64(0)
		if raw := bucket.Get(keyInstanceUISettings); raw != nil {
			var current domain.InstanceUISettings
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode instance UI settings: %w", err)
			}
			currentRevision = current.Revision
		}
		if currentRevision != expectedRevision {
			return ErrRevisionConflict
		}
		settings.Revision = currentRevision + 1
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		return bucket.Put(keyInstanceUISettings, encoded)
	})
	return settings, err
}

func Open(path string) (*Store, error) {
	return openWithMigrationHooks(path, nil, nil)
}

// OpenReadOnly opens existing metadata without creating files or running
// migrations. It is intended for diagnostics that must never mutate state.
func OpenReadOnly(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open metadata read-only: %w", err)
	}
	store := &Store{db: db}
	version, err := store.SchemaVersion()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("read metadata schema: %w", err)
	}
	if version != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("metadata schema version %d does not match required version %d", version, schemaVersion)
	}
	return store, nil
}

func openWithMigrationHook(path string, afterUp func(uint64) error) (*Store, error) {
	return openWithMigrationHooks(path, afterUp, nil)
}

func openWithMigrationStepHook(path string, stepHook func(uint64, string) error) (*Store, error) {
	return openWithMigrationHooks(path, nil, stepHook)
}

func openWithMigrationHooks(path string, afterUp func(uint64) error, stepHook func(uint64, string) error) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create metadata directory: %w", err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{
		Timeout:      2 * time.Second,
		FreelistType: bbolt.FreelistMapType,
	})
	if err != nil {
		return nil, fmt.Errorf("open metadata: %w", err)
	}
	store := &Store{db: db}
	if err := store.initialize(afterUp, stepHook); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) PutVaultKeyCheck(value []byte) error {
	if len(value) == 0 {
		return errors.New("vault key check cannot be empty")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyVaultCheck) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyVaultCheck, value)
	})
}

func (s *Store) VaultKeyCheck() ([]byte, error) {
	var value []byte
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyVaultCheck)
		if raw == nil {
			return ErrNotFound
		}
		value = bytes.Clone(raw)
		return nil
	})
	return value, err
}

func (s *Store) PutAuditHMACEnvelope(value []byte) error {
	if len(value) == 0 {
		return errors.New("audit HMAC envelope cannot be empty")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyAuditHMACEnvelope) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyAuditHMACEnvelope, value)
	})
}

func (s *Store) AuditHMACEnvelope() ([]byte, error) {
	return s.metaBytes(keyAuditHMACEnvelope)
}

type VaultKeyring struct {
	FormatVersion       uint8  `json:"format_version"`
	ActiveKeyVersion    uint64 `json:"active_key_version"`
	ActiveFingerprint   string `json:"active_fingerprint"`
	PreviousFingerprint string `json:"previous_fingerprint,omitempty"`
	RecoveryEnvelope    []byte `json:"recovery_envelope,omitempty"`
}

func (k VaultKeyring) Validate() error {
	if k.FormatVersion != 1 || k.ActiveKeyVersion == 0 ||
		!validKeyFingerprint(k.ActiveFingerprint) ||
		(k.PreviousFingerprint != "" && !validKeyFingerprint(k.PreviousFingerprint)) {
		return errors.New("vault keyring is invalid")
	}
	return nil
}

func validKeyFingerprint(value string) bool {
	if len(value) != len("sha256:")+64 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func (s *Store) PutVaultKeyring(keyring VaultKeyring) error {
	if err := keyring.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(keyring)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyVaultKeyring) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyVaultKeyring, encoded)
	})
}

func (s *Store) VaultKeyring() (VaultKeyring, error) {
	var keyring VaultKeyring
	raw, err := s.metaBytes(keyVaultKeyring)
	if err != nil {
		return keyring, err
	}
	if err := json.Unmarshal(raw, &keyring); err != nil {
		return keyring, fmt.Errorf("decode vault keyring: %w", err)
	}
	return keyring, keyring.Validate()
}

func (s *Store) VaultRotationBridge() ([]byte, error) {
	keyring, err := s.VaultKeyring()
	if err != nil {
		return nil, err
	}
	if len(keyring.RecoveryEnvelope) == 0 {
		return nil, ErrNotFound
	}
	return bytes.Clone(keyring.RecoveryEnvelope), nil
}

func (s *Store) metaBytes(key []byte) ([]byte, error) {
	var value []byte
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(key)
		if raw == nil {
			return ErrNotFound
		}
		value = bytes.Clone(raw)
		return nil
	})
	return value, err
}

type VaultRewrite struct {
	VaultKeyCheck     []byte
	AuditHMACEnvelope []byte
	Keyring           VaultKeyring
	Transform         func(domain.Credential) (domain.Credential, error)
	TransformAdminMFA func(domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error)
}

// RewriteVaultMaterial runs only against an offline copy of metadata. All
// credential ciphertext and system envelopes change in one bbolt transaction.
func (s *Store) RewriteVaultMaterial(options VaultRewrite) error {
	if len(options.VaultKeyCheck) == 0 || len(options.AuditHMACEnvelope) == 0 ||
		options.Transform == nil || options.TransformAdminMFA == nil || options.Keyring.Validate() != nil ||
		len(options.Keyring.RecoveryEnvelope) == 0 {
		return errors.New("complete vault rewrite material is required")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		credentials := tx.Bucket(bucketCredentials)
		cursor := credentials.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			var credential domain.Credential
			if err := json.Unmarshal(raw, &credential); err != nil {
				return fmt.Errorf("decode credential %q: %w", key, err)
			}
			updated, err := options.Transform(credential)
			if err != nil {
				return fmt.Errorf("rewrite credential %q: %w", key, err)
			}
			if updated.ID != credential.ID || updated.Revision != credential.Revision {
				return errors.New("vault rewrite cannot change credential identity or revision")
			}
			if err := updated.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(updated)
			if err != nil {
				return err
			}
			if err := credentials.Put(key, encoded); err != nil {
				return err
			}
		}
		authenticators := tx.Bucket(bucketAdminMFAAuthenticators)
		mfaCursor := authenticators.Cursor()
		for key, raw := mfaCursor.First(); key != nil; key, raw = mfaCursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			updated, err := options.TransformAdminMFA(value)
			if err != nil {
				return err
			}
			if updated.ID != value.ID || updated.Username != value.Username || updated.Revision != value.Revision {
				return errors.New("vault rewrite cannot change MFA identity or revision")
			}
			if err = updated.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(updated)
			if err != nil {
				return err
			}
			if err = authenticators.Put(key, encoded); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		if err := meta.Put(keyVaultCheck, options.VaultKeyCheck); err != nil {
			return err
		}
		if err := meta.Put(keyAuditHMACEnvelope, options.AuditHMACEnvelope); err != nil {
			return err
		}
		encodedKeyring, err := json.Marshal(options.Keyring)
		if err != nil {
			return err
		}
		if err := meta.Put(keyVaultKeyring, encodedKeyring); err != nil {
			return err
		}
		// Master-key rotation invalidates every active Admin identity and
		// pre-auth challenge in the same transaction as the ciphertext rewrite.
		users := tx.Bucket(bucketAdminUsers)
		userCursor := users.Cursor()
		for key, raw := userCursor.First(); key != nil; key, raw = userCursor.Next() {
			var user domain.AdminUser
			if err := json.Unmarshal(raw, &user); err != nil {
				return err
			}
			user.SessionGeneration++
			user.Revision++
			user.UpdatedAt = time.Now().UTC()
			encoded, err := json.Marshal(user)
			if err != nil {
				return err
			}
			if err := users.Put(key, encoded); err != nil {
				return err
			}
		}
		sessions := tx.Bucket(bucketAdminSessions)
		sessionCursor := sessions.Cursor()
		for key, _ := sessionCursor.First(); key != nil; key, _ = sessionCursor.Next() {
			if err := sessionCursor.Delete(); err != nil {
				return err
			}
		}
		challenges := tx.Bucket(bucketAdminMFAChallenges)
		challengeCursor := challenges.Cursor()
		for key, _ := challengeCursor.First(); key != nil; key, _ = challengeCursor.Next() {
			if err := challengeCursor.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ListAllAdminMFAAuthenticators(ctx context.Context) ([]domain.AdminMFAAuthenticator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var values []domain.AdminMFAAuthenticator
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminMFAAuthenticators).ForEach(func(_, raw []byte) error {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			values = append(values, value)
			return nil
		})
	})
	return values, err
}

func (s *Store) ClearVaultRotationBridge() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyVaultKeyring)
		if raw == nil {
			return ErrNotFound
		}
		var keyring VaultKeyring
		if err := json.Unmarshal(raw, &keyring); err != nil {
			return err
		}
		if err := keyring.Validate(); err != nil {
			return err
		}
		keyring.RecoveryEnvelope = nil
		encoded, err := json.Marshal(keyring)
		if err != nil {
			return err
		}
		return meta.Put(keyVaultKeyring, encoded)
	})
}

func (s *Store) PutUsageCheckpoint(watermark ledger.Watermark, payload []byte) error {
	if watermark.Sequence == 0 || watermark.Offset <= 0 || watermark.Generation != 1 {
		return errors.New("usage checkpoint watermark is invalid")
	}
	if len(payload) == 0 {
		return errors.New("usage checkpoint payload cannot be empty")
	}
	encoded, err := json.Marshal(usageCheckpoint{Watermark: watermark, Payload: bytes.Clone(payload)})
	if err != nil {
		return fmt.Errorf("encode usage checkpoint envelope: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if current := meta.Get(keyUsageCheckpoint); current != nil {
			var saved usageCheckpoint
			if err := json.Unmarshal(current, &saved); err != nil {
				return fmt.Errorf("decode current usage checkpoint: %w", err)
			}
			if watermark.Sequence < saved.Watermark.Sequence {
				return errors.New("usage checkpoint watermark cannot move backwards")
			}
		}
		return meta.Put(keyUsageCheckpoint, encoded)
	})
}

func (s *Store) UsageCheckpoint() (ledger.Watermark, []byte, error) {
	var saved usageCheckpoint
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyUsageCheckpoint)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &saved); err != nil {
			return fmt.Errorf("decode usage checkpoint envelope: %w", err)
		}
		return nil
	})
	if err != nil {
		return ledger.Watermark{}, nil, err
	}
	if saved.Watermark.Sequence == 0 || saved.Watermark.Offset <= 0 ||
		saved.Watermark.Generation != 1 || len(saved.Payload) == 0 {
		return ledger.Watermark{}, nil, errors.New("usage checkpoint is invalid")
	}
	return saved.Watermark, bytes.Clone(saved.Payload), nil
}

// DeleteUsageCheckpoint removes only the rebuildable aggregate accelerator.
// The Ledger remains authoritative and the next startup replays it from zero.
func (s *Store) DeleteUsageCheckpoint() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Delete(keyUsageCheckpoint)
	})
}

func (s *Store) PutTokenGuardCheckpoint(payload []byte) error {
	if len(payload) == 0 || len(payload) > 64<<20 {
		return errors.New("Token Guard checkpoint payload is invalid")
	}
	copyPayload := bytes.Clone(payload)
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyTokenGuardCheckpoint, copyPayload)
	})
}

func (s *Store) TokenGuardCheckpoint() ([]byte, error) {
	var payload []byte
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyTokenGuardCheckpoint)
		if raw == nil {
			return ErrNotFound
		}
		payload = bytes.Clone(raw)
		return nil
	})
	return payload, err
}

func (s *Store) DeleteTokenGuardCheckpoint() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Delete(keyTokenGuardCheckpoint)
	})
}

func (s *Store) PutAuditCheckpoint(checkpoint AuditCheckpoint) error {
	if checkpoint.Bytes < 0 || (checkpoint.Records == 0 && (checkpoint.Bytes != 0 ||
		checkpoint.LastHash != [32]byte{})) ||
		(checkpoint.Records > 0 && (checkpoint.Bytes == 0 || checkpoint.LastHash == [32]byte{})) {
		return errors.New("audit checkpoint is invalid")
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(keyAuditCheckpoint); raw != nil {
			var current AuditCheckpoint
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode current audit checkpoint: %w", err)
			}
			if checkpoint.Records < current.Records {
				return errors.New("audit checkpoint cannot move backwards")
			}
			if checkpoint.Records == current.Records && checkpoint != current {
				return errors.New("audit checkpoint conflicts at the same sequence")
			}
		}
		return meta.Put(keyAuditCheckpoint, encoded)
	})
}

func (s *Store) AuditCheckpoint() (AuditCheckpoint, error) {
	var checkpoint AuditCheckpoint
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyAuditCheckpoint)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			return fmt.Errorf("decode audit checkpoint: %w", err)
		}
		return nil
	})
	return checkpoint, err
}

// PutBootstrap writes the first usable provider, route, project, and key in one
// bbolt transaction so a failure cannot leave a half-configured gateway.
func (s *Store) PutBootstrap(ctx context.Context, records *BootstrapRecords) error {
	if records == nil {
		return errors.New("bootstrap records are required")
	}
	// Keep programmatic bootstrap callers compatible with records created before
	// provider profiles became durable metadata. The public bootstrap path writes
	// declared evidence explicitly; omitted metadata is conservatively legacy.
	normalizeCredentialProfile(&records.Credential)
	normalizeProviderProfile(&records.Provider, domain.EvidenceLegacy)
	normalizeDeploymentProfile(&records.Deployment, records.Provider, domain.EvidenceLegacy)
	if err := errors.Join(
		records.Credential.Validate(),
		records.Provider.Validate(),
		records.Deployment.Validate(),
		records.Route.Validate(),
		records.Project.Validate(),
		records.GatewayKey.Validate(),
	); err != nil {
		return err
	}
	if records.Provider.CredentialID != records.Credential.ID ||
		records.Deployment.ProviderID != records.Provider.ID ||
		records.Route.DeploymentID != records.Deployment.ID ||
		records.GatewayKey.ProjectID != records.Project.ID {
		return errors.New("bootstrap references are inconsistent")
	}
	if err := validateProviderCredentialProfile(records.Provider, records.Credential); err != nil {
		return err
	}
	if err := validateDeploymentProviderProfile(records.Deployment, records.Provider); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketGatewayKeyHash).Get(records.GatewayKey.KeyHash[:]) != nil {
			return ErrKeyHashConflict
		}
		for _, operation := range []func() error{
			func() error {
				return putVersioned(tx.Bucket(bucketCredentials), records.Credential.ID, 0, &records.Credential)
			},
			func() error {
				return putVersioned(tx.Bucket(bucketProviders), records.Provider.ID, 0, &records.Provider)
			},
			func() error {
				return putVersioned(tx.Bucket(bucketDeployments), records.Deployment.ID, 0, &records.Deployment)
			},
			func() error {
				return putVersioned(tx.Bucket(bucketRoutes), records.Route.ID, 0, &records.Route)
			},
			func() error {
				return putVersioned(tx.Bucket(bucketProjects), records.Project.ID, 0, &records.Project)
			},
			func() error {
				return putVersioned(tx.Bucket(bucketGatewayKeys), records.GatewayKey.ID, 0, &records.GatewayKey)
			},
		} {
			if err := operation(); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketGatewayKeyHash).Put(records.GatewayKey.KeyHash[:], []byte(records.GatewayKey.ID))
	})
}

func (s *Store) initialize(afterUp func(uint64) error, stepHook func(uint64, string) error) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		var currentVersion uint64
		if meta != nil {
			current := meta.Get(keySchemaVersion)
			if current == nil || len(current) != 8 {
				return errors.New("invalid metadata schema version")
			}
			currentVersion = binary.BigEndian.Uint64(current)
		}
		if currentVersion > schemaVersion {
			return fmt.Errorf(
				"metadata schema version %d is newer than this build supports (%d)",
				currentVersion, schemaVersion,
			)
		}
		for currentVersion < schemaVersion {
			nextVersion := currentVersion + 1
			next := migrations[nextVersion-1]
			if next.version != nextVersion {
				return fmt.Errorf("metadata migration chain is invalid at version %d", nextVersion)
			}
			step := func(point string) error {
				if stepHook == nil {
					return nil
				}
				return stepHook(next.version, point)
			}
			if err := next.up(tx, step); err != nil {
				return fmt.Errorf("apply metadata migration %d (%s): %w", next.version, next.name, err)
			}
			if afterUp != nil {
				if err := afterUp(next.version); err != nil {
					return fmt.Errorf("metadata migration %d interrupted: %w", next.version, err)
				}
			}
			if err := migrationStep(step, "after_up"); err != nil {
				return fmt.Errorf("metadata migration %d interrupted: %w", next.version, err)
			}
			meta = tx.Bucket(bucketMeta)
			if meta == nil {
				return errors.New("metadata migration did not create meta bucket")
			}
			var encoded [8]byte
			binary.BigEndian.PutUint64(encoded[:], next.version)
			if err := migrationStep(step, "before_schema_version"); err != nil {
				return fmt.Errorf("metadata migration %d interrupted: %w", next.version, err)
			}
			if err := meta.Put(keySchemaVersion, encoded[:]); err != nil {
				return err
			}
			if err := migrationStep(step, "after_schema_version"); err != nil {
				return fmt.Errorf("metadata migration %d interrupted: %w", next.version, err)
			}
			if history := tx.Bucket(bucketMigrationHistory); history != nil {
				record, err := json.Marshal(MigrationRecord{Version: next.version, Name: next.name})
				if err != nil {
					return err
				}
				if err := migrationStep(step, "before_migration_history"); err != nil {
					return fmt.Errorf("metadata migration %d interrupted: %w", next.version, err)
				}
				if err := history.Put(encoded[:], record); err != nil {
					return err
				}
				if err := migrationStep(step, "after_migration_history"); err != nil {
					return fmt.Errorf("metadata migration %d interrupted: %w", next.version, err)
				}
			}
			currentVersion = nextVersion
		}
		for _, name := range requiredBuckets() {
			if tx.Bucket(name) == nil {
				return fmt.Errorf("metadata schema %d is missing bucket %s", schemaVersion, name)
			}
		}
		return nil
	})
}

func versionKey(version uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], version)
	return encoded[:]
}

func createInitialBuckets(tx *bbolt.Tx, step func(string) error) error {
	for _, name := range requiredBuckets() {
		if bytes.Equal(name, bucketMigrationHistory) {
			continue
		}
		label := string(name)
		if err := migrationStep(step, "before_create_bucket_"+label); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(name); err != nil {
			return fmt.Errorf("create bucket %s: %w", name, err)
		}
		if err := migrationStep(step, "after_create_bucket_"+label); err != nil {
			return err
		}
	}
	return nil
}

func migrateDeployments(tx *bbolt.Tx, step func(string) error) error {
	if err := migrationStep(step, "before_create_deployments_bucket"); err != nil {
		return err
	}
	deployments, err := tx.CreateBucketIfNotExists(bucketDeployments)
	if err != nil {
		return err
	}
	if err := migrationStep(step, "after_create_deployments_bucket"); err != nil {
		return err
	}
	routes := tx.Bucket(bucketRoutes)
	if routes == nil {
		return errors.New("routes bucket is missing")
	}
	type routeMigration struct {
		key        []byte
		route      domain.Route
		deployment domain.Deployment
	}
	var pending []routeMigration
	if err := routes.ForEach(func(key, raw []byte) error {
		if raw == nil {
			return nil
		}
		var route domain.Route
		if err := json.Unmarshal(raw, &route); err != nil {
			return fmt.Errorf("decode legacy route %q: %w", key, err)
		}
		if route.DeploymentID != "" {
			return nil
		}
		if route.ProviderID == "" || route.ProviderModel == "" {
			return fmt.Errorf("legacy route %q cannot be migrated", route.ID)
		}
		deploymentID := "dep_migrated_" + route.ID
		deployment := domain.Deployment{
			ID: deploymentID, Name: route.PublicModel + " / " + route.ProviderModel,
			ProviderID: route.ProviderID, ProviderModel: route.ProviderModel,
			InputMicrosPerMillion:  route.InputMicrosPerMillion,
			OutputMicrosPerMillion: route.OutputMicrosPerMillion,
			Priority:               route.Priority, Weight: 1, Enabled: route.Enabled,
			CreatedAt: route.CreatedAt, UpdatedAt: route.UpdatedAt, Revision: 1,
			DeletedAt: route.DeletedAt,
		}
		route.DeploymentID = deploymentID
		pending = append(pending, routeMigration{
			key: bytes.Clone(key), route: route, deployment: deployment,
		})
		return nil
	}); err != nil {
		return err
	}
	for _, item := range pending {
		encodedDeployment, err := json.Marshal(item.deployment)
		if err != nil {
			return err
		}
		if err := migrationStep(step, "before_put_deployment_"+item.deployment.ID); err != nil {
			return err
		}
		if err := deployments.Put([]byte(item.deployment.ID), encodedDeployment); err != nil {
			return err
		}
		if err := migrationStep(step, "after_put_deployment_"+item.deployment.ID); err != nil {
			return err
		}
		encodedRoute, err := json.Marshal(item.route)
		if err != nil {
			return err
		}
		if err := migrationStep(step, "before_put_route_"+item.route.ID); err != nil {
			return err
		}
		if err := routes.Put(item.key, encodedRoute); err != nil {
			return err
		}
		if err := migrationStep(step, "after_put_route_"+item.route.ID); err != nil {
			return err
		}
	}
	return nil
}

func migrateProviderProfiles(tx *bbolt.Tx, step func(string) error) error {
	if err := migrationStep(step, "before_migrate_provider_profiles"); err != nil {
		return err
	}
	credentials := tx.Bucket(bucketCredentials)
	providers := tx.Bucket(bucketProviders)
	deployments := tx.Bucket(bucketDeployments)
	if credentials == nil || providers == nil || deployments == nil {
		return errors.New("provider profile migration buckets are missing")
	}
	if err := rewriteBucket(credentials, func(raw []byte) ([]byte, error) {
		var credential domain.Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return nil, err
		}
		normalizeCredentialProfile(&credential)
		return json.Marshal(credential)
	}); err != nil {
		return fmt.Errorf("migrate credential profiles: %w", err)
	}
	providerByID := make(map[string]domain.ProviderInstance)
	if err := rewriteBucket(providers, func(raw []byte) ([]byte, error) {
		var instance domain.ProviderInstance
		if err := json.Unmarshal(raw, &instance); err != nil {
			return nil, err
		}
		normalizeProviderProfile(&instance, domain.EvidenceLegacy)
		providerByID[instance.ID] = instance
		return json.Marshal(instance)
	}); err != nil {
		return fmt.Errorf("migrate provider profiles: %w", err)
	}
	if err := rewriteBucket(deployments, func(raw []byte) ([]byte, error) {
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return nil, err
		}
		instance, ok := providerByID[deployment.ProviderID]
		if !ok {
			// Earlier migration fixtures and damaged legacy databases can contain
			// orphan deployments. Profile migration must remain atomic and must not
			// invent an access surface without a provider. Runtime topology checks
			// continue to reject these records if they are used.
			return raw, nil
		}
		normalizeDeploymentProfile(&deployment, instance, domain.EvidenceLegacy)
		return json.Marshal(deployment)
	}); err != nil {
		return fmt.Errorf("migrate deployment profiles: %w", err)
	}
	return migrationStep(step, "after_migrate_provider_profiles")
}

func rewriteBucket(bucket *bbolt.Bucket, transform func([]byte) ([]byte, error)) error {
	type entry struct{ key, value []byte }
	var updates []entry
	if err := bucket.ForEach(func(key, raw []byte) error {
		if raw == nil {
			return nil
		}
		encoded, err := transform(raw)
		if err != nil {
			return fmt.Errorf("record %q: %w", key, err)
		}
		updates = append(updates, entry{bytes.Clone(key), encoded})
		return nil
	}); err != nil {
		return err
	}
	for _, update := range updates {
		if err := bucket.Put(update.key, update.value); err != nil {
			return err
		}
	}
	return nil
}

func normalizeCredentialProfile(credential *domain.Credential) {
	profile, ok := domain.DefaultProviderProfile(credential.Type)
	if !ok {
		return
	}
	if credential.AccessSurface == "" {
		credential.AccessSurface = profile.AccessSurface
	}
	if credential.Scheme == "" {
		credential.Scheme = profile.CredentialScheme
	}
}

func normalizeProviderProfile(instance *domain.ProviderInstance, evidence domain.CapabilityEvidence) {
	legacyProfile := instance.AccessSurface == "" && instance.ProfileID == "" && instance.CredentialScheme == ""
	profile, ok := domain.DefaultProviderProfile(instance.Type)
	if !ok {
		return
	}
	if instance.AccessSurface == "" {
		instance.AccessSurface = profile.AccessSurface
	}
	if instance.ProfileID == "" {
		instance.ProfileID = profile.ProfileID
	}
	if instance.CredentialScheme == "" {
		instance.CredentialScheme = profile.CredentialScheme
	}
	if legacyProfile && !instance.Capabilities.Chat && !instance.Capabilities.Embeddings {
		instance.Capabilities = domain.DefaultProviderCapabilities(instance.Type)
	}
	if instance.ProfileID == domain.ProfileBedrockConverseText {
		instance.Capabilities.DeveloperRole = false
	}
	if len(instance.CapabilityEvidence) == 0 {
		instance.CapabilityEvidence = domain.EvidenceForCapabilities(instance.Capabilities, evidence)
	}
}

func normalizeDeploymentProfile(deployment *domain.Deployment, instance domain.ProviderInstance, evidence domain.CapabilityEvidence) {
	legacyProfile := deployment.AccessSurface == "" && deployment.ProfileID == ""
	if deployment.AccessSurface == "" {
		deployment.AccessSurface = instance.AccessSurface
	}
	if deployment.ProfileID == "" {
		deployment.ProfileID = instance.ProfileID
	}
	if legacyProfile && !deployment.Capabilities.Chat && !deployment.Capabilities.Embeddings {
		deployment.Capabilities = instance.Capabilities
	}
	if instance.ProfileID == domain.ProfileBedrockConverseText {
		deployment.Capabilities.DeveloperRole = false
	}
	if len(deployment.CapabilityEvidence) == 0 {
		deployment.CapabilityEvidence = domain.EvidenceForCapabilities(deployment.Capabilities, evidence)
	}
}

func migrationStep(step func(string) error, point string) error {
	if step == nil {
		return nil
	}
	return step(point)
}

func requiredBuckets() [][]byte {
	return [][]byte{
		bucketMeta,
		bucketCredentials,
		bucketProjects,
		bucketGatewayKeys,
		bucketGatewayKeyHash,
		bucketProviders,
		bucketDeployments,
		bucketRoutes,
		bucketRedactionPolicies,
		bucketTokenGuardPolicies,
		bucketAlertWebhooks,
		bucketAdminUsers,
		bucketAdminSessions,
		bucketAdminMFAAuthenticators,
		bucketAdminMFARecoveryCodes,
		bucketAdminMFAChallenges,
		bucketMigrationHistory,
		bucketProviderResources,
	}
}

func (s *Store) SchemaVersion() (uint64, error) {
	var version uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keySchemaVersion)
		if len(raw) != 8 {
			return errors.New("invalid metadata schema version")
		}
		version = binary.BigEndian.Uint64(raw)
		return nil
	})
	return version, err
}

func (s *Store) MigrationHistory() ([]MigrationRecord, error) {
	var records []MigrationRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMigrationHistory).ForEach(func(_, value []byte) error {
			var record MigrationRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return fmt.Errorf("decode migration history: %w", err)
			}
			records = append(records, record)
			return nil
		})
	})
	return records, err
}

// Snapshot writes a transactionally consistent bbolt image to a caller-owned
// staging path. The caller is responsible for atomic publication.
func (s *Store) Snapshot(path string) (MetadataInfo, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return MetadataInfo{}, err
	}
	var info MetadataInfo
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keySchemaVersion)
		if len(raw) != 8 {
			return errors.New("invalid metadata schema version")
		}
		info = MetadataInfo{
			SchemaVersion: binary.BigEndian.Uint64(raw), TxID: uint64(tx.ID()),
		}
		return tx.CopyFile(path, 0o600)
	})
	if err != nil {
		return MetadataInfo{}, fmt.Errorf("snapshot metadata: %w", err)
	}
	return info, nil
}

// CompactSnapshot copies only live keys into a fresh bbolt file. Unlike a
// page-level snapshot, it excludes freelist pages that may contain retired
// ciphertext or a deleted master-key rotation bridge.
func (s *Store) CompactSnapshot(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	destination, err := bbolt.Open(path, 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType})
	if err != nil {
		return fmt.Errorf("open compact metadata destination: %w", err)
	}
	compactErr := bbolt.Compact(destination, s.db, 0)
	closeErr := destination.Close()
	if compactErr != nil {
		return fmt.Errorf("compact metadata: %w", compactErr)
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func (s *Store) PutAdminUser(
	ctx context.Context,
	user domain.AdminUser,
	expectedRevision uint64,
) (domain.AdminUser, error) {
	if err := user.Validate(); err != nil {
		return domain.AdminUser{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.AdminUser{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putVersioned(tx.Bucket(bucketAdminUsers), user.Username, expectedRevision, &user)
	})
	return user, err
}

// CreateFirstAdmin atomically proves that the admin bucket is empty and
// creates its first user. This prevents concurrent setup requests from both
// succeeding.
func (s *Store) CreateFirstAdmin(
	ctx context.Context,
	user domain.AdminUser,
) (domain.AdminUser, error) {
	if err := user.Validate(); err != nil {
		return domain.AdminUser{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.AdminUser{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminUsers)
		if bucket.Stats().KeyN != 0 {
			return ErrAdminInitialized
		}
		return putVersioned(bucket, user.Username, 0, &user)
	})
	return user, err
}

func (s *Store) GetAdminUser(ctx context.Context, username string) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.getJSON(ctx, bucketAdminUsers, username, &user)
	return user, err
}

func (s *Store) AdminUserCount(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var count int
	err := s.db.View(func(tx *bbolt.Tx) error {
		count = tx.Bucket(bucketAdminUsers).Stats().KeyN
		return nil
	})
	return count, err
}

func (s *Store) PutAdminSession(ctx context.Context, session domain.AdminSession) error {
	if err := session.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminSessions).Put(session.IDHash[:], encoded)
	})
}

func (s *Store) GetAdminSession(
	ctx context.Context,
	hash [32]byte,
) (domain.AdminSession, error) {
	if err := ctx.Err(); err != nil {
		return domain.AdminSession{}, err
	}
	var session domain.AdminSession
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketAdminSessions).Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &session)
	})
	return session, err
}

func (s *Store) DeleteAdminSession(ctx context.Context, hash [32]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminSessions).Delete(hash[:])
	})
}

func (s *Store) DeleteAdminSessionsForUser(ctx context.Context, username string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminSessions)
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			var session domain.AdminSession
			if err := json.Unmarshal(raw, &session); err != nil {
				return err
			}
			if session.Username == username {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// InvalidateAdminAuthenticationForRestore makes credentials captured in a
// backup unusable before that backup is published as the live data set. The
// user generation changes and both transient authentication buckets are
// cleared in one transaction so a restored database can never expose a mixed
// state.
func (s *Store) InvalidateAdminAuthenticationForRestore(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		users := tx.Bucket(bucketAdminUsers)
		cursor := users.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			var user domain.AdminUser
			if err := json.Unmarshal(raw, &user); err != nil {
				return fmt.Errorf("decode admin user %q during restore: %w", key, err)
			}
			if user.SessionGeneration == ^uint64(0) || user.Revision == ^uint64(0) {
				return errors.New("admin authentication version is exhausted")
			}
			user.SessionGeneration++
			user.Revision++
			encoded, err := json.Marshal(user)
			if err != nil {
				return err
			}
			if err := users.Put(key, encoded); err != nil {
				return err
			}
		}
		for _, bucketName := range [][]byte{bucketAdminSessions, bucketAdminMFAChallenges} {
			bucket := tx.Bucket(bucketName)
			bucketCursor := bucket.Cursor()
			for key, _ := bucketCursor.First(); key != nil; key, _ = bucketCursor.Next() {
				if err := bucketCursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func adminMFAKey(username, id string) string { return username + "\x00" + id }

func (s *Store) PutAdminMFAAuthenticator(ctx context.Context, value domain.AdminMFAAuthenticator, expected uint64) (domain.AdminMFAAuthenticator, error) {
	if err := value.Validate(); err != nil {
		return value, err
	}
	if err := ctx.Err(); err != nil {
		return value, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putVersioned(tx.Bucket(bucketAdminMFAAuthenticators), adminMFAKey(value.Username, value.ID), expected, &value)
	})
	return value, err
}

func (s *Store) GetAdminMFAAuthenticator(ctx context.Context, username, id string) (domain.AdminMFAAuthenticator, error) {
	var value domain.AdminMFAAuthenticator
	err := s.getJSON(ctx, bucketAdminMFAAuthenticators, adminMFAKey(username, id), &value)
	return value, err
}

func (s *Store) ListAdminMFAAuthenticators(ctx context.Context, username string) ([]domain.AdminMFAAuthenticator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := []domain.AdminMFAAuthenticator{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		prefix := []byte(username + "\x00")
		cursor := tx.Bucket(bucketAdminMFAAuthenticators).Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			values = append(values, value)
		}
		return nil
	})
	return values, err
}

// AcceptAdminMFATimeStep atomically prevents concurrent or repeated use of a TOTP time step.
func (s *Store) AcceptAdminMFATimeStep(ctx context.Context, username, id string, step int64, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusActive || step <= value.LastAcceptedTimeStep {
			return ErrRevisionConflict
		}
		value.LastAcceptedTimeStep = step
		used := now.UTC()
		value.LastUsedAt = &used
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

func (s *Store) ReplaceAdminMFARecoveryCodes(ctx context.Context, username string, codes []domain.AdminMFARecoveryCode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, code := range codes {
		if code.Username != username {
			return errors.New("MFA recovery code user mismatch")
		}
		if err := code.Validate(); err != nil {
			return err
		}
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFARecoveryCodes)
		prefix := []byte(username + "\x00")
		cursor := bucket.Cursor()
		for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		for _, code := range codes {
			raw, err := json.Marshal(code)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(adminMFAKey(username, code.ID)), raw); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ConsumeAdminMFARecoveryCode(ctx context.Context, username string, hash [32]byte, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	remaining := 0
	found := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFARecoveryCodes)
		prefix := []byte(username + "\x00")
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var code domain.AdminMFARecoveryCode
			if err := json.Unmarshal(raw, &code); err != nil {
				return err
			}
			if code.UsedAt == nil && subtle.ConstantTimeCompare(code.CodeHash[:], hash[:]) == 1 {
				used := now.UTC()
				code.UsedAt = &used
				encoded, _ := json.Marshal(code)
				if err := bucket.Put(key, encoded); err != nil {
					return err
				}
				found = true
			} else if code.UsedAt == nil {
				remaining++
			}
		}
		if !found {
			return ErrNotFound
		}
		return nil
	})
	return remaining, err
}

func (s *Store) CountUnusedAdminMFARecoveryCodes(ctx context.Context, username string) (int, error) {
	count := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		prefix := []byte(username + "\x00")
		cursor := tx.Bucket(bucketAdminMFARecoveryCodes).Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var code domain.AdminMFARecoveryCode
			if err := json.Unmarshal(raw, &code); err != nil {
				return err
			}
			if code.UsedAt == nil {
				count++
			}
		}
		return nil
	})
	return count, err
}

func (s *Store) PutAdminMFAChallenge(ctx context.Context, value domain.AdminMFAChallenge) error {
	if err := value.Validate(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		cursor := bucket.Cursor()
		for key, existingRaw := cursor.First(); key != nil; key, existingRaw = cursor.Next() {
			var existing domain.AdminMFAChallenge
			if err := json.Unmarshal(existingRaw, &existing); err != nil {
				return err
			}
			if existing.Username == value.Username && existing.SessionGeneration == value.SessionGeneration {
				if value.CreatedAt.Before(existing.ExpiresAt) && existing.AttemptsRemaining < value.AttemptsRemaining {
					value.AttemptsRemaining = existing.AttemptsRemaining
				}
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(value.IDHash[:], raw)
	})
}

func (s *Store) ClaimAdminMFAChallenge(ctx context.Context, hash [32]byte, now time.Time) (domain.AdminMFAChallenge, error) {
	var value domain.AdminMFAChallenge
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		raw := bucket.Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if !now.Before(value.ExpiresAt) {
			_ = bucket.Delete(hash[:])
			return ErrNotFound
		}
		if value.AttemptsRemaining == 0 {
			return ErrNotFound
		}
		if value.Claimed {
			return ErrMFAClaimed
		}
		value.Claimed = true
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(hash[:], encoded)
	})
	return value, err
}

func (s *Store) CompleteAdminMFAChallenge(ctx context.Context, hash [32]byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		raw := bucket.Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAChallenge
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if !value.Claimed {
			return ErrRevisionConflict
		}
		return bucket.Delete(hash[:])
	})
}

func (s *Store) GetAdminMFAChallenge(ctx context.Context, hash [32]byte) (domain.AdminMFAChallenge, error) {
	var value domain.AdminMFAChallenge
	if err := ctx.Err(); err != nil {
		return value, err
	}
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketAdminMFAChallenges).Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &value)
	})
	return value, err
}

func (s *Store) DeleteAdminMFAChallenge(ctx context.Context, hash [32]byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucketAdminMFAChallenges).Delete(hash[:]) })
}

func (s *Store) FailAdminMFAChallenge(ctx context.Context, hash [32]byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		raw := bucket.Get(hash[:])
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAChallenge
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.AttemptsRemaining <= 1 {
			value.AttemptsRemaining = 0
			value.Claimed = false
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			return bucket.Put(hash[:], encoded)
		}
		value.AttemptsRemaining--
		value.Claimed = false
		encoded, _ := json.Marshal(value)
		return bucket.Put(hash[:], encoded)
	})
}

func (s *Store) ActivateAdminMFAAuthenticator(ctx context.Context, username, id string, step int64, now time.Time, limit int) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		prefix := []byte(username + "\x00")
		active := 0
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == domain.AdminMFAStatusActive {
				active++
			}
		}
		if active >= limit {
			return ErrMFALimit
		}
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusPending || value.ExpiresAt == nil || !now.Before(*value.ExpiresAt) {
			return ErrRevisionConflict
		}
		value.Status, value.ConfirmedAt, value.ExpiresAt = domain.AdminMFAStatusActive, &now, nil
		value.LastAcceptedTimeStep = step
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

func (s *Store) RevokeAdminMFAAuthenticator(ctx context.Context, username, id string, required bool) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		prefix := []byte(username + "\x00")
		active := 0
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == domain.AdminMFAStatusActive {
				active++
			}
		}
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusActive {
			return ErrRevisionConflict
		}
		if required && active <= 1 {
			return ErrMFARequired
		}
		value.Status, value.SecretCiphertext = domain.AdminMFAStatusRevoked, nil
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

func (s *Store) DeleteAdminMFAForUser(ctx context.Context, username string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		for _, bucketName := range [][]byte{bucketAdminMFAAuthenticators, bucketAdminMFARecoveryCodes} {
			bucket := tx.Bucket(bucketName)
			prefix := []byte(username + "\x00")
			cursor := bucket.Cursor()
			for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		bucket := tx.Bucket(bucketAdminMFAChallenges)
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			var value domain.AdminMFAChallenge
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Username == username {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RotateAdminIdentity atomically advances the security generation and removes
// every session and pre-auth challenge for one administrator.
func (s *Store) RotateAdminIdentity(ctx context.Context, username string) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		var err error
		user, err = rotateAdminIdentityTx(tx, username)
		return err
	})
	return user, err
}

func rotateAdminIdentityTx(tx *bbolt.Tx, username string) (domain.AdminUser, error) {
	users := tx.Bucket(bucketAdminUsers)
	raw := users.Get([]byte(username))
	if raw == nil {
		return domain.AdminUser{}, ErrNotFound
	}
	var user domain.AdminUser
	if err := json.Unmarshal(raw, &user); err != nil {
		return user, err
	}
	if user.SessionGeneration == ^uint64(0) || user.Revision == ^uint64(0) {
		return user, errors.New("admin identity version exhausted")
	}
	user.SessionGeneration++
	user.Revision++
	user.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(user)
	if err != nil {
		return user, err
	}
	if err = users.Put([]byte(username), encoded); err != nil {
		return user, err
	}
	if err = deleteAdminIdentityRecords(tx, username, false); err != nil {
		return user, err
	}
	return user, nil
}

func setPendingMFAAuditTx(tx *bbolt.Tx, user *domain.AdminUser, intent domain.AdminMFAAuditIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if user.PendingMFAAudit != nil && user.PendingMFAAudit.EventID != intent.EventID {
		return errors.New("a previous MFA audit event is still pending delivery")
	}
	user.PendingMFAAudit = &intent
	encoded, err := json.Marshal(*user)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketAdminUsers).Put([]byte(user.Username), encoded)
}

func (s *Store) ClearPendingAdminMFAAudit(ctx context.Context, username, eventID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminUsers)
		raw := bucket.Get([]byte(username))
		if raw == nil {
			return ErrNotFound
		}
		var user domain.AdminUser
		if err := json.Unmarshal(raw, &user); err != nil {
			return err
		}
		if user.PendingMFAAudit == nil {
			return nil
		}
		if user.PendingMFAAudit.EventID != eventID {
			return ErrRevisionConflict
		}
		user.PendingMFAAudit = nil
		encoded, err := json.Marshal(user)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(username), encoded)
	})
}

func (s *Store) ListPendingAdminMFAAudits(ctx context.Context) ([]domain.AdminMFAAuditIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var values []domain.AdminMFAAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminUsers).ForEach(func(_, raw []byte) error {
			var user domain.AdminUser
			if err := json.Unmarshal(raw, &user); err != nil {
				return err
			}
			if user.PendingMFAAudit != nil {
				values = append(values, *user.PendingMFAAudit)
			}
			return nil
		})
	})
	return values, err
}

func replaceAdminMFARecoveryCodesTx(tx *bbolt.Tx, username string, codes []domain.AdminMFARecoveryCode) error {
	for _, code := range codes {
		if err := code.Validate(); err != nil {
			return err
		}
		if code.Username != username {
			return errors.New("MFA recovery code owner does not match")
		}
	}
	bucket := tx.Bucket(bucketAdminMFARecoveryCodes)
	prefix := []byte(username + "\x00")
	cursor := bucket.Cursor()
	for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	for _, code := range codes {
		raw, err := json.Marshal(code)
		if err != nil {
			return err
		}
		if err = bucket.Put([]byte(adminMFAKey(username, code.ID)), raw); err != nil {
			return err
		}
	}
	return nil
}

// ConfirmAdminMFAEnrollment atomically activates a pending factor, creates the
// first recovery-code set when supplied, and rotates the administrator identity.
func (s *Store) ConfirmAdminMFAEnrollment(ctx context.Context, username, id string, step int64, now time.Time, limit int, codes []domain.AdminMFARecoveryCode, intent domain.AdminMFAAuditIntent) (domain.AdminUser, bool, error) {
	var user domain.AdminUser
	first := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		prefix := []byte(username + "\x00")
		active := 0
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == domain.AdminMFAStatusActive {
				active++
			}
		}
		if active >= limit {
			return ErrMFALimit
		}
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusPending || value.ExpiresAt == nil || !now.Before(*value.ExpiresAt) {
			return ErrRevisionConflict
		}
		value.Status = domain.AdminMFAStatusActive
		value.ConfirmedAt = &now
		value.ExpiresAt = nil
		value.LastAcceptedTimeStep = step
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err = bucket.Put(key, encoded); err != nil {
			return err
		}
		if active == 0 {
			if len(codes) == 0 {
				return errors.New("first MFA authenticator requires recovery codes")
			}
			if err = replaceAdminMFARecoveryCodesTx(tx, username, codes); err != nil {
				return err
			}
			first = true
		}
		user, err = rotateAdminIdentityTx(tx, username)
		if err != nil {
			return err
		}
		return setPendingMFAAuditTx(tx, &user, intent)
	})
	return user, first, err
}

func (s *Store) ReplaceAdminMFARecoveryCodesAndRotate(ctx context.Context, username string, codes []domain.AdminMFARecoveryCode, intent domain.AdminMFAAuditIntent) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := replaceAdminMFARecoveryCodesTx(tx, username, codes); err != nil {
			return err
		}
		var err error
		user, err = rotateAdminIdentityTx(tx, username)
		if err != nil {
			return err
		}
		return setPendingMFAAuditTx(tx, &user, intent)
	})
	return user, err
}

func (s *Store) RevokeAdminMFAAuthenticatorAndRotate(ctx context.Context, username, id string, required bool, clearRecovery bool, intent domain.AdminMFAAuditIntent) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAdminMFAAuthenticators)
		prefix := []byte(username + "\x00")
		active := 0
		cursor := bucket.Cursor()
		for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value.Status == domain.AdminMFAStatusActive {
				active++
			}
		}
		key := []byte(adminMFAKey(username, id))
		raw := bucket.Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var value domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value.Status != domain.AdminMFAStatusActive {
			return ErrRevisionConflict
		}
		if required && active <= 1 {
			return ErrMFARequired
		}
		value.Status = domain.AdminMFAStatusRevoked
		value.SecretCiphertext = nil
		value.Revision++
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err = bucket.Put(key, encoded); err != nil {
			return err
		}
		if clearRecovery || active <= 1 {
			if err = replaceAdminMFARecoveryCodesTx(tx, username, nil); err != nil {
				return err
			}
		}
		user, err = rotateAdminIdentityTx(tx, username)
		if err != nil {
			return err
		}
		return setPendingMFAAuditTx(tx, &user, intent)
	})
	return user, err
}

func (s *Store) DisableAdminMFAAndRotate(ctx context.Context, username string, recoveryHash *[32]byte, intent domain.AdminMFAAuditIntent) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if recoveryHash != nil {
			matched := false
			bucket := tx.Bucket(bucketAdminMFARecoveryCodes)
			prefix := []byte(username + "\x00")
			cursor := bucket.Cursor()
			for key, raw := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, raw = cursor.Next() {
				var code domain.AdminMFARecoveryCode
				if err := json.Unmarshal(raw, &code); err != nil {
					return err
				}
				if code.UsedAt == nil && subtle.ConstantTimeCompare(code.CodeHash[:], recoveryHash[:]) == 1 {
					matched = true
					break
				}
			}
			if !matched {
				return ErrNotFound
			}
		}
		for _, bucketName := range [][]byte{bucketAdminMFAAuthenticators, bucketAdminMFARecoveryCodes} {
			bucket := tx.Bucket(bucketName)
			prefix := []byte(username + "\x00")
			cursor := bucket.Cursor()
			for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		var err error
		user, err = rotateAdminIdentityTx(tx, username)
		if err != nil {
			return err
		}
		return setPendingMFAAuditTx(tx, &user, intent)
	})
	return user, err
}

// ResetAdminMFAIdentity additionally removes authenticators and recovery codes
// in the same transaction as identity invalidation.
func (s *Store) ResetAdminMFAIdentity(ctx context.Context, username string) (domain.AdminUser, error) {
	var user domain.AdminUser
	err := s.db.Update(func(tx *bbolt.Tx) error {
		users := tx.Bucket(bucketAdminUsers)
		raw := users.Get([]byte(username))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &user); err != nil {
			return err
		}
		user.SessionGeneration++
		user.Revision++
		user.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(user)
		if err != nil {
			return err
		}
		if err := users.Put([]byte(username), encoded); err != nil {
			return err
		}
		return deleteAdminIdentityRecords(tx, username, true)
	})
	return user, err
}

func deleteAdminIdentityRecords(tx *bbolt.Tx, username string, includeMFA bool) error {
	for _, bucketName := range [][]byte{bucketAdminSessions, bucketAdminMFAChallenges} {
		bucket := tx.Bucket(bucketName)
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			matches := false
			if bytes.Equal(bucketName, bucketAdminSessions) {
				var value domain.AdminSession
				if err := json.Unmarshal(raw, &value); err != nil {
					return err
				}
				matches = value.Username == username
			} else {
				var value domain.AdminMFAChallenge
				if err := json.Unmarshal(raw, &value); err != nil {
					return err
				}
				matches = value.Username == username
			}
			if matches {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
	}
	if includeMFA {
		prefix := []byte(username + "\x00")
		for _, bucketName := range [][]byte{bucketAdminMFAAuthenticators, bucketAdminMFARecoveryCodes} {
			cursor := tx.Bucket(bucketName).Cursor()
			for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Store) PutCredential(ctx context.Context, credential domain.Credential, expectedRevision uint64) (domain.Credential, error) {
	normalizeCredentialProfile(&credential)
	if err := credential.Validate(); err != nil {
		return domain.Credential{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Credential{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCredentials)
		return putVersioned(bucket, credential.ID, expectedRevision, &credential)
	})
	return credential, err
}

func (s *Store) GetCredential(ctx context.Context, id string) (domain.Credential, error) {
	var credential domain.Credential
	err := s.getJSON(ctx, bucketCredentials, id, &credential)
	return credential, err
}

func (s *Store) ListCredentials(ctx context.Context) ([]domain.Credential, error) {
	var credentials []domain.Credential
	err := s.listJSON(ctx, bucketCredentials, func(raw []byte) error {
		var credential domain.Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return err
		}
		credentials = append(credentials, credential)
		return nil
	})
	sort.Slice(credentials, func(i, j int) bool { return credentials[i].ID < credentials[j].ID })
	return credentials, err
}

func (s *Store) DeleteCredential(ctx context.Context, id string, expectedRevision uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" || expectedRevision == 0 {
		return errors.New("credential id and expected revision are required")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCredentials)
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var header struct {
			Revision uint64 `json:"revision"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
		if header.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if err := ensureCredentialUnreferenced(tx, id); err != nil {
			return err
		}
		return bucket.Delete([]byte(id))
	})
}

func (s *Store) PutProject(ctx context.Context, project domain.Project, expectedRevision uint64) (domain.Project, error) {
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Project{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putVersioned(tx.Bucket(bucketProjects), project.ID, expectedRevision, &project)
	})
	return project, err
}

func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	var project domain.Project
	err := s.getJSON(ctx, bucketProjects, id, &project)
	return project, err
}

func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	var projects []domain.Project
	err := s.listJSON(ctx, bucketProjects, func(raw []byte) error {
		var project domain.Project
		if err := json.Unmarshal(raw, &project); err != nil {
			return err
		}
		projects = append(projects, project)
		return nil
	})
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return projects, err
}

func (s *Store) PutGatewayKey(ctx context.Context, key domain.GatewayKey, expectedRevision uint64) (domain.GatewayKey, error) {
	if err := key.Validate(); err != nil {
		return domain.GatewayKey{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.GatewayKey{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketProjects).Get([]byte(key.ProjectID)) == nil {
			return fmt.Errorf("project %q: %w", key.ProjectID, ErrNotFound)
		}
		keys := tx.Bucket(bucketGatewayKeys)
		index := tx.Bucket(bucketGatewayKeyHash)
		existingID := index.Get(key.KeyHash[:])
		if existingID != nil && !bytes.Equal(existingID, []byte(key.ID)) {
			return ErrKeyHashConflict
		}
		if err := putVersioned(keys, key.ID, expectedRevision, &key); err != nil {
			return err
		}
		return index.Put(key.KeyHash[:], []byte(key.ID))
	})
	return key, err
}

func (s *Store) GetGatewayKey(ctx context.Context, id string) (domain.GatewayKey, error) {
	var key domain.GatewayKey
	err := s.getJSON(ctx, bucketGatewayKeys, id, &key)
	return key, err
}

func (s *Store) FindGatewayKeyByHash(ctx context.Context, hash [32]byte) (domain.GatewayKey, error) {
	if err := ctx.Err(); err != nil {
		return domain.GatewayKey{}, err
	}
	var key domain.GatewayKey
	err := s.db.View(func(tx *bbolt.Tx) error {
		id := tx.Bucket(bucketGatewayKeyHash).Get(hash[:])
		if id == nil {
			return ErrNotFound
		}
		raw := tx.Bucket(bucketGatewayKeys).Get(id)
		if raw == nil {
			return errors.New("gateway key hash index is inconsistent")
		}
		return json.Unmarshal(raw, &key)
	})
	return key, err
}

func (s *Store) ListGatewayKeys(ctx context.Context) ([]domain.GatewayKey, error) {
	var keys []domain.GatewayKey
	err := s.listJSON(ctx, bucketGatewayKeys, func(raw []byte) error {
		var key domain.GatewayKey
		if err := json.Unmarshal(raw, &key); err != nil {
			return err
		}
		keys = append(keys, key)
		return nil
	})
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys, err
}

func (s *Store) PutProvider(ctx context.Context, provider domain.ProviderInstance, expectedRevision uint64) (domain.ProviderInstance, error) {
	if err := provider.Validate(); err != nil {
		return domain.ProviderInstance{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.ProviderInstance{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		rawCredential := tx.Bucket(bucketCredentials).Get([]byte(provider.CredentialID))
		if rawCredential == nil {
			return fmt.Errorf("credential %q: %w", provider.CredentialID, ErrNotFound)
		}
		var credential domain.Credential
		if err := json.Unmarshal(rawCredential, &credential); err != nil {
			return fmt.Errorf("decode credential %q: %w", provider.CredentialID, err)
		}
		normalizeCredentialProfile(&credential)
		if err := validateProviderCredentialProfile(provider, credential); err != nil {
			return err
		}
		deployments := tx.Bucket(bucketDeployments)
		if err := deployments.ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var deployment domain.Deployment
			if err := json.Unmarshal(raw, &deployment); err != nil {
				return err
			}
			if deployment.ProviderID != provider.ID || deployment.DeletedAt != nil {
				return nil
			}
			if err := validateDeploymentProviderProfile(deployment, provider); err != nil {
				return fmt.Errorf("deployment %q would become incompatible: %w", deployment.ID, err)
			}
			return nil
		}); err != nil {
			return err
		}
		return putVersioned(tx.Bucket(bucketProviders), provider.ID, expectedRevision, &provider)
	})
	return provider, err
}

func (s *Store) GetProvider(ctx context.Context, id string) (domain.ProviderInstance, error) {
	var provider domain.ProviderInstance
	err := s.getJSON(ctx, bucketProviders, id, &provider)
	return provider, err
}

func (s *Store) ListProviders(ctx context.Context) ([]domain.ProviderInstance, error) {
	var providers []domain.ProviderInstance
	err := s.listJSON(ctx, bucketProviders, func(raw []byte) error {
		var provider domain.ProviderInstance
		if err := json.Unmarshal(raw, &provider); err != nil {
			return err
		}
		providers = append(providers, provider)
		return nil
	})
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return providers, err
}

func (s *Store) PutDeployment(ctx context.Context, deployment domain.Deployment, expectedRevision uint64) (domain.Deployment, error) {
	if err := ctx.Err(); err != nil {
		return domain.Deployment{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		rawProvider := tx.Bucket(bucketProviders).Get([]byte(deployment.ProviderID))
		if rawProvider == nil {
			return fmt.Errorf("provider %q: %w", deployment.ProviderID, ErrNotFound)
		}
		var instance domain.ProviderInstance
		if err := json.Unmarshal(rawProvider, &instance); err != nil {
			return fmt.Errorf("decode provider %q: %w", deployment.ProviderID, err)
		}
		if err := deployment.Validate(); err != nil {
			return err
		}
		if err := validateDeploymentProviderProfile(deployment, instance); err != nil {
			return err
		}
		return putVersioned(tx.Bucket(bucketDeployments), deployment.ID, expectedRevision, &deployment)
	})
	return deployment, err
}

func validateProviderCredentialProfile(instance domain.ProviderInstance, credential domain.Credential) error {
	if credential.Type != instance.Type || credential.AccessSurface != instance.AccessSurface ||
		credential.Scheme != instance.CredentialScheme {
		return errors.New("provider credential profile is incompatible")
	}
	return nil
}

func validateDeploymentProviderProfile(deployment domain.Deployment, instance domain.ProviderInstance) error {
	if deployment.AccessSurface != instance.AccessSurface || deployment.ProfileID != instance.ProfileID {
		return errors.New("deployment access surface or profile does not match provider")
	}
	if !providerCapabilitySubset(deployment.Capabilities, instance.Capabilities) {
		return errors.New("deployment capabilities exceed provider capabilities")
	}
	for name, value := range deployment.CapabilityEvidence {
		providerValue, ok := instance.CapabilityEvidence[name]
		if !ok || capabilityEvidenceRank(value) > capabilityEvidenceRank(providerValue) {
			return errors.New("deployment capability evidence exceeds provider evidence")
		}
	}
	return nil
}

func providerCapabilitySubset(candidate, available domain.ProviderCapabilities) bool {
	return (!candidate.Chat || available.Chat) &&
		(!candidate.Streaming || available.Streaming) &&
		(!candidate.Embeddings || available.Embeddings) &&
		(!candidate.Tools || available.Tools) &&
		(!candidate.Vision || available.Vision) &&
		(!candidate.JSONMode || available.JSONMode) &&
		(!candidate.DeveloperRole || available.DeveloperRole) &&
		(!candidate.Reasoning || available.Reasoning) &&
		(!candidate.StreamUsage || available.StreamUsage) &&
		providerCapabilityLimitSubset(candidate.MaxContextTokens, available.MaxContextTokens) &&
		providerCapabilityLimitSubset(candidate.MaxOutputTokens, available.MaxOutputTokens)
}

func providerCapabilityLimitSubset(candidate, available int64) bool {
	if available == 0 {
		return candidate >= 0
	}
	return candidate > 0 && candidate <= available
}

func capabilityEvidenceRank(value domain.CapabilityEvidence) int {
	switch value {
	case domain.EvidenceVerified:
		return 3
	case domain.EvidenceDeclared:
		return 2
	case domain.EvidenceLegacy:
		return 1
	default:
		return 0
	}
}

func (s *Store) GetDeployment(ctx context.Context, id string) (domain.Deployment, error) {
	var deployment domain.Deployment
	err := s.getJSON(ctx, bucketDeployments, id, &deployment)
	return deployment, err
}

func (s *Store) ListDeployments(ctx context.Context) ([]domain.Deployment, error) {
	var deployments []domain.Deployment
	err := s.listJSON(ctx, bucketDeployments, func(raw []byte) error {
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		deployments = append(deployments, deployment)
		return nil
	})
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].ID < deployments[j].ID })
	return deployments, err
}

func (s *Store) PutRoute(ctx context.Context, route domain.Route, expectedRevision uint64) (domain.Route, error) {
	if err := route.Validate(); err != nil {
		return domain.Route{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Route{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if route.DeploymentID != "" {
			if tx.Bucket(bucketDeployments).Get([]byte(route.DeploymentID)) == nil {
				return fmt.Errorf("deployment %q: %w", route.DeploymentID, ErrNotFound)
			}
		} else if tx.Bucket(bucketProviders).Get([]byte(route.ProviderID)) == nil {
			return fmt.Errorf("provider %q: %w", route.ProviderID, ErrNotFound)
		}
		return putVersioned(tx.Bucket(bucketRoutes), route.ID, expectedRevision, &route)
	})
	return route, err
}

func (s *Store) GetRoute(ctx context.Context, id string) (domain.Route, error) {
	var route domain.Route
	err := s.getJSON(ctx, bucketRoutes, id, &route)
	return route, err
}

func (s *Store) ListRoutes(ctx context.Context) ([]domain.Route, error) {
	var routes []domain.Route
	err := s.listJSON(ctx, bucketRoutes, func(raw []byte) error {
		var route domain.Route
		if err := json.Unmarshal(raw, &route); err != nil {
			return err
		}
		routes = append(routes, route)
		return nil
	})
	sort.Slice(routes, func(i, j int) bool { return routes[i].ID < routes[j].ID })
	return routes, err
}

func (s *Store) PutRedactionPolicy(
	ctx context.Context,
	policy domain.RedactionPolicy,
	expectedRevision uint64,
) (domain.RedactionPolicy, error) {
	if err := policy.Validate(); err != nil {
		return domain.RedactionPolicy{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.RedactionPolicy{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putVersioned(
			tx.Bucket(bucketRedactionPolicies),
			policy.ID,
			expectedRevision,
			&policy,
		)
	})
	return policy, err
}

func (s *Store) GetRedactionPolicy(ctx context.Context, id string) (domain.RedactionPolicy, error) {
	var policy domain.RedactionPolicy
	err := s.getJSON(ctx, bucketRedactionPolicies, id, &policy)
	return policy, err
}

func (s *Store) ListRedactionPolicies(ctx context.Context) ([]domain.RedactionPolicy, error) {
	var policies []domain.RedactionPolicy
	err := s.listJSON(ctx, bucketRedactionPolicies, func(raw []byte) error {
		var policy domain.RedactionPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			return err
		}
		policies = append(policies, policy)
		return nil
	})
	sort.Slice(policies, func(i, j int) bool { return policies[i].ID < policies[j].ID })
	return policies, err
}

func (s *Store) PutTokenGuardPolicy(
	ctx context.Context,
	policy domain.TokenGuardPolicy,
	expectedRevision uint64,
) (domain.TokenGuardPolicy, error) {
	if err := policy.Validate(); err != nil {
		return domain.TokenGuardPolicy{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.TokenGuardPolicy{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putVersioned(tx.Bucket(bucketTokenGuardPolicies), policy.ID, expectedRevision, &policy)
	})
	return policy, err
}

func (s *Store) GetTokenGuardPolicy(ctx context.Context, id string) (domain.TokenGuardPolicy, error) {
	var policy domain.TokenGuardPolicy
	err := s.getJSON(ctx, bucketTokenGuardPolicies, id, &policy)
	return policy, err
}

func (s *Store) ListTokenGuardPolicies(ctx context.Context) ([]domain.TokenGuardPolicy, error) {
	var policies []domain.TokenGuardPolicy
	err := s.listJSON(ctx, bucketTokenGuardPolicies, func(raw []byte) error {
		var policy domain.TokenGuardPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			return err
		}
		policies = append(policies, policy)
		return nil
	})
	sort.Slice(policies, func(i, j int) bool { return policies[i].ID < policies[j].ID })
	return policies, err
}

func (s *Store) PutAlertWebhook(
	ctx context.Context,
	webhook domain.AlertWebhook,
	expectedRevision uint64,
) (domain.AlertWebhook, error) {
	if err := webhook.Validate(); err != nil {
		return domain.AlertWebhook{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.AlertWebhook{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if webhook.CredentialID != "" &&
			tx.Bucket(bucketCredentials).Get([]byte(webhook.CredentialID)) == nil {
			return fmt.Errorf("credential %q: %w", webhook.CredentialID, ErrNotFound)
		}
		return putVersioned(tx.Bucket(bucketAlertWebhooks), webhook.ID, expectedRevision, &webhook)
	})
	return webhook, err
}

func (s *Store) PutAlertWebhookBundle(
	ctx context.Context,
	webhook domain.AlertWebhook,
	expectedWebhookRevision uint64,
	credential *domain.Credential,
	expectedCredentialRevision uint64,
	deleteCredentialID string,
) (domain.AlertWebhook, error) {
	if err := webhook.Validate(); err != nil {
		return domain.AlertWebhook{}, err
	}
	if credential != nil {
		if err := credential.Validate(); err != nil {
			return domain.AlertWebhook{}, err
		}
		if webhook.CredentialID == "" || credential.ID != webhook.CredentialID {
			return domain.AlertWebhook{}, errors.New("alert credential does not match webhook")
		}
	}
	if deleteCredentialID != "" && deleteCredentialID == webhook.CredentialID {
		return domain.AlertWebhook{}, errors.New("cannot delete the active alert credential")
	}
	if err := ctx.Err(); err != nil {
		return domain.AlertWebhook{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if credential != nil {
			if err := putVersioned(
				tx.Bucket(bucketCredentials),
				credential.ID,
				expectedCredentialRevision,
				credential,
			); err != nil {
				return err
			}
		}
		if webhook.CredentialID != "" &&
			tx.Bucket(bucketCredentials).Get([]byte(webhook.CredentialID)) == nil {
			return fmt.Errorf("credential %q: %w", webhook.CredentialID, ErrNotFound)
		}
		if err := putVersioned(
			tx.Bucket(bucketAlertWebhooks),
			webhook.ID,
			expectedWebhookRevision,
			&webhook,
		); err != nil {
			return err
		}
		if deleteCredentialID == "" {
			return nil
		}
		if tx.Bucket(bucketCredentials).Get([]byte(deleteCredentialID)) == nil {
			return ErrNotFound
		}
		if err := ensureCredentialUnreferenced(tx, deleteCredentialID); err != nil {
			return err
		}
		return tx.Bucket(bucketCredentials).Delete([]byte(deleteCredentialID))
	})
	return webhook, err
}

func ensureCredentialUnreferenced(tx *bbolt.Tx, credentialID string) error {
	for _, bucketName := range [][]byte{bucketProviders, bucketAlertWebhooks} {
		err := tx.Bucket(bucketName).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var reference struct {
				CredentialID string     `json:"credential_id"`
				DeletedAt    *time.Time `json:"deleted_at,omitempty"`
			}
			if err := json.Unmarshal(raw, &reference); err != nil {
				return err
			}
			if reference.CredentialID == credentialID && reference.DeletedAt == nil {
				return ErrCredentialInUse
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetAlertWebhook(ctx context.Context, id string) (domain.AlertWebhook, error) {
	var webhook domain.AlertWebhook
	err := s.getJSON(ctx, bucketAlertWebhooks, id, &webhook)
	return webhook, err
}

func (s *Store) ListAlertWebhooks(ctx context.Context) ([]domain.AlertWebhook, error) {
	var webhooks []domain.AlertWebhook
	err := s.listJSON(ctx, bucketAlertWebhooks, func(raw []byte) error {
		var webhook domain.AlertWebhook
		if err := json.Unmarshal(raw, &webhook); err != nil {
			return err
		}
		webhooks = append(webhooks, webhook)
		return nil
	})
	sort.Slice(webhooks, func(i, j int) bool { return webhooks[i].ID < webhooks[j].ID })
	return webhooks, err
}

func (s *Store) getJSON(ctx context.Context, bucketName []byte, id string, target any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketName).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, target)
	})
}

func (s *Store) listJSON(ctx context.Context, bucketName []byte, visit func([]byte) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(_, value []byte) error {
			if value == nil {
				return nil
			}
			return visit(value)
		})
	})
}

func (s *Store) PutProviderResource(ctx context.Context, resource domain.ProviderResource, expectedRevision uint64) (domain.ProviderResource, error) {
	if err := resource.Validate(); err != nil {
		return domain.ProviderResource{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketProjects).Get([]byte(resource.ProjectID)) == nil {
			return errors.New("resource project does not exist")
		}
		if tx.Bucket(bucketProviders).Get([]byte(resource.ProviderID)) == nil {
			return errors.New("resource provider does not exist")
		}
		if tx.Bucket(bucketDeployments).Get([]byte(resource.DeploymentID)) == nil {
			return errors.New("resource deployment does not exist")
		}
		bucket := tx.Bucket(bucketProviderResources)
		if expectedRevision == 0 && resource.IdempotencyKeyHash != ([32]byte{}) {
			if err := bucket.ForEach(func(key, raw []byte) error {
				var existing domain.ProviderResource
				if err := json.Unmarshal(raw, &existing); err != nil {
					return err
				}
				if existing.ProjectID == resource.ProjectID && existing.Kind == resource.Kind && existing.IdempotencyKeyHash == resource.IdempotencyKeyHash {
					return ErrAlreadyExists
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return putVersioned(bucket, resource.ID, expectedRevision, &resource)
	})
	return resource, err
}

func (s *Store) ProviderResource(ctx context.Context, projectID, id string) (domain.ProviderResource, error) {
	var resource domain.ProviderResource
	if err := s.getJSON(ctx, bucketProviderResources, id, &resource); err != nil {
		return resource, err
	}
	if resource.ProjectID != projectID {
		return domain.ProviderResource{}, ErrNotFound
	}
	return resource, nil
}

func (s *Store) ProviderResourceByIdempotency(ctx context.Context, projectID string, kind domain.ProviderResourceKind, keyHash [32]byte) (domain.ProviderResource, error) {
	var found domain.ProviderResource
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketProviderResources).ForEach(func(_, raw []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var resource domain.ProviderResource
			if err := json.Unmarshal(raw, &resource); err != nil {
				return err
			}
			if resource.ProjectID == projectID && resource.Kind == kind && resource.IdempotencyKeyHash == keyHash {
				found = resource
				return errStopIteration
			}
			return nil
		})
	})
	if errors.Is(err, errStopIteration) {
		return found, nil
	}
	if err != nil {
		return found, err
	}
	return found, ErrNotFound
}

func (s *Store) DeleteProviderResource(ctx context.Context, projectID, id string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketProviderResources)
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var resource domain.ProviderResource
		if err := json.Unmarshal(raw, &resource); err != nil {
			return err
		}
		if resource.ProjectID != projectID {
			return ErrNotFound
		}
		return bucket.Delete([]byte(id))
	})
}

func (s *Store) ExpiredProviderResources(ctx context.Context, now time.Time) ([]domain.ProviderResource, error) {
	var expired []domain.ProviderResource
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketProviderResources)
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var resource domain.ProviderResource
			if err := json.Unmarshal(raw, &resource); err != nil {
				return err
			}
			if !resource.ExpiresAt.After(now) && resource.ExpiryReapable() {
				expired = append(expired, resource)
			}
		}
		return nil
	})
	return expired, err
}

type revisioned interface {
	GetRevision() uint64
	SetRevision(uint64)
}

func putVersioned(bucket *bbolt.Bucket, id string, expectedRevision uint64, value revisioned) error {
	existing := bucket.Get([]byte(id))
	if existing == nil {
		if expectedRevision != 0 {
			return ErrNotFound
		}
		value.SetRevision(1)
	} else {
		if expectedRevision == 0 {
			return ErrAlreadyExists
		}
		var header struct {
			Revision uint64 `json:"revision"`
		}
		if err := json.Unmarshal(existing, &header); err != nil {
			return err
		}
		if header.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		value.SetRevision(expectedRevision + 1)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(id), encoded)
}
