package bolt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/masterkey"
	bbolt "go.etcd.io/bbolt"
)

const schemaVersion uint64 = 15

const (
	maxPriceVersionsPerDeployment   = 10_000
	maxScheduledPricesPerDeployment = 1_000
	maxPricingIdempotencyRecords    = 100_000
)

func CurrentSchemaVersion() uint64 { return schemaVersion }

var (
	ErrNotFound            = errors.New("record not found")
	ErrAlreadyExists       = errors.New("record already exists")
	ErrRevisionConflict    = errors.New("record revision conflict")
	ErrKeyHashConflict     = errors.New("gateway key hash already exists")
	ErrCredentialInUse     = errors.New("credential is still referenced")
	ErrAdminInitialized    = errors.New("an admin user already exists")
	ErrMFARequired         = errors.New("MFA is required")
	ErrMFALimit            = errors.New("MFA authenticator limit reached")
	ErrMFAClaimed          = errors.New("MFA challenge is already claimed")
	ErrIdempotencyConflict = errors.New("idempotency key conflicts with another request")
	ErrPricingQuarantined  = errors.New("deployment pricing is quarantined")
	errStopIteration       = errors.New("stop iteration")
)

var (
	bucketMeta                       = []byte("meta")
	bucketCredentials                = []byte("credentials")
	bucketProjects                   = []byte("projects")
	bucketGatewayKeys                = []byte("gateway_keys")
	bucketGatewayKeyHash             = []byte("gateway_key_hash")
	bucketProviders                  = []byte("providers")
	bucketDeployments                = []byte("deployments")
	bucketRoutes                     = []byte("routes")
	bucketRedactionPolicies          = []byte("redaction_policies")
	bucketTokenGuardPolicies         = []byte("token_guard_policies")
	bucketAlertWebhooks              = []byte("alert_webhooks")
	bucketAdminUsers                 = []byte("admin_users")
	bucketAdminSessions              = []byte("admin_sessions")
	bucketAdminMFAAuthenticators     = []byte("admin_mfa_authenticators")
	bucketAdminMFARecoveryCodes      = []byte("admin_mfa_recovery_codes")
	bucketAdminMFAChallenges         = []byte("admin_mfa_challenges")
	bucketMigrationHistory           = []byte("migration_history")
	bucketProviderResources          = []byte("provider_resources")
	bucketDeploymentPriceVersions    = []byte("deployment_price_versions")
	bucketDeploymentPriceTimeline    = []byte("deployment_price_timeline")
	bucketDeploymentPriceNext        = []byte("deployment_price_next_version")
	bucketDeploymentPricingHighWater = []byte("deployment_pricing_high_water")
	bucketDeploymentPricePins        = []byte("deployment_price_pin_intents")
	bucketPricingAuditIntents        = []byte("pricing_audit_intents")
	bucketPricingIdempotency         = []byte("pricing_idempotency")
	bucketDeploymentPriceProposals   = []byte("deployment_price_proposals")
	bucketPricingProposalIdempotency = []byte("pricing_proposal_idempotency")
	bucketCostAdjustmentIntents      = []byte("cost_adjustment_intents")
	keySchemaVersion                 = []byte("schema_version")
	keyVaultCheck                    = []byte("vault_key_check")
	keyUsageCheckpoint               = []byte("usage_checkpoint")
	keyTokenGuardCheckpoint          = []byte("token_guard_checkpoint")
	keyAuditCheckpoint               = []byte("audit_checkpoint")
	keyAuditHMACEnvelope             = []byte("audit_hmac_envelope")
	keyVaultKeyring                  = []byte("vault_keyring")
	keyKeySlotDescriptor             = []byte("key_slot_descriptor")
	keyKeySlotAuditIntent            = []byte("key_slot_audit_intent")
	keyMasterKeyRotationAuditIntent  = []byte("master_key_rotation_audit_intent")
	keyRuntimeSettings               = []byte("runtime_settings")
	keyInstanceUISettings            = []byte("instance_ui_settings")
	keyMinimumLedgerReaderVersion    = []byte("minimum_ledger_reader_version")
	keyLedgerFeatureEpoch            = []byte("ledger_feature_epoch")
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
	{version: 9, name: "provider_profile_bindings", up: migrateProviderProfileBindings},
	{version: 10, name: "master_key_slots", up: func(_ *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_reserve_key_slot_descriptor"); err != nil {
			return err
		}
		return migrationStep(step, "after_reserve_key_slot_descriptor")
	}},
	{version: 11, name: "versioned_deployment_pricing", up: migrateVersionedDeploymentPricing},
	{version: 12, name: "deployment_price_pin_intents", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_create_deployment_price_pin_intents"); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketDeploymentPricePins); err != nil {
			return err
		}
		highWaters := tx.Bucket(bucketDeploymentPricingHighWater)
		if highWaters != nil {
			if err := rewriteBucket(highWaters, func(raw []byte) ([]byte, error) {
				var highWater domain.DeploymentPricingHighWater
				if err := json.Unmarshal(raw, &highWater); err != nil {
					return nil, err
				}
				if highWater.LatestObservedPriceVersionID == "" {
					price, err := selectDeploymentPriceVersionTx(tx, highWater.DeploymentID, highWater.LatestSelectedAt)
					if err != nil {
						return nil, fmt.Errorf("backfill deployment %q pricing high-water: %w", highWater.DeploymentID, err)
					}
					highWater.LatestObservedPriceVersionID = price.ID
				}
				if err := highWater.Validate(); err != nil {
					return nil, err
				}
				return json.Marshal(highWater)
			}); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_create_deployment_price_pin_intents")
	}},
	{version: 13, name: "cost_adjustment_intents", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_create_cost_adjustment_intents"); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketCostAdjustmentIntents); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		if err := meta.Put(keyMinimumLedgerReaderVersion, []byte("v2")); err != nil {
			return err
		}
		if err := meta.Put(keyLedgerFeatureEpoch, []byte{2}); err != nil {
			return err
		}
		return migrationStep(step, "after_create_cost_adjustment_intents")
	}},
	{version: 14, name: "pricing_proposals", up: func(tx *bbolt.Tx, step func(string) error) error {
		for _, name := range [][]byte{bucketDeploymentPriceProposals, bucketPricingProposalIdempotency} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_create_pricing_proposal_buckets")
	}},
	{version: 15, name: "optional_manual_price_evidence", up: func(_ *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_optional_manual_price_evidence"); err != nil {
			return err
		}
		return migrationStep(step, "after_optional_manual_price_evidence")
	}},
}

func migrateVersionedDeploymentPricing(tx *bbolt.Tx, step func(string) error) error {
	for _, item := range []struct {
		name  []byte
		label string
	}{
		{bucketDeploymentPriceVersions, "deployment_price_versions"},
		{bucketDeploymentPriceTimeline, "deployment_price_timeline"},
		{bucketDeploymentPriceNext, "deployment_price_next_version"},
		{bucketDeploymentPricingHighWater, "deployment_pricing_high_water"},
		{bucketPricingAuditIntents, "pricing_audit_intents"},
		{bucketPricingIdempotency, "pricing_idempotency"},
	} {
		if err := migrationStep(step, "before_create_"+item.label); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(item.name); err != nil {
			return err
		}
		if err := migrationStep(step, "after_create_"+item.label); err != nil {
			return err
		}
	}
	deployments := tx.Bucket(bucketDeployments)
	if deployments == nil {
		return errors.New("deployment bucket is missing during pricing migration")
	}
	now := time.Now().UTC()
	if err := deployments.ForEach(func(_, raw []byte) error {
		if raw == nil {
			return nil
		}
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		billingMode := domain.BillingModeMetered
		var explicitResolution PricingMigrationResolution
		if resolutions := tx.Bucket(bucketPricingMigrationResolutions); resolutions != nil {
			if encoded := resolutions.Get([]byte(deployment.ID)); encoded != nil {
				if err := json.Unmarshal(encoded, &explicitResolution); err != nil {
					return err
				}
			}
		}
		if deployment.InputMicrosPerMillion == 0 && deployment.OutputMicrosPerMillion == 0 && deployment.FixedRequestMicrosUSD == 0 {
			if explicitResolution.KeepDisabled {
				return nil
			}
			if explicitResolution.Mode == domain.BillingModeFree {
				billingMode = domain.BillingModeFree
			} else if deployment.Enabled && deployment.DeletedAt == nil {
				return fmt.Errorf("pricing migration readiness failed: enabled deployment %q has ambiguous zero legacy pricing; explicitly resolve it as free or metered before upgrading", deployment.ID)
			} else {
				return nil
			}
		}
		if err := migrationStep(step, "before_seed_price_"+deployment.ID); err != nil {
			return err
		}
		origin, err := json.Marshal(struct {
			DeploymentID           string `json:"deployment_id"`
			DeploymentRevision     uint64 `json:"deployment_revision"`
			InputMicrosPerMillion  int64  `json:"input_micros_per_million"`
			OutputMicrosPerMillion int64  `json:"output_micros_per_million"`
			FixedRequestMicrosUSD  int64  `json:"fixed_request_micros_usd"`
		}{deployment.ID, deployment.Revision, deployment.InputMicrosPerMillion, deployment.OutputMicrosPerMillion, deployment.FixedRequestMicrosUSD})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(origin)
		price := domain.DeploymentPriceVersion{
			ID: fmt.Sprintf("price_migration_%x", digest[:12]), DeploymentID: deployment.ID,
			Version: 1, BillingMode: billingMode, Currency: "USD",
			FormulaVersion:         domain.PriceFormulaUSDTokensV1,
			InputMicrosPerMillion:  deployment.InputMicrosPerMillion,
			OutputMicrosPerMillion: deployment.OutputMicrosPerMillion,
			FixedRequestMicrosUSD:  deployment.FixedRequestMicrosUSD,
			EffectiveFrom:          now, CreatedBy: "system:migration:v11", CreatedAt: now, Revision: 1,
			Source: domain.PriceSource{
				Type: domain.PriceSourceMigration, Assurance: domain.PriceAssuranceAsserted,
				ReceivedAt: now, ContentSHA256: fmt.Sprintf("sha256:%x", digest[:]),
				Reference:        "metadata schema v10 deployment price",
				MigrationVersion: 11, OriginalResourceID: deployment.ID, OriginalRevision: deployment.Revision,
			},
		}
		if explicitResolution.SourceReference != "" {
			price.Source.Reference, price.Source.ContentSHA256 = explicitResolution.SourceReference, explicitResolution.SourceContentSHA256
		}
		if err := price.Validate(); err != nil {
			return fmt.Errorf("migrate deployment %q price: %w", deployment.ID, err)
		}
		if existingRaw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(price.ID)); existingRaw != nil {
			var existing domain.DeploymentPriceVersion
			if err := json.Unmarshal(existingRaw, &existing); err != nil {
				return err
			}
			if existing.DeploymentID != price.DeploymentID || existing.Version != 1 ||
				existing.InputMicrosPerMillion != price.InputMicrosPerMillion || existing.OutputMicrosPerMillion != price.OutputMicrosPerMillion ||
				existing.FixedRequestMicrosUSD != price.FixedRequestMicrosUSD || existing.Source.Type != domain.PriceSourceMigration ||
				existing.Source.OriginalResourceID != deployment.ID || existing.Source.OriginalRevision != deployment.Revision {
				return fmt.Errorf("existing migration price %q conflicts with deployment %q", price.ID, deployment.ID)
			}
			timeline, err := tx.Bucket(bucketDeploymentPriceTimeline).CreateBucketIfNotExists([]byte(deployment.ID))
			if err != nil {
				return err
			}
			if err := timeline.Put(deploymentPriceTimelineKey(existing.EffectiveFrom), []byte(existing.ID)); err != nil {
				return err
			}
			if err := tx.Bucket(bucketDeploymentPriceNext).Put([]byte(deployment.ID), versionKey(1)); err != nil {
				return err
			}
			return migrationStep(step, "after_seed_price_"+deployment.ID)
		}
		if err := putDeploymentPriceVersionTx(tx, price); err != nil {
			return err
		}
		if err := tx.Bucket(bucketDeploymentPriceNext).Put([]byte(deployment.ID), versionKey(1)); err != nil {
			return err
		}
		return migrationStep(step, "after_seed_price_"+deployment.ID)
	}); err != nil {
		return err
	}
	if tx.Bucket(bucketPricingMigrationResolutions) != nil {
		return tx.DeleteBucket(bucketPricingMigrationResolutions)
	}
	return nil
}

func migrateProviderProfileBindings(tx *bbolt.Tx, step func(string) error) error {
	if err := migrationStep(step, "before_migrate_provider_profile_bindings"); err != nil {
		return err
	}
	providers, deployments := tx.Bucket(bucketProviders), tx.Bucket(bucketDeployments)
	if providers == nil || deployments == nil {
		return errors.New("provider profile binding migration buckets are missing")
	}
	providerByID := make(map[string]domain.ProviderInstance)
	if err := rewriteBucket(providers, func(raw []byte) ([]byte, error) {
		var instance domain.ProviderInstance
		if err := json.Unmarshal(raw, &instance); err != nil {
			return nil, err
		}
		normalizeProviderBindings(&instance)
		providerByID[instance.ID] = instance
		return json.Marshal(instance)
	}); err != nil {
		return fmt.Errorf("migrate provider profile bindings: %w", err)
	}
	if err := rewriteBucket(deployments, func(raw []byte) ([]byte, error) {
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return nil, err
		}
		if instance, ok := providerByID[deployment.ProviderID]; ok {
			normalizeDeploymentBinding(&deployment, instance)
		}
		return json.Marshal(deployment)
	}); err != nil {
		return fmt.Errorf("migrate deployment profile bindings: %w", err)
	}
	return migrationStep(step, "after_migrate_provider_profile_bindings")
}

type usageCheckpoint struct {
	Watermark ledger.Watermark `json:"watermark"`
	Payload   []byte           `json:"payload"`
}

type pricingIdempotencyRecord struct {
	KeySHA256     string    `json:"key_sha256"`
	RequestSHA256 string    `json:"request_sha256"`
	PriceID       string    `json:"price_id"`
	AuditEventID  string    `json:"audit_event_id"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Store) PricingIdempotencyRequestSHA256(ctx context.Context, keySHA256 string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	var record pricingIdempotencyRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketPricingIdempotency).Get([]byte(keySHA256))
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &record)
	})
	return record.RequestSHA256, record.KeySHA256 != "", err
}

type AuditCheckpoint struct {
	Records  uint64   `json:"records"`
	Bytes    int64    `json:"bytes"`
	LastHash [32]byte `json:"last_hash"`
}

type Store struct {
	db                       *bbolt.DB
	pricingGates             sync.Map
	pricingClockMu           sync.Mutex
	pricingClockObservations map[string]pricingClockObservation
}

type pricingClockObservation struct {
	SelectedAt time.Time
	ObservedAt time.Time
}

type MetadataInfo struct {
	SchemaVersion              uint64 `json:"schema_version"`
	TxID                       uint64 `json:"txid"`
	MinimumLedgerReaderVersion string `json:"minimum_ledger_reader_version"`
	LedgerFeatureEpoch         uint8  `json:"ledger_feature_epoch"`
}

type LedgerCompatibilityGate struct {
	MinimumReaderVersion string `json:"minimum_reader_version"`
	FeatureEpoch         uint8  `json:"feature_epoch"`
}

func (s *Store) LedgerCompatibilityGate() (LedgerCompatibilityGate, error) {
	var gate LedgerCompatibilityGate
	err := s.db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		gate.MinimumReaderVersion = string(meta.Get(keyMinimumLedgerReaderVersion))
		raw := meta.Get(keyLedgerFeatureEpoch)
		if gate.MinimumReaderVersion == "" || len(raw) != 1 {
			return errors.New("metadata Ledger compatibility gate is missing")
		}
		gate.FeatureEpoch = raw[0]
		return nil
	})
	return gate, err
}

type BootstrapRecords struct {
	Credential domain.Credential
	Provider   domain.ProviderInstance
	Deployment domain.Deployment
	Price      domain.DeploymentPriceVersion
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
	RotationOperationID string `json:"rotation_operation_id,omitempty"`
}

func (k VaultKeyring) Validate() error {
	if k.FormatVersion != 1 || k.ActiveKeyVersion == 0 ||
		!validKeyFingerprint(k.ActiveFingerprint) ||
		(k.PreviousFingerprint != "" && !validKeyFingerprint(k.PreviousFingerprint)) ||
		!validOperationID(k.RotationOperationID) {
		return errors.New("vault keyring is invalid")
	}
	return nil
}

func validOperationID(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
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

func (s *Store) PutKeySlotDescriptor(ctx context.Context, descriptor masterkey.KeySlotDescriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := descriptor.Validate(); err != nil {
		return err
	}
	// Only a pristine descriptor may be created directly. Every subsequent
	// mutation must pass through the operation-specific methods below so an
	// active slot cannot be fabricated without unwrap and candidate verification.
	if descriptor.Revision != 1 || descriptor.ActiveGeneration != 1 || len(descriptor.Slots) != 0 {
		return errors.New("initial key slot descriptor must be pristine")
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyKeySlotDescriptor) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyKeySlotDescriptor, encoded)
	})
}

func (s *Store) KeySlotDescriptor(ctx context.Context) (masterkey.KeySlotDescriptor, error) {
	var descriptor masterkey.KeySlotDescriptor
	if err := ctx.Err(); err != nil {
		return descriptor, err
	}
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyKeySlotDescriptor)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &descriptor); err != nil {
			return fmt.Errorf("decode key slot descriptor: %w", err)
		}
		return descriptor.Validate()
	})
	return descriptor.Clone(), err
}

func (s *Store) AddKeySlot(
	ctx context.Context,
	pending masterkey.PendingKeySlot,
	expectedRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	next, transition, err := current.AddSlot(pending, expectedRevision, now)
	if err != nil || transition == nil {
		return next, transition, err
	}
	if err := s.replaceKeySlotDescriptor(ctx, current.Revision, next); err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	return next, transition, nil
}

func (s *Store) AddKeySlotWithAuditIntent(
	ctx context.Context,
	pending masterkey.PendingKeySlot,
	expectedRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, masterkey.KeySlotAuditIntent, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	next, transition, err := current.AddSlot(pending, expectedRevision, now)
	if err != nil || transition == nil {
		return next, masterkey.KeySlotAuditIntent{}, err
	}
	intent, err := newKeySlotAuditIntent(transition, expectedRevision, 0, "")
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	if err := s.replaceKeySlotDescriptorWithAuditIntent(ctx, current, next, intent); err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	return next, intent, nil
}

func (s *Store) VerifyKeySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	unwrapper masterkey.SlotUnwrapper,
	verifier masterkey.CandidateVerifier,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	next, transition, err := current.VerifySlot(
		ctx, slotID, expectedDescriptorRevision, expectedSlotRevision, unwrapper, verifier, now,
	)
	if err != nil || transition == nil {
		return next, transition, err
	}
	if err := s.replaceKeySlotDescriptor(ctx, current.Revision, next); err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	return next, transition, nil
}

func (s *Store) VerifyKeySlotWithAuditIntent(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	unwrapper masterkey.SlotUnwrapper,
	verifier masterkey.CandidateVerifier,
	now time.Time,
) (masterkey.KeySlotDescriptor, masterkey.KeySlotAuditIntent, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	next, transition, err := current.VerifySlot(ctx, slotID, expectedDescriptorRevision, expectedSlotRevision, unwrapper, verifier, now)
	if err != nil || transition == nil {
		return next, masterkey.KeySlotAuditIntent{}, err
	}
	intent, err := newKeySlotAuditIntent(transition, expectedDescriptorRevision, expectedSlotRevision, "")
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	if err := s.replaceKeySlotDescriptorWithAuditIntent(ctx, current, next, intent); err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	return next, intent, nil
}

func (s *Store) RetireKeySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	return s.transitionKeySlot(ctx, slotID, expectedDescriptorRevision, expectedSlotRevision, masterkey.KeySlotRetiring, now)
}

func (s *Store) RetireKeySlotWithAuditIntent(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, masterkey.KeySlotAuditIntent, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	next, transition, err := current.RetireSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, now)
	if err != nil || transition == nil {
		return next, masterkey.KeySlotAuditIntent{}, err
	}
	intent, err := newKeySlotAuditIntent(transition, expectedDescriptorRevision, expectedSlotRevision, "")
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	if err := s.replaceKeySlotDescriptorWithAuditIntent(ctx, current, next, intent); err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	return next, intent, nil
}

func (s *Store) RevokeKeySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	return s.transitionKeySlot(ctx, slotID, expectedDescriptorRevision, expectedSlotRevision, masterkey.KeySlotRevoked, now)
}

func (s *Store) RevokeKeySlotWithAuditIntent(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	reasonCode string,
	now time.Time,
) (masterkey.KeySlotDescriptor, masterkey.KeySlotAuditIntent, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	next, transition, err := current.RevokeSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, now)
	if err != nil || transition == nil {
		return next, masterkey.KeySlotAuditIntent{}, err
	}
	intent, err := newKeySlotAuditIntent(transition, expectedDescriptorRevision, expectedSlotRevision, reasonCode)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	if err := s.replaceKeySlotDescriptorWithAuditIntent(ctx, current, next, intent); err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	return next, intent, nil
}

func newKeySlotAuditIntent(transition *masterkey.SlotTransition, expectedDescriptorRevision, expectedSlotRevision uint64, reasonCode string) (masterkey.KeySlotAuditIntent, error) {
	if transition == nil {
		return masterkey.KeySlotAuditIntent{}, errors.New("Key Slot transition is required")
	}
	intent := masterkey.KeySlotAuditIntent{
		EventID:    fmt.Sprintf("aud-slot-%s-%d-%d", transition.SlotID, transition.DescriptorRevision, transition.SlotRevision),
		OccurredAt: transition.OccurredAt, Action: transition.AuditAction(), TargetID: transition.SlotID,
		Purpose: transition.Purpose, ReasonCode: reasonCode,
		ExpectedDescriptorRevision: expectedDescriptorRevision, ExpectedSlotRevision: expectedSlotRevision,
		DescriptorRevision: transition.DescriptorRevision, SlotRevision: transition.SlotRevision,
	}
	return intent, intent.Validate()
}

func (s *Store) replaceKeySlotDescriptorWithAuditIntent(ctx context.Context, current, next masterkey.KeySlotDescriptor, intent masterkey.KeySlotAuditIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if err := next.ValidateSuccessor(current); err != nil {
		return err
	}
	descriptorPayload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	intentPayload, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(keyKeySlotAuditIntent); raw != nil {
			var previous masterkey.KeySlotAuditIntent
			if err := json.Unmarshal(raw, &previous); err != nil {
				return err
			}
			if err := previous.Validate(); err != nil {
				return err
			}
			if !previous.Delivered {
				return errors.New("a previous Key Slot audit event is still pending delivery")
			}
		}
		raw := meta.Get(keyKeySlotDescriptor)
		if raw == nil {
			return ErrNotFound
		}
		var persisted masterkey.KeySlotDescriptor
		if err := json.Unmarshal(raw, &persisted); err != nil {
			return err
		}
		if persisted.Revision != current.Revision {
			return ErrRevisionConflict
		}
		if err := persisted.Validate(); err != nil {
			return err
		}
		if err := next.ValidateSuccessor(persisted); err != nil {
			return err
		}
		if err := meta.Put(keyKeySlotDescriptor, descriptorPayload); err != nil {
			return err
		}
		return meta.Put(keyKeySlotAuditIntent, intentPayload)
	})
}

func (s *Store) KeySlotAuditIntent() (masterkey.KeySlotAuditIntent, error) {
	var intent masterkey.KeySlotAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyKeySlotAuditIntent)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		return intent.Validate()
	})
	return intent, err
}

func (s *Store) MarkKeySlotAuditDelivered(ctx context.Context, eventID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyKeySlotAuditIntent)
		if raw == nil {
			return ErrNotFound
		}
		var intent masterkey.KeySlotAuditIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if err := intent.Validate(); err != nil {
			return err
		}
		if intent.EventID != eventID {
			return ErrRevisionConflict
		}
		intent.Delivered = true
		payload, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return meta.Put(keyKeySlotAuditIntent, payload)
	})
}

func (s *Store) transitionKeySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	target masterkey.KeySlotState,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	var next masterkey.KeySlotDescriptor
	var transition *masterkey.SlotTransition
	switch target {
	case masterkey.KeySlotRetiring:
		next, transition, err = current.RetireSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, now)
	case masterkey.KeySlotRevoked:
		next, transition, err = current.RevokeSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, now)
	default:
		return masterkey.KeySlotDescriptor{}, nil, masterkey.ErrInvalidTransition
	}
	if err != nil || transition == nil {
		return next, transition, err
	}
	if err := s.replaceKeySlotDescriptor(ctx, current.Revision, next); err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	return next, transition, nil
}

func (s *Store) replaceKeySlotDescriptor(
	ctx context.Context,
	expectedRevision uint64,
	descriptor masterkey.KeySlotDescriptor,
) error {
	return s.replaceKeySlotDescriptorWithHook(ctx, expectedRevision, descriptor, nil)
}

// KeySlotInitialization is the complete cryptographic root state for a new
// external-KMS instance. It is published in one bbolt transaction so no reader
// can observe a descriptor without its matching Vault Key Check, Keyring,
// protected Audit key and Audit checkpoint.
type KeySlotInitialization struct {
	Descriptor        masterkey.KeySlotDescriptor
	Keyring           VaultKeyring
	VaultKeyCheck     []byte
	AuditHMACEnvelope []byte
	AuditCheckpoint   AuditCheckpoint
	Unwrapper         masterkey.SlotUnwrapper
	Verifier          masterkey.CandidateVerifier
}

func (s *Store) InitializeKeySlotState(ctx context.Context, state KeySlotInitialization) error {
	return s.initializeKeySlotStateWithHook(ctx, state, nil)
}

func (s *Store) initializeKeySlotStateWithHook(ctx context.Context, state KeySlotInitialization, hook func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := state.Descriptor.Validate(); err != nil {
		return err
	}
	if !state.Descriptor.ProductionReady() {
		return errors.New("initial key slot descriptor must be production-ready")
	}
	if err := state.Keyring.Validate(); err != nil {
		return err
	}
	if state.Keyring.ActiveFingerprint != state.Descriptor.MasterKeyFingerprint {
		return errors.New("keyring fingerprint does not match key slot descriptor")
	}
	if state.Unwrapper == nil || state.Verifier == nil {
		return errors.New("key slot initialization requires unwrap and candidate verification")
	}
	for _, slot := range state.Descriptor.Slots {
		if slot.State != masterkey.KeySlotActive || slot.VerifiedAt == nil {
			return errors.New("initial key slot descriptor contains an unverified slot")
		}
		candidate, err := state.Unwrapper.Unwrap(ctx, slot)
		if err != nil {
			return fmt.Errorf("verify initial key slot %q: %w", slot.ID, err)
		}
		fingerprint, fingerprintErr := masterkey.MasterKeyFingerprint(candidate)
		verifyErr := state.Verifier.VerifyCandidate(ctx, candidate)
		clear(candidate)
		if fingerprintErr != nil {
			return fmt.Errorf("verify initial key slot %q fingerprint: %w", slot.ID, fingerprintErr)
		}
		if fingerprint != state.Descriptor.MasterKeyFingerprint {
			return fmt.Errorf("verify initial key slot %q: %w", slot.ID, masterkey.ErrVaultKeyMismatch)
		}
		if verifyErr != nil {
			return fmt.Errorf("verify initial key slot %q candidate: %w", slot.ID, verifyErr)
		}
	}
	if len(state.VaultKeyCheck) == 0 || len(state.AuditHMACEnvelope) == 0 || state.AuditCheckpoint.Bytes < 0 {
		return errors.New("complete Vault and Audit initialization material is required")
	}
	descriptor, err := json.Marshal(state.Descriptor)
	if err != nil {
		return err
	}
	keyring, err := json.Marshal(state.Keyring)
	if err != nil {
		return err
	}
	checkpoint, err := json.Marshal(state.AuditCheckpoint)
	if err != nil {
		return err
	}
	values := []struct {
		name  string
		key   []byte
		value []byte
	}{
		{name: "descriptor", key: keyKeySlotDescriptor, value: descriptor},
		{name: "keyring", key: keyVaultKeyring, value: keyring},
		{name: "vault_key_check", key: keyVaultCheck, value: state.VaultKeyCheck},
		{name: "audit_hmac_envelope", key: keyAuditHMACEnvelope, value: state.AuditHMACEnvelope},
		{name: "audit_checkpoint", key: keyAuditCheckpoint, value: checkpoint},
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		for _, item := range values {
			if meta.Get(item.key) != nil {
				return ErrAlreadyExists
			}
		}
		for _, item := range values {
			if hook != nil {
				if err := hook("before_put_" + item.name); err != nil {
					return err
				}
			}
			if err := meta.Put(item.key, item.value); err != nil {
				return err
			}
			if hook != nil {
				if err := hook("after_put_" + item.name); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Store) replaceKeySlotDescriptorWithHook(
	ctx context.Context,
	expectedRevision uint64,
	descriptor masterkey.KeySlotDescriptor,
	hook func(string) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expectedRevision == 0 {
		return errors.New("expected key slot descriptor revision is required")
	}
	if err := descriptor.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyKeySlotDescriptor)
		if raw == nil {
			return ErrNotFound
		}
		var current masterkey.KeySlotDescriptor
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("decode current key slot descriptor: %w", err)
		}
		if err := current.Validate(); err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if err := descriptor.ValidateSuccessor(current); err != nil {
			return err
		}
		if hook != nil {
			if err := hook("before_put_key_slot_descriptor"); err != nil {
				return err
			}
		}
		if err := meta.Put(keyKeySlotDescriptor, encoded); err != nil {
			return err
		}
		if hook != nil {
			if err := hook("after_put_key_slot_descriptor"); err != nil {
				return err
			}
		}
		return nil
	})
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
	Context           context.Context
	VaultKeyCheck     []byte
	AuditHMACEnvelope []byte
	Keyring           VaultKeyring
	KeySlotDescriptor *masterkey.KeySlotDescriptor
	Transform         func(domain.Credential) (domain.Credential, error)
	TransformAdminMFA func(domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error)
	Unwrapper         masterkey.SlotUnwrapper
	Verifier          masterkey.CandidateVerifier
}

// RewriteVaultMaterial runs only against an offline copy of metadata. All
// credential ciphertext and system envelopes change in one bbolt transaction.
func (s *Store) RewriteVaultMaterial(options VaultRewrite) error {
	if len(options.VaultKeyCheck) == 0 || len(options.AuditHMACEnvelope) == 0 ||
		options.Transform == nil || options.TransformAdminMFA == nil || options.Keyring.Validate() != nil ||
		len(options.Keyring.RecoveryEnvelope) == 0 {
		return errors.New("complete vault rewrite material is required")
	}
	if options.KeySlotDescriptor != nil {
		if options.Context == nil || options.Unwrapper == nil || options.Verifier == nil {
			return errors.New("rotated key slot descriptor requires unwrap and candidate verification")
		}
		if err := options.KeySlotDescriptor.Validate(); err != nil {
			return err
		}
		if options.KeySlotDescriptor.MasterKeyFingerprint != options.Keyring.ActiveFingerprint {
			return errors.New("rotated descriptor and keyring fingerprints do not match")
		}
		for _, slot := range options.KeySlotDescriptor.Slots {
			candidate, err := options.Unwrapper.Unwrap(options.Context, slot)
			if err != nil {
				return fmt.Errorf("verify rotated key slot %q: %w", slot.ID, err)
			}
			fingerprint, fingerprintErr := masterkey.MasterKeyFingerprint(candidate)
			verifyErr := options.Verifier.VerifyCandidate(options.Context, candidate)
			clear(candidate)
			if fingerprintErr != nil {
				return fmt.Errorf("verify rotated key slot %q fingerprint: %w", slot.ID, fingerprintErr)
			}
			if fingerprint != options.KeySlotDescriptor.MasterKeyFingerprint {
				return fmt.Errorf("verify rotated key slot %q: %w", slot.ID, masterkey.ErrVaultKeyMismatch)
			}
			if verifyErr != nil {
				return fmt.Errorf("verify rotated key slot %q candidate: %w", slot.ID, verifyErr)
			}
		}
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if options.KeySlotDescriptor != nil {
			raw := tx.Bucket(bucketMeta).Get(keyKeySlotDescriptor)
			if raw == nil {
				return ErrNotFound
			}
			var previous masterkey.KeySlotDescriptor
			if err := json.Unmarshal(raw, &previous); err != nil {
				return fmt.Errorf("decode previous key slot descriptor: %w", err)
			}
			if err := options.KeySlotDescriptor.ValidateRotationSuccessor(previous); err != nil {
				return err
			}
		}
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
		if options.KeySlotDescriptor != nil {
			encodedDescriptor, err := json.Marshal(options.KeySlotDescriptor)
			if err != nil {
				return err
			}
			if err := meta.Put(keyKeySlotDescriptor, encodedDescriptor); err != nil {
				return err
			}
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

func (s *Store) EnsureMasterKeyRotationAuditIntent(ctx context.Context, operationID string, now time.Time) (masterkey.MasterKeyRotationAuditIntent, error) {
	requested, err := masterkey.NewMasterKeyRotationAuditIntent(operationID, now)
	if err != nil {
		return masterkey.MasterKeyRotationAuditIntent{}, err
	}
	var result masterkey.MasterKeyRotationAuditIntent
	err = s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(keyMasterKeyRotationAuditIntent); raw != nil {
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			if err := result.Validate(); err != nil {
				return err
			}
			if result.OperationID == operationID {
				return nil
			}
			if !result.CompletedDelivered {
				return errors.New("a different Master Key rotation Audit intent is still pending")
			}
		}
		payload, err := json.Marshal(requested)
		if err != nil {
			return err
		}
		if err := meta.Put(keyMasterKeyRotationAuditIntent, payload); err != nil {
			return err
		}
		result = requested
		return nil
	})
	return result, err
}

func (s *Store) MasterKeyRotationAuditIntent() (masterkey.MasterKeyRotationAuditIntent, error) {
	var intent masterkey.MasterKeyRotationAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyMasterKeyRotationAuditIntent)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		return intent.Validate()
	})
	return intent, err
}

func (s *Store) MarkMasterKeyRotationAuditDelivered(ctx context.Context, eventID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyMasterKeyRotationAuditIntent)
		if raw == nil {
			return ErrNotFound
		}
		var intent masterkey.MasterKeyRotationAuditIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if err := intent.Validate(); err != nil {
			return err
		}
		switch eventID {
		case intent.StartedEventID:
			intent.StartedDelivered = true
		case intent.CompletedEventID:
			if intent.CompletedAt == nil || !intent.StartedDelivered {
				return ErrRevisionConflict
			}
			intent.CompletedDelivered = true
		default:
			return ErrRevisionConflict
		}
		payload, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return meta.Put(keyMasterKeyRotationAuditIntent, payload)
	})
}

func (s *Store) ClearVaultRotationBridgeWithAuditIntent(ctx context.Context, operationID string, now time.Time) (masterkey.MasterKeyRotationAuditIntent, error) {
	var completed masterkey.MasterKeyRotationAuditIntent
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		rawKeyring := meta.Get(keyVaultKeyring)
		if rawKeyring == nil {
			return ErrNotFound
		}
		var keyring VaultKeyring
		if err := json.Unmarshal(rawKeyring, &keyring); err != nil {
			return err
		}
		if err := keyring.Validate(); err != nil {
			return err
		}
		if keyring.RotationOperationID != operationID || len(keyring.RecoveryEnvelope) == 0 {
			return errors.New("Master Key rotation bridge does not match the requested operation")
		}
		rawIntent := meta.Get(keyMasterKeyRotationAuditIntent)
		if rawIntent == nil {
			return ErrNotFound
		}
		var intent masterkey.MasterKeyRotationAuditIntent
		if err := json.Unmarshal(rawIntent, &intent); err != nil {
			return err
		}
		if intent.OperationID != operationID {
			return ErrRevisionConflict
		}
		var err error
		completed, err = intent.WithCompletion(now)
		if err != nil {
			return err
		}
		keyring.RecoveryEnvelope = nil
		encodedKeyring, err := json.Marshal(keyring)
		if err != nil {
			return err
		}
		encodedIntent, err := json.Marshal(completed)
		if err != nil {
			return err
		}
		if err := meta.Put(keyVaultKeyring, encodedKeyring); err != nil {
			return err
		}
		return meta.Put(keyMasterKeyRotationAuditIntent, encodedIntent)
	})
	return completed, err
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
		records.Price.Validate(),
		records.Route.Validate(),
		records.Project.Validate(),
		records.GatewayKey.Validate(),
	); err != nil {
		return err
	}
	if records.Provider.CredentialID != records.Credential.ID ||
		records.Deployment.ProviderID != records.Provider.ID ||
		records.Price.DeploymentID != records.Deployment.ID || records.Price.Version != 1 || records.Price.Revision != 1 ||
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
				if err := putDeploymentPriceVersionTx(tx, records.Price); err != nil {
					return err
				}
				return tx.Bucket(bucketDeploymentPriceNext).Put([]byte(records.Deployment.ID), versionKey(records.Price.Version))
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
	normalizeProviderBindings(instance)
}

func normalizeProviderBindings(instance *domain.ProviderInstance) {
	if len(instance.Bindings) == 0 && instance.ProfileID != "" {
		instance.Bindings = []domain.ProviderProfileBinding{{
			ID: domain.DefaultProviderProfileBindingID(instance.ID, instance.ProfileID), ProviderID: instance.ID,
			ProfileID: instance.ProfileID, AccessSurface: instance.AccessSurface,
			CredentialScheme: instance.CredentialScheme, Capabilities: instance.Capabilities,
			CapabilityEvidence: instance.CapabilityEvidence.Clone(), Enabled: instance.Enabled,
		}}
	}
	if len(instance.Bindings) != 0 {
		instance.Capabilities, instance.CapabilityEvidence = domain.BindingsCapabilitiesSummary(instance.Bindings)
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
	normalizeDeploymentBinding(deployment, instance)
}

func normalizeDeploymentBinding(deployment *domain.Deployment, instance domain.ProviderInstance) {
	if deployment.BindingID != "" {
		return
	}
	for _, binding := range instance.EffectiveProfileBindings() {
		if binding.ProfileID == deployment.ProfileID && binding.AccessSurface == deployment.AccessSurface {
			deployment.BindingID = binding.ID
			return
		}
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
		bucketDeploymentPriceVersions,
		bucketDeploymentPriceTimeline,
		bucketDeploymentPriceNext,
		bucketDeploymentPricingHighWater,
		bucketDeploymentPricePins,
		bucketPricingAuditIntents,
		bucketPricingIdempotency,
		bucketDeploymentPriceProposals,
		bucketPricingProposalIdempotency,
		bucketCostAdjustmentIntents,
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
			MinimumLedgerReaderVersion: string(tx.Bucket(bucketMeta).Get(keyMinimumLedgerReaderVersion)),
		}
		if epoch := tx.Bucket(bucketMeta).Get(keyLedgerFeatureEpoch); len(epoch) == 1 {
			info.LedgerFeatureEpoch = epoch[0]
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
	if err == nil {
		normalizeProviderBindings(&provider)
	}
	return provider, err
}

func (s *Store) ListProviders(ctx context.Context) ([]domain.ProviderInstance, error) {
	var providers []domain.ProviderInstance
	err := s.listJSON(ctx, bucketProviders, func(raw []byte) error {
		var provider domain.ProviderInstance
		if err := json.Unmarshal(raw, &provider); err != nil {
			return err
		}
		normalizeProviderBindings(&provider)
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
	bindingID := deployment.BindingID
	if bindingID == "" {
		bindingID = domain.DefaultProviderProfileBindingID(instance.ID, deployment.ProfileID)
	}
	binding, ok := instance.ProfileBinding(bindingID)
	if !ok {
		return errors.New("deployment provider profile binding was not found")
	}
	if deployment.AccessSurface != binding.AccessSurface || deployment.ProfileID != binding.ProfileID {
		return errors.New("deployment access surface or profile does not match provider")
	}
	if !domain.ProviderCapabilitiesSubset(deployment.Capabilities, binding.Capabilities) {
		return errors.New("deployment capabilities exceed provider capabilities")
	}
	for name, value := range deployment.CapabilityEvidence {
		providerValue, ok := binding.CapabilityEvidence[name]
		if !ok || capabilityEvidenceRank(value) > capabilityEvidenceRank(providerValue) {
			return errors.New("deployment capability evidence exceeds provider evidence")
		}
	}
	return nil
}

func providerCapabilitySubset(candidate, available domain.ProviderCapabilities) bool {
	return domain.ProviderCapabilitiesSubset(candidate, available)
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
	if err == nil && deployment.BindingID == "" {
		if instance, providerErr := s.GetProvider(ctx, deployment.ProviderID); providerErr == nil {
			normalizeDeploymentBinding(&deployment, instance)
		}
	}
	return deployment, err
}

func (s *Store) ListDeployments(ctx context.Context) ([]domain.Deployment, error) {
	providers, err := s.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	providerByID := make(map[string]domain.ProviderInstance, len(providers))
	for _, instance := range providers {
		providerByID[instance.ID] = instance
	}
	var deployments []domain.Deployment
	err = s.listJSON(ctx, bucketDeployments, func(raw []byte) error {
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		if instance, ok := providerByID[deployment.ProviderID]; ok {
			normalizeDeploymentBinding(&deployment, instance)
		}
		deployments = append(deployments, deployment)
		return nil
	})
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].ID < deployments[j].ID })
	return deployments, err
}

func (s *Store) CreateDeploymentPriceVersion(ctx context.Context, price domain.DeploymentPriceVersion) (domain.DeploymentPriceVersion, error) {
	created, _, _, err := s.createDeploymentPriceVersion(ctx, price, nil, "")
	return created, err
}

func (s *Store) CreateDeploymentPriceVersionWithAuditIntent(ctx context.Context, price domain.DeploymentPriceVersion, intent domain.PricingAuditIntent) (domain.DeploymentPriceVersion, error) {
	if err := intent.Validate(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	if intent.Action != "deployment_price.create" || intent.TargetID != price.ID {
		return domain.DeploymentPriceVersion{}, errors.New("pricing audit intent does not match price creation")
	}
	created, _, _, err := s.createDeploymentPriceVersion(ctx, price, &intent, "")
	return created, err
}

func (s *Store) CreateDeploymentPriceVersionIdempotent(ctx context.Context, price domain.DeploymentPriceVersion, intent domain.PricingAuditIntent, keySHA256 string) (domain.DeploymentPriceVersion, domain.PricingAuditIntent, bool, error) {
	if err := intent.Validate(); err != nil {
		return domain.DeploymentPriceVersion{}, domain.PricingAuditIntent{}, false, err
	}
	if intent.Action != "deployment_price.create" || intent.TargetID != price.ID || !validSHA256Label(keySHA256) {
		return domain.DeploymentPriceVersion{}, domain.PricingAuditIntent{}, false, errors.New("invalid idempotent pricing mutation")
	}
	created, effectiveIntent, replayed, err := s.createDeploymentPriceVersion(ctx, price, &intent, keySHA256)
	if effectiveIntent == nil {
		return created, domain.PricingAuditIntent{}, replayed, err
	}
	return created, *effectiveIntent, replayed, err
}

func (s *Store) createDeploymentPriceVersion(ctx context.Context, price domain.DeploymentPriceVersion, intent *domain.PricingAuditIntent, keySHA256 string) (domain.DeploymentPriceVersion, *domain.PricingAuditIntent, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentPriceVersion{}, nil, false, err
	}
	if strings.TrimSpace(price.ID) == "" || strings.TrimSpace(price.DeploymentID) == "" {
		return domain.DeploymentPriceVersion{}, nil, false, errors.New("price version id and deployment id are required")
	}
	if price.Version != 0 || price.Revision != 0 || price.CancelledAt != nil || price.CancelledBy != "" {
		return domain.DeploymentPriceVersion{}, nil, false, errors.New("new price version must not set version, revision, or cancellation metadata")
	}
	var effectiveIntent *domain.PricingAuditIntent
	replayed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if keySHA256 != "" {
			idempotency := tx.Bucket(bucketPricingIdempotency)
			if raw := idempotency.Get([]byte(keySHA256)); raw != nil {
				var previous pricingIdempotencyRecord
				if err := json.Unmarshal(raw, &previous); err != nil {
					return err
				}
				if intent == nil || previous.RequestSHA256 != intent.RequestSHA256 {
					return ErrIdempotencyConflict
				}
				priceRaw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(previous.PriceID))
				intentRaw := tx.Bucket(bucketPricingAuditIntents).Get([]byte(previous.AuditEventID))
				if priceRaw == nil || intentRaw == nil {
					return errors.New("pricing idempotency record references missing state")
				}
				if err := json.Unmarshal(priceRaw, &price); err != nil {
					return err
				}
				var storedIntent domain.PricingAuditIntent
				if err := json.Unmarshal(intentRaw, &storedIntent); err != nil {
					return err
				}
				effectiveIntent, replayed = &storedIntent, true
				return nil
			}
		}
		if tx.Bucket(bucketDeployments).Get([]byte(price.DeploymentID)) == nil {
			return fmt.Errorf("deployment %q: %w", price.DeploymentID, ErrNotFound)
		}
		prices := tx.Bucket(bucketDeploymentPriceVersions)
		if prices.Get([]byte(price.ID)) != nil {
			return ErrAlreadyExists
		}
		nextVersions := tx.Bucket(bucketDeploymentPriceNext)
		var current uint64
		if raw := nextVersions.Get([]byte(price.DeploymentID)); raw != nil {
			if len(raw) != 8 {
				return errors.New("invalid deployment price next-version state")
			}
			current = binary.BigEndian.Uint64(raw)
		}
		if current == ^uint64(0) {
			return errors.New("deployment price version overflow")
		}
		price.Version = current + 1
		price.Revision = 1
		if err := price.Validate(); err != nil {
			return err
		}
		timelineRoot := tx.Bucket(bucketDeploymentPriceTimeline)
		timeline, err := timelineRoot.CreateBucketIfNotExists([]byte(price.DeploymentID))
		if err != nil {
			return err
		}
		key := deploymentPriceTimelineKey(price.EffectiveFrom)
		if timeline.Get(key) != nil {
			return domain.ErrPriceTimelineConflict
		}
		var latest domain.DeploymentPriceVersion
		if err := timeline.ForEach(func(_, priceID []byte) error {
			if priceID == nil {
				return nil
			}
			raw := prices.Get(priceID)
			if raw == nil {
				return errors.New("deployment price timeline references a missing record")
			}
			var existing domain.DeploymentPriceVersion
			if err := json.Unmarshal(raw, &existing); err != nil {
				return err
			}
			if existing.CancelledAt == nil && (latest.ID == "" || existing.EffectiveFrom.After(latest.EffectiveFrom)) {
				latest = existing
			}
			return nil
		}); err != nil {
			return err
		}
		if latest.ID != "" && !price.EffectiveFrom.After(latest.EffectiveFrom) {
			return fmt.Errorf("%w: effective_from must follow all non-cancelled versions", domain.ErrPriceTimelineConflict)
		}
		versionCount, scheduledCount := 0, 0
		if err := timeline.ForEach(func(_, priceID []byte) error {
			if priceID == nil {
				return nil
			}
			versionCount++
			var existing domain.DeploymentPriceVersion
			if err := json.Unmarshal(prices.Get(priceID), &existing); err != nil {
				return err
			}
			if existing.CancelledAt == nil && existing.EffectiveFrom.After(price.CreatedAt) {
				scheduledCount++
			}
			return nil
		}); err != nil {
			return err
		}
		if versionCount >= maxPriceVersionsPerDeployment || (price.EffectiveFrom.After(price.CreatedAt) && scheduledCount >= maxScheduledPricesPerDeployment) {
			return errors.New("deployment pricing retention capacity exceeded")
		}
		if err := putDeploymentPriceVersionTx(tx, price); err != nil {
			return err
		}
		if err := nextVersions.Put([]byte(price.DeploymentID), versionKey(price.Version)); err != nil {
			return err
		}
		if intent != nil {
			effective := price.EffectiveFrom.UTC()
			intent.DeploymentID, intent.PriceVersion, intent.EffectiveFrom = price.DeploymentID, price.Version, &effective
			intent.SourceType, intent.SourceContentSHA256 = price.Source.Type, price.Source.ContentSHA256
			intent.ChangeSummary = fmt.Sprintf("before=none after={billing:%s,input:%d,output:%d,fixed:%d}", price.BillingMode, price.InputMicrosPerMillion, price.OutputMicrosPerMillion, price.FixedRequestMicrosUSD)
			if err := putPricingAuditIntentTx(tx, *intent); err != nil {
				return err
			}
			copy := *intent
			effectiveIntent = &copy
		}
		if keySHA256 != "" {
			if tx.Bucket(bucketPricingIdempotency).Stats().KeyN >= maxPricingIdempotencyRecords {
				return errors.New("pricing idempotency retention capacity exceeded")
			}
			record := pricingIdempotencyRecord{
				KeySHA256: keySHA256, RequestSHA256: intent.RequestSHA256, PriceID: price.ID,
				AuditEventID: intent.EventID, CreatedAt: intent.OccurredAt,
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				return err
			}
			return tx.Bucket(bucketPricingIdempotency).Put([]byte(keySHA256), encoded)
		}
		return nil
	})
	return price, effectiveIntent, replayed, err
}

func (s *Store) GetDeploymentPriceVersion(ctx context.Context, deploymentID, priceID string) (domain.DeploymentPriceVersion, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	var price domain.DeploymentPriceVersion
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(priceID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &price); err != nil {
			return err
		}
		if price.DeploymentID != deploymentID {
			return ErrNotFound
		}
		return price.Validate()
	})
	return price, err
}

func (s *Store) ListDeploymentPriceVersions(ctx context.Context, deploymentID string) ([]domain.DeploymentPriceVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var prices []domain.DeploymentPriceVersion
	err := s.db.View(func(tx *bbolt.Tx) error {
		timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(deploymentID))
		if timeline == nil {
			if tx.Bucket(bucketDeployments).Get([]byte(deploymentID)) == nil {
				return ErrNotFound
			}
			return nil
		}
		return timeline.ForEach(func(_, priceID []byte) error {
			if priceID == nil {
				return nil
			}
			raw := tx.Bucket(bucketDeploymentPriceVersions).Get(priceID)
			if raw == nil {
				return errors.New("deployment price timeline references a missing record")
			}
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(raw, &price); err != nil {
				return err
			}
			if price.DeploymentID != deploymentID {
				return errors.New("deployment price timeline references another deployment")
			}
			if err := price.Validate(); err != nil {
				return err
			}
			prices = append(prices, price)
			return nil
		})
	})
	return prices, err
}

func (s *Store) SelectDeploymentPriceVersion(ctx context.Context, deploymentID string, selectedAt time.Time) (domain.DeploymentPriceVersion, error) {
	prices, err := s.ListDeploymentPriceVersions(ctx, deploymentID)
	if err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	return domain.SelectDeploymentPriceVersion(prices, deploymentID, selectedAt)
}

func (s *Store) LockDeploymentPricing(deploymentID string) func() {
	value, _ := s.pricingGates.LoadOrStore(deploymentID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (s *Store) PricingReadiness(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeploymentPricingHighWater).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var highWater domain.DeploymentPricingHighWater
			if err := json.Unmarshal(raw, &highWater); err != nil {
				return err
			}
			if err := highWater.Validate(); err != nil {
				return err
			}
			if highWater.Quarantined {
				return fmt.Errorf("deployment %q: %w", highWater.DeploymentID, ErrPricingQuarantined)
			}
			return nil
		})
	})
}

func (s *Store) PricingQuarantineCount(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeploymentPricingHighWater).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var highWater domain.DeploymentPricingHighWater
			if err := json.Unmarshal(raw, &highWater); err != nil {
				return err
			}
			if highWater.Quarantined {
				count++
			}
			return nil
		})
	})
	return count, err
}

func (s *Store) DeploymentPricingQuarantine(ctx context.Context, deploymentID string) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	var high domain.DeploymentPricingHighWater
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte(deploymentID))
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &high)
	})
	return high.Quarantined, high.QuarantineReason, err
}

func (s *Store) QuarantineRestoredScheduledPrices(ctx context.Context, backupCreatedAt, restoredAt time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	backupCreatedAt, restoredAt = backupCreatedAt.UTC(), restoredAt.UTC()
	if backupCreatedAt.IsZero() || !restoredAt.After(backupCreatedAt) {
		return 0, nil
	}
	count := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		due := make(map[string]domain.DeploymentPriceVersion)
		if err := tx.Bucket(bucketDeploymentPriceVersions).ForEach(func(_, raw []byte) error {
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(raw, &price); err != nil {
				return err
			}
			if price.EffectiveFrom.After(backupCreatedAt) && !price.EffectiveFrom.After(restoredAt) &&
				(price.CancelledAt == nil || price.CancelledAt.After(backupCreatedAt)) {
				if current, ok := due[price.DeploymentID]; !ok || current.EffectiveFrom.Before(price.EffectiveFrom) {
					due[price.DeploymentID] = price
				}
			}
			return nil
		}); err != nil {
			return err
		}
		highs := tx.Bucket(bucketDeploymentPricingHighWater)
		for deploymentID, price := range due {
			var high domain.DeploymentPricingHighWater
			if raw := highs.Get([]byte(deploymentID)); raw != nil {
				if err := json.Unmarshal(raw, &high); err != nil {
					return err
				}
				if high.Quarantined {
					continue
				}
				high.Quarantined = true
				high.QuarantineReason = "restored_scheduled_price_requires_confirmation"
				high.Revision++
			} else {
				high = domain.DeploymentPricingHighWater{
					DeploymentID: deploymentID, LatestObservedPriceVersionID: price.ID,
					LatestSelectedAt: restoredAt, LatestObservedEffectiveFrom: price.EffectiveFrom,
					Quarantined: true, QuarantineReason: "restored_scheduled_price_requires_confirmation", Revision: 1,
				}
			}
			if err := high.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(high)
			if err != nil {
				return err
			}
			if err := highs.Put([]byte(deploymentID), encoded); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) ConfirmRestoredPricing(ctx context.Context, deploymentID string) error {
	return s.confirmRestoredPricing(ctx, deploymentID, nil)

}

func (s *Store) ConfirmRestoredPricingWithAuditIntent(ctx context.Context, deploymentID string, intent domain.PricingAuditIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if intent.Action != "deployment_price.restore_confirm" || intent.TargetID != deploymentID {
		return errors.New("pricing audit intent does not match restore confirmation")
	}
	return s.confirmRestoredPricing(ctx, deploymentID, &intent)
}

func (s *Store) confirmRestoredPricing(ctx context.Context, deploymentID string, intent *domain.PricingAuditIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeploymentPricingHighWater)
		raw := bucket.Get([]byte(deploymentID))
		if raw == nil {
			return ErrNotFound
		}
		var high domain.DeploymentPricingHighWater
		if err := json.Unmarshal(raw, &high); err != nil {
			return err
		}
		if !high.Quarantined || !strings.HasPrefix(high.QuarantineReason, "restored_") {
			return ErrRevisionConflict
		}
		high.Quarantined = false
		high.QuarantineReason = ""
		high.Revision++
		if err := high.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(high)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(deploymentID), encoded); err != nil {
			return err
		}
		if intent != nil {
			return putPricingAuditIntentTx(tx, *intent)
		}
		return nil
	})
}

func (s *Store) PrepareDeploymentPricePin(ctx context.Context, deploymentID, attemptID string, selectedAt time.Time, rollbackTolerance, forwardTolerance time.Duration) (domain.DeploymentPriceVersion, domain.PriceSnapshot, domain.PricePinIntent, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, err
	}
	selectedAt = selectedAt.UTC()
	if deploymentID == "" || attemptID == "" || selectedAt.IsZero() || rollbackTolerance < 0 || forwardTolerance <= 0 {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, errors.New("deployment, attempt, UTC selection time, and valid clock tolerances are required")
	}
	observedAt := time.Now()
	s.pricingClockMu.Lock()
	defer s.pricingClockMu.Unlock()
	if s.pricingClockObservations == nil {
		s.pricingClockObservations = make(map[string]pricingClockObservation)
	}
	previousObservation, hasObservation := s.pricingClockObservations[deploymentID]
	forwardJump := false
	if hasObservation {
		elapsed := observedAt.Sub(previousObservation.ObservedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		forwardJump = selectedAt.After(previousObservation.SelectedAt.Add(elapsed).Add(forwardTolerance))
	}
	var price domain.DeploymentPriceVersion
	var snapshot domain.PriceSnapshot
	var intent domain.PricePinIntent
	var quarantined bool
	err := s.db.Update(func(tx *bbolt.Tx) error {
		highWaterBucket := tx.Bucket(bucketDeploymentPricingHighWater)
		var highWater domain.DeploymentPricingHighWater
		if raw := highWaterBucket.Get([]byte(deploymentID)); raw != nil {
			if err := json.Unmarshal(raw, &highWater); err != nil {
				return err
			}
			if err := highWater.Validate(); err != nil {
				return err
			}
			if highWater.Quarantined {
				return ErrPricingQuarantined
			}
			if forwardJump {
				highWater.Quarantined = true
				highWater.QuarantineReason = "wall_clock_forward_jump"
				highWater.Revision++
				encoded, err := json.Marshal(highWater)
				if err != nil {
					return err
				}
				if err := highWaterBucket.Put([]byte(deploymentID), encoded); err != nil {
					return err
				}
				quarantined = true
				return nil
			}
			if selectedAt.Before(highWater.LatestSelectedAt) {
				rollback := highWater.LatestSelectedAt.Sub(selectedAt)
				if rollback > rollbackTolerance {
					highWater.Quarantined = true
					highWater.QuarantineReason = "wall_clock_rollback"
					highWater.Revision++
					encoded, err := json.Marshal(highWater)
					if err != nil {
						return err
					}
					if err := highWaterBucket.Put([]byte(deploymentID), encoded); err != nil {
						return err
					}
					quarantined = true
					return nil
				}
				selectedAt = highWater.LatestSelectedAt
			}
		}
		var err error
		price, err = selectDeploymentPriceVersionTx(tx, deploymentID, selectedAt)
		if err != nil {
			return err
		}
		snapshot, err = domain.NewVersionedPriceSnapshot(price, selectedAt)
		if err != nil {
			return err
		}
		digest, err := snapshot.Digest()
		if err != nil {
			return err
		}
		var deployment domain.Deployment
		if raw := tx.Bucket(bucketDeployments).Get([]byte(deploymentID)); raw == nil {
			return ErrNotFound
		} else if err := json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		pins := tx.Bucket(bucketDeploymentPricePins)
		if raw := pins.Get([]byte(attemptID)); raw != nil {
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if intent.DeploymentID != deploymentID || intent.PriceVersionID != price.ID || intent.SnapshotSHA256 != digest {
				return ErrIdempotencyConflict
			}
			return nil
		}
		now := time.Now().UTC()
		intent = domain.PricePinIntent{
			AttemptID: attemptID, DeploymentID: deploymentID, PriceVersionID: price.ID, PriceVersion: price.Version,
			SnapshotSHA256: digest, PricingSelectedAt: selectedAt, MetadataRevision: deployment.Revision,
			State: domain.PricePinPrepared, CreatedAt: now,
		}
		if err := intent.Validate(); err != nil {
			return err
		}
		encodedIntent, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		if err := pins.Put([]byte(attemptID), encodedIntent); err != nil {
			return err
		}
		if highWater.DeploymentID == "" {
			highWater = domain.DeploymentPricingHighWater{DeploymentID: deploymentID, LatestObservedPriceVersionID: price.ID, LatestSelectedAt: selectedAt, LatestObservedEffectiveFrom: price.EffectiveFrom, Revision: 1}
		} else {
			highWater.LatestSelectedAt = selectedAt
			if price.EffectiveFrom.After(highWater.LatestObservedEffectiveFrom) {
				highWater.LatestObservedEffectiveFrom = price.EffectiveFrom
				highWater.LatestObservedPriceVersionID = price.ID
			}
			highWater.Revision++
		}
		if err := highWater.Validate(); err != nil {
			return err
		}
		encodedHighWater, err := json.Marshal(highWater)
		if err != nil {
			return err
		}
		return highWaterBucket.Put([]byte(deploymentID), encodedHighWater)
	})
	if err != nil {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, err
	}
	if quarantined {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, ErrPricingQuarantined
	}
	s.pricingClockObservations[deploymentID] = pricingClockObservation{SelectedAt: selectedAt, ObservedAt: observedAt}
	return price, snapshot, intent, nil
}

func (s *Store) CommitDeploymentPricePin(ctx context.Context, attemptID, snapshotSHA256 string, ledgerSequence uint64, committedAt time.Time) (domain.PricePinIntent, error) {
	if err := ctx.Err(); err != nil {
		return domain.PricePinIntent{}, err
	}
	var intent domain.PricePinIntent
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeploymentPricePins)
		raw := bucket.Get([]byte(attemptID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if intent.SnapshotSHA256 != snapshotSHA256 {
			return ErrIdempotencyConflict
		}
		if intent.State == domain.PricePinCommitted {
			if intent.LedgerSequence != ledgerSequence {
				return ErrIdempotencyConflict
			}
			return nil
		}
		value := committedAt.UTC()
		intent.State, intent.LedgerSequence, intent.CommittedAt = domain.PricePinCommitted, ledgerSequence, &value
		if err := intent.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(attemptID), encoded)
	})
	return intent, err
}

func (s *Store) DeletePreparedDeploymentPricePin(ctx context.Context, attemptID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeploymentPricePins)
		raw := bucket.Get([]byte(attemptID))
		if raw == nil {
			return nil
		}
		var intent domain.PricePinIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if intent.State != domain.PricePinPrepared {
			return domain.ErrPriceVersionUnavailable
		}
		return bucket.Delete([]byte(attemptID))
	})
}

func quarantineIncoherentPricingHighWatersTx(tx *bbolt.Tx) error {
	highWaterBucket := tx.Bucket(bucketDeploymentPricingHighWater)
	updates := make(map[string]domain.DeploymentPricingHighWater)
	if err := highWaterBucket.ForEach(func(key, raw []byte) error {
		if raw == nil {
			return nil
		}
		var highWater domain.DeploymentPricingHighWater
		if err := json.Unmarshal(raw, &highWater); err != nil {
			return err
		}
		if err := highWater.Validate(); err != nil {
			return err
		}
		if highWater.Quarantined {
			return nil
		}
		var price domain.DeploymentPriceVersion
		priceRaw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(highWater.LatestObservedPriceVersionID))
		incoherent := priceRaw == nil
		if !incoherent {
			if err := json.Unmarshal(priceRaw, &price); err != nil {
				return err
			}
			incoherent = price.DeploymentID != highWater.DeploymentID ||
				!price.EffectiveFrom.Equal(highWater.LatestObservedEffectiveFrom)
		}
		if incoherent {
			highWater.Quarantined = true
			highWater.QuarantineReason = "restored_high_water_incoherent"
			highWater.Revision++
			updates[string(key)] = highWater
		}
		return nil
	}); err != nil {
		return err
	}
	for key, highWater := range updates {
		encoded, err := json.Marshal(highWater)
		if err != nil {
			return err
		}
		if err := highWaterBucket.Put([]byte(key), encoded); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecoverDeploymentPricePins(ctx context.Context, state *ledger.State) error {
	if state == nil {
		return errors.New("ledger state is required for price pin recovery")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := quarantineIncoherentPricingHighWatersTx(tx); err != nil {
			return err
		}
		bucket := tx.Bucket(bucketDeploymentPricePins)
		var deletes [][]byte
		var updates = make(map[string]domain.PricePinIntent)
		if err := bucket.ForEach(func(key, raw []byte) error {
			if raw == nil {
				return nil
			}
			var intent domain.PricePinIntent
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if err := intent.Validate(); err != nil {
				return err
			}
			lease, exists := state.AccountingLease(intent.AttemptID)
			if !exists {
				if intent.State == domain.PricePinPrepared {
					deletes = append(deletes, bytes.Clone(key))
					return nil
				}
				return fmt.Errorf("committed price pin %q has no Ledger lease", intent.AttemptID)
			}
			if lease.Event.PriceSnapshot == nil {
				return fmt.Errorf("price pin %q Ledger lease has no snapshot", intent.AttemptID)
			}
			digest, err := lease.Event.PriceSnapshot.Digest()
			if err != nil || digest != intent.SnapshotSHA256 {
				return fmt.Errorf("price pin %q snapshot digest mismatch: %w", intent.AttemptID, err)
			}
			if intent.State == domain.PricePinPrepared {
				now := time.Now().UTC()
				intent.State, intent.LedgerSequence, intent.CommittedAt = domain.PricePinCommitted, lease.Sequence, &now
				updates[string(key)] = intent
			} else if intent.LedgerSequence != lease.Sequence {
				return fmt.Errorf("price pin %q Ledger sequence mismatch", intent.AttemptID)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range deletes {
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		for key, intent := range updates {
			if err := intent.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(intent)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(key), encoded); err != nil {
				return err
			}
		}
		return nil
	})
}

func selectDeploymentPriceVersionTx(tx *bbolt.Tx, deploymentID string, selectedAt time.Time) (domain.DeploymentPriceVersion, error) {
	timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(deploymentID))
	if timeline == nil {
		return domain.DeploymentPriceVersion{}, domain.ErrPriceUnavailable
	}
	cursor := timeline.Cursor()
	key, priceID := cursor.Seek(deploymentPriceTimelineKey(selectedAt))
	if key == nil || bytes.Compare(key, deploymentPriceTimelineKey(selectedAt)) > 0 {
		key, priceID = cursor.Prev()
	}
	for key != nil {
		raw := tx.Bucket(bucketDeploymentPriceVersions).Get(priceID)
		if raw == nil {
			return domain.DeploymentPriceVersion{}, errors.New("deployment price timeline references a missing record")
		}
		var price domain.DeploymentPriceVersion
		if err := json.Unmarshal(raw, &price); err != nil {
			return domain.DeploymentPriceVersion{}, err
		}
		if price.CancelledAt == nil && !price.EffectiveFrom.After(selectedAt) {
			return price, price.Validate()
		}
		key, priceID = cursor.Prev()
	}
	return domain.DeploymentPriceVersion{}, domain.ErrPriceUnavailable
}

func (s *Store) CancelDeploymentPriceVersion(ctx context.Context, deploymentID, priceID, actor string, cancelledAt time.Time, expectedRevision uint64) (domain.DeploymentPriceVersion, error) {
	return s.cancelDeploymentPriceVersion(ctx, deploymentID, priceID, actor, cancelledAt, expectedRevision, nil)
}

func (s *Store) CancelDeploymentPriceVersionWithAuditIntent(ctx context.Context, deploymentID, priceID, actor string, cancelledAt time.Time, expectedRevision uint64, intent domain.PricingAuditIntent) (domain.DeploymentPriceVersion, error) {
	if err := intent.Validate(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	if intent.Action != "deployment_price.cancel" || intent.TargetID != priceID || intent.ActorID != strings.TrimSpace(actor) {
		return domain.DeploymentPriceVersion{}, errors.New("pricing audit intent does not match price cancellation")
	}
	return s.cancelDeploymentPriceVersion(ctx, deploymentID, priceID, actor, cancelledAt, expectedRevision, &intent)
}

func (s *Store) cancelDeploymentPriceVersion(ctx context.Context, deploymentID, priceID, actor string, cancelledAt time.Time, expectedRevision uint64, intent *domain.PricingAuditIntent) (domain.DeploymentPriceVersion, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	var price domain.DeploymentPriceVersion
	err := s.db.Update(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(priceID))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &price); err != nil {
			return err
		}
		if price.DeploymentID != deploymentID {
			return ErrNotFound
		}
		if price.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if price.CancelledAt != nil || !cancelledAt.Before(price.EffectiveFrom) {
			return domain.ErrPriceVersionUnavailable
		}
		if err := tx.Bucket(bucketDeploymentPricePins).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var pin domain.PricePinIntent
			if err := json.Unmarshal(raw, &pin); err != nil {
				return err
			}
			if pin.DeploymentID == deploymentID && pin.PriceVersionID == priceID {
				return domain.ErrPriceVersionUnavailable
			}
			return nil
		}); err != nil {
			return err
		}
		price.CancelledAt = &cancelledAt
		price.CancelledBy = strings.TrimSpace(actor)
		price.Revision++
		if err := price.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(price)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketDeploymentPriceVersions).Put([]byte(price.ID), encoded); err != nil {
			return err
		}
		if intent != nil {
			return putPricingAuditIntentTx(tx, *intent)
		}
		return nil
	})
	return price, err
}

func (s *Store) ListPendingPricingAuditIntents(ctx context.Context) ([]domain.PricingAuditIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var intents []domain.PricingAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketPricingAuditIntents).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var intent domain.PricingAuditIntent
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if err := intent.Validate(); err != nil {
				return err
			}
			if !intent.Delivered {
				intents = append(intents, intent)
			}
			return nil
		})
	})
	sort.Slice(intents, func(i, j int) bool {
		if intents[i].OccurredAt.Equal(intents[j].OccurredAt) {
			return intents[i].EventID < intents[j].EventID
		}
		return intents[i].OccurredAt.Before(intents[j].OccurredAt)
	})
	return intents, err
}

func (s *Store) MarkPricingAuditIntentDelivered(ctx context.Context, eventID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketPricingAuditIntents)
		raw := bucket.Get([]byte(eventID))
		if raw == nil {
			return ErrNotFound
		}
		var intent domain.PricingAuditIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if intent.Delivered {
			return nil
		}
		intent.Delivered = true
		encoded, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(eventID), encoded)
	})
}

func putPricingAuditIntentTx(tx *bbolt.Tx, intent domain.PricingAuditIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	bucket := tx.Bucket(bucketPricingAuditIntents)
	if raw := bucket.Get([]byte(intent.EventID)); raw != nil {
		var existing domain.PricingAuditIntent
		if err := json.Unmarshal(raw, &existing); err != nil {
			return err
		}
		if existing != intent {
			return errors.New("pricing audit event id conflicts with another intent")
		}
		return nil
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(intent.EventID), encoded)
}

func putDeploymentPriceVersionTx(tx *bbolt.Tx, price domain.DeploymentPriceVersion) error {
	if err := price.Validate(); err != nil {
		return err
	}
	prices := tx.Bucket(bucketDeploymentPriceVersions)
	if prices.Get([]byte(price.ID)) != nil {
		return ErrAlreadyExists
	}
	timelineRoot := tx.Bucket(bucketDeploymentPriceTimeline)
	timeline, err := timelineRoot.CreateBucketIfNotExists([]byte(price.DeploymentID))
	if err != nil {
		return err
	}
	key := deploymentPriceTimelineKey(price.EffectiveFrom)
	if timeline.Get(key) != nil {
		return domain.ErrPriceTimelineConflict
	}
	encoded, err := json.Marshal(price)
	if err != nil {
		return err
	}
	if err := prices.Put([]byte(price.ID), encoded); err != nil {
		return err
	}
	return timeline.Put(key, []byte(price.ID))
}

func deploymentPriceTimelineKey(effectiveFrom time.Time) []byte {
	var key [12]byte
	seconds := effectiveFrom.UTC().Unix()
	binary.BigEndian.PutUint64(key[:8], uint64(seconds)^(uint64(1)<<63))
	binary.BigEndian.PutUint32(key[8:], uint32(effectiveFrom.UTC().Nanosecond()))
	return key[:]
}

func validSHA256Label(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
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
		} else {
			raw := tx.Bucket(bucketProviders).Get([]byte(route.ProviderID))
			if raw == nil {
				return fmt.Errorf("provider %q: %w", route.ProviderID, ErrNotFound)
			}
			var instance domain.ProviderInstance
			if err := json.Unmarshal(raw, &instance); err != nil {
				return err
			}
			normalizeProviderBindings(&instance)
			if len(instance.Bindings) != 1 {
				return errors.New("deployment is required for a provider with multiple profile bindings")
			}
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

// ListProviderResources returns every persisted resource owner mapping. Backup
// uses this while holding the data-directory lock so it can include exactly
// the local objects referenced by the metadata snapshot and reject a missing
// object instead of publishing an incomplete archive.
func (s *Store) ListProviderResources(ctx context.Context) ([]domain.ProviderResource, error) {
	var resources []domain.ProviderResource
	err := s.listJSON(ctx, bucketProviderResources, func(raw []byte) error {
		var resource domain.ProviderResource
		if err := json.Unmarshal(raw, &resource); err != nil {
			return err
		}
		resources = append(resources, resource)
		return nil
	})
	return resources, err
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
