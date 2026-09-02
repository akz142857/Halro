package bolt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/masterkey"
	bbolt "go.etcd.io/bbolt"
)

const schemaVersion uint64 = 34

// legacyCapabilityEvidence is the evidence tier this project used before
// capability evidence was durable metadata. The domain no longer accepts it, so
// no code path can produce it for new data. It survives here for one reason: the
// migrations below wrote it, and migration 21 has to recognise those bytes on
// disk in order to refuse the directory rather than misread it.
const legacyCapabilityEvidence domain.CapabilityEvidence = "legacy"

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
	bucketAdminAuditIntents          = []byte("admin_audit_intents")
	bucketPricingIdempotency         = []byte("pricing_idempotency")
	bucketDeploymentPriceProposals   = []byte("deployment_price_proposals")
	bucketPricingProposalIdempotency = []byte("pricing_proposal_idempotency")
	bucketCostAdjustmentIntents      = []byte("cost_adjustment_intents")
	bucketAuditAnchors               = []byte("audit_anchors")
	bucketModelCapabilityDetections  = []byte("model_capability_detections")
	bucketCapabilityDetectionIdem    = []byte("model_capability_detection_idempotency")
	bucketCapabilityDetectionIndex   = []byte("model_capability_detection_fingerprint_index")
	bucketUsageDailyRollup           = []byte("usage_daily_rollup")
	keySchemaVersion                 = []byte("schema_version")
	keyVaultCheck                    = []byte("vault_key_check")
	keyUsageCheckpoint               = []byte("usage_checkpoint")
	keyUsageRollupState              = []byte("usage_rollup_state")
	keyTokenGuardCheckpoint          = []byte("token_guard_checkpoint")
	keyAuditCheckpoint               = []byte("audit_checkpoint")
	keyAuditHMACEnvelope             = []byte("audit_hmac_envelope")
	keyLedgerHMACEnvelope            = []byte("ledger_hmac_envelope")
	keyLedgerChainCheckpoint         = []byte("ledger_chain_checkpoint")
	keyVaultKeyring                  = []byte("vault_keyring")
	keyKeySlotDescriptor             = []byte("key_slot_descriptor")
	keyKeySlotAuditIntent            = []byte("key_slot_audit_intent")
	keyMasterKeyRotationAuditIntent  = []byte("master_key_rotation_audit_intent")
	keyRuntimeSettings               = []byte("runtime_settings")
	keyInstanceUISettings            = []byte("instance_ui_settings")
	keyInstanceAccountingSettings    = []byte("instance_accounting_settings")
	keyInstanceID                    = []byte("instance_id")
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
	// The "phase2" in this migration's name and in its two step names is a
	// recorded fact about deployments that already ran, not a description of
	// anything. The identifiers that once shared the name are now
	// InferenceResources; these three strings must not follow, or an upgraded
	// instance stops matching its own migration history.
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
					value.CapabilityEvidence = domain.NormalizeCapabilityEvidence(value.Capabilities, value.CapabilityEvidence, legacyCapabilityEvidence)
					return json.Marshal(value)
				}
				var value domain.Deployment
				if err := json.Unmarshal(raw, &value); err != nil {
					return nil, err
				}
				value.CapabilityEvidence = domain.NormalizeCapabilityEvidence(value.Capabilities, value.CapabilityEvidence, legacyCapabilityEvidence)
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
	// The record itself is written on first open from the configured zone
	// (SeedInstanceAccountingSettings), not here: config.yaml is the seed, and
	// this layer has no access to it. The step marks the version at which the
	// accounting timezone stopped being read from configuration on every start.
	{version: 16, name: "instance_accounting_settings", up: func(_ *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_instance_accounting_settings"); err != nil {
			return err
		}
		return migrationStep(step, "after_instance_accounting_settings")
	}},
	// The accounting-timezone work (schema v16) made every new Ledger event
	// carry period identity, which unconditionally promotes it to frame epoch
	// 3 (frameVersionPeriod) — but no migration ever moved this gate off epoch
	// 2, so an old binary that only understood epoch 2 was never refused for
	// opening a WAL that already contained epoch-3 frames it couldn't parse.
	// This migration jumps straight to epoch 4 (frame HMAC + hash chain,
	// ADR 0016) and retroactively covers the epoch-3 gap in the same step
	// rather than leaving two generations of the same oversight stacked.
	{version: 17, name: "ledger_frame_integrity", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_ledger_frame_integrity"); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		if err := meta.Put(keyMinimumLedgerReaderVersion, []byte("v4")); err != nil {
			return err
		}
		if err := meta.Put(keyLedgerFeatureEpoch, []byte{4}); err != nil {
			return err
		}
		// keyLedgerChainCheckpoint is brand new at this migration — unlike
		// audit's checkpoint, which has existed since before any migration in
		// this chain, every instance (fresh or upgrading) passes through here
		// exactly once, so this is the one place that can seed it universally
		// rather than only covering the brand-new-instance init path.
		zeroCheckpoint, err := json.Marshal(LedgerChainCheckpoint{})
		if err != nil {
			return err
		}
		if err := meta.Put(keyLedgerChainCheckpoint, zeroCheckpoint); err != nil {
			return err
		}
		return migrationStep(step, "after_ledger_frame_integrity")
	}},
	{version: 18, name: "audit_anchors", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_create_audit_anchors"); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketAuditAnchors); err != nil {
			return err
		}
		return migrationStep(step, "after_create_audit_anchors")
	}},
	// domain.AdminUser.Role arrived with the two-level Admin roles and is
	// required by Validate, but every account created before it exists on disk
	// with an empty role. Nothing backfilled them, which left an upgraded
	// instance in a state its own validation rejects: the account cannot save
	// its own preferences, and requireAdministratorRole reads the empty string
	// as "not an administrator" and refuses every administrator-gated write.
	//
	// Administrator is the faithful backfill, not a lenient one. Before roles
	// existed there was exactly one kind of admin account and it could do
	// everything; recording anything else here would take capability away from
	// an operator who never gave it up.
	{version: 19, name: "admin_role_backfill", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_admin_role_backfill"); err != nil {
			return err
		}
		if err := rewriteBucket(tx.Bucket(bucketAdminUsers), func(raw []byte) ([]byte, error) {
			var value domain.AdminUser
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, err
			}
			// Only the empty role is backfilled — the exact shape a record
			// written before the field existed has. Any other unrecognised
			// value is left alone so it keeps failing validation loudly:
			// normalising an unexpected role would turn "this record is not
			// what we think it is" into a silent grant of the highest
			// privilege, which is the wrong direction for a value that no
			// supported write path can produce.
			if value.Role != "" {
				return raw, nil
			}
			value.Role = domain.AdminRoleAdministrator
			return json.Marshal(value)
		}); err != nil {
			return err
		}
		return migrationStep(step, "after_admin_role_backfill")
	}},
	{version: 20, name: "deployment_capability_snapshot", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_capability_snapshot_check"); err != nil {
			return err
		}
		// Deployments now carry the model capability snapshot the request path
		// reads. There is no backfill: the honest value for an existing
		// deployment would be "the provider ceiling", which is exactly the guess
		// the snapshot exists to replace, and writing it would make a guess
		// indistinguishable from an established fact for the rest of time.
		//
		// Pre-1.0.0 there is no deployment in the wild to preserve, so a data
		// directory holding deployments is refused and rebuilt rather than
		// silently reinterpreted. An empty one upgrades in place.
		deployments := tx.Bucket(bucketDeployments)
		if deployments != nil && deployments.Stats().KeyN > 0 {
			return fmt.Errorf(
				"this build stores a model capability snapshot on every deployment and cannot infer one for the %d existing deployment(s); "+
					"re-initialise the data directory (make reset CONFIRM=RESET) and recreate them, or keep running the previous build",
				deployments.Stats().KeyN,
			)
		}
		return migrationStep(step, "after_capability_snapshot_check")
	}},
	{version: 21, name: "refuse_legacy_capability_evidence", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_legacy_evidence_check"); err != nil {
			return err
		}
		// The `legacy` evidence tier is gone. It meant "this bit came from a
		// record written before capability evidence was durable metadata", and
		// keeping it beside declared and verified meant three tiers where the
		// design has two, with the third carrying no evidence at all.
		//
		// There is no rewrite. Promoting it to `declared` would assert that
		// somebody declared these capabilities, which nobody did; demoting it to
		// `unsupported` would turn capabilities off under a running deployment.
		// Both invent an answer. Pre-1.0.0 there is nothing in the wild to
		// preserve, so the directory is refused and rebuilt instead — the same
		// choice migration 20 made, for the same reason.
		affected := 0
		for _, bucketName := range [][]byte{bucketProviders, bucketDeployments} {
			bucket := tx.Bucket(bucketName)
			if bucket == nil {
				continue
			}
			if err := bucket.ForEach(func(_, raw []byte) error {
				var record struct {
					CapabilityEvidence map[string]string `json:"capability_evidence"`
					Bindings           []struct {
						CapabilityEvidence map[string]string `json:"capability_evidence"`
					} `json:"bindings,omitempty"`
				}
				if err := json.Unmarshal(raw, &record); err != nil {
					// Unreadable here means unreadable by the running build too.
					// Counting it as affected keeps this fail-closed rather than
					// letting a record slip through because it would not parse.
					affected++
					return nil
				}
				sets := []map[string]string{record.CapabilityEvidence}
				for _, binding := range record.Bindings {
					sets = append(sets, binding.CapabilityEvidence)
				}
				for _, set := range sets {
					for _, value := range set {
						if value == string(legacyCapabilityEvidence) {
							affected++
							return nil
						}
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if affected > 0 {
			return fmt.Errorf(
				"this build removed the %q capability evidence tier and will not guess what %d existing provider/deployment record(s) meant by it; "+
					"re-initialise the data directory (make reset CONFIRM=RESET) and recreate them, or keep running the previous build",
				legacyCapabilityEvidence, affected,
			)
		}
		return migrationStep(step, "after_legacy_evidence_check")
	}},
	{version: 22, name: "refuse_deployment_less_routes", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_deployment_less_route_check"); err != nil {
			return err
		}
		// A route now names a deployment and nothing else. The old shape could
		// reach a provider directly, which meant it bypassed the deployment's
		// versioned price, health probe, capability snapshot and concurrency
		// limit — four governed things, silently absent.
		//
		// ForEach, not Stats(): migration 3 used to synthesise deployments in
		// this same transaction, and Stats() does not observe uncommitted writes
		// made earlier in it. That is exactly how an invalid deployment slipped
		// past migration 20's guard. ForEach does see them.
		routes := tx.Bucket(bucketRoutes)
		if routes == nil {
			return migrationStep(step, "after_deployment_less_route_check")
		}
		affected := 0
		if err := routes.ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var route struct {
				DeploymentID string `json:"deployment_id"`
			}
			if err := json.Unmarshal(raw, &route); err != nil {
				// Unreadable here is unreadable by the running build too.
				affected++
				return nil
			}
			if route.DeploymentID == "" {
				affected++
			}
			return nil
		}); err != nil {
			return err
		}
		if affected > 0 {
			return fmt.Errorf(
				"this build requires every route to name a deployment and found %d route(s) without one; "+
					"a route that reaches a provider directly has no versioned price, health probe, capability snapshot or concurrency limit behind it, "+
					"and none of those can be inferred; re-initialise the data directory (make reset CONFIRM=RESET) and recreate the topology, "+
					"or keep running the previous build",
				affected,
			)
		}
		return migrationStep(step, "after_deployment_less_route_check")
	}},
	{version: 23, name: "deployment_snapshot_evidence_and_disabled", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_snapshot_evidence_backfill"); err != nil {
			return err
		}
		// Two §5.2 fields arrive on existing records: the snapshot's evidence and
		// the set of capabilities the operator switched off.
		//
		// This one backfills where migration 20 and 21 refused, and the
		// difference is whether the value can be reconstructed. `legacy` evidence
		// could not: promoting it to `declared` would assert a declaration nobody
		// made. A capability snapshot could not: the honest value was the
		// provider ceiling, which is the guess the snapshot replaces. These two
		// can — both are pure functions of fields already in the record, computed
		// by the same domain helpers the write path calls, so a record brought
		// forward here is byte-identical to the same record re-saved. A test
		// asserts exactly that rather than trusting this paragraph.
		//
		// Fields are patched into the decoded object rather than through the
		// Deployment struct, so anything this migration does not know about
		// survives it intact.
		deployments := tx.Bucket(bucketDeployments)
		if deployments == nil {
			return migrationStep(step, "after_snapshot_evidence_backfill")
		}
		type snapshotShape struct {
			Source       string                      `json:"source"`
			Capabilities domain.ProviderCapabilities `json:"capabilities"`
		}
		cursor := deployments.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			if raw == nil {
				continue
			}
			var record map[string]json.RawMessage
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("deployment %q is unreadable and cannot be brought forward: %w", key, err)
			}
			encodedSnapshot, ok := record["model_capability_snapshot"]
			if !ok {
				return fmt.Errorf("deployment %q has no capability snapshot to derive evidence from", key)
			}
			var snapshot snapshotShape
			if err := json.Unmarshal(encodedSnapshot, &snapshot); err != nil {
				return fmt.Errorf("deployment %q has an unreadable capability snapshot: %w", key, err)
			}
			var capabilities domain.ProviderCapabilities
			if encoded, ok := record["capabilities"]; ok {
				if err := json.Unmarshal(encoded, &capabilities); err != nil {
					return fmt.Errorf("deployment %q has unreadable capabilities: %w", key, err)
				}
			}
			model := domain.ModelCapabilitySnapshot{Source: snapshot.Source, Capabilities: snapshot.Capabilities}
			patched, err := patchJSONField(encodedSnapshot, "evidence", domain.SnapshotEvidence(model))
			if err != nil {
				return fmt.Errorf("deployment %q snapshot evidence: %w", key, err)
			}
			record["model_capability_snapshot"] = patched
			disabled := domain.OperatorDisabledCapabilities(model, capabilities, nil)
			if len(disabled) == 0 {
				delete(record, "operator_disabled")
			} else {
				encoded, err := json.Marshal(disabled)
				if err != nil {
					return fmt.Errorf("deployment %q disabled capabilities: %w", key, err)
				}
				record["operator_disabled"] = encoded
			}
			updated, err := json.Marshal(record)
			if err != nil {
				return fmt.Errorf("deployment %q could not be re-encoded: %w", key, err)
			}
			if err := deployments.Put(key, updated); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_snapshot_evidence_backfill")
	}},
	{version: 24, name: "model_capability_detections", up: func(tx *bbolt.Tx, step func(string) error) error {
		for _, name := range [][]byte{bucketModelCapabilityDetections, bucketCapabilityDetectionIdem, bucketCapabilityDetectionIndex} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_create_model_capability_detection_buckets")
	}},
	// Detection records gained a candidate set, a selection fingerprint and a
	// resolved-only target fingerprint when identification stopped asking the
	// operator which interface a model speaks. A record written before that
	// cannot be reshaped honestly: its selection fingerprint would have to be
	// recomputed here from the app's own hashing, and the candidate set it was
	// never evaluated against would be invented. Detections are a rebuildable
	// cache — they carry a freshness TTL and a retention window, deployments
	// keep their own capability snapshot, and re-running one costs the same
	// probes it cost the first time. So drop them and let them be re-detected
	// rather than carry a fabricated shape forward.
	{version: 25, name: "reset_capability_detections_for_interface_identification", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_reset_model_capability_detections"); err != nil {
			return err
		}
		for _, name := range [][]byte{bucketModelCapabilityDetections, bucketCapabilityDetectionIdem, bucketCapabilityDetectionIndex} {
			if tx.Bucket(name) != nil {
				if err := tx.DeleteBucket(name); err != nil {
					return err
				}
			}
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_reset_model_capability_detections")
	}},
	// Candidates now record what their interface can verify at all, which is
	// what separates "the model refused everything" from "nothing here could be
	// asked". A record written before that carries no such list, and inventing
	// one would mean re-deriving a detection plan from a provider whose bindings
	// may since have changed. Same reasoning as 25: rebuild the cache.
	{version: 26, name: "reset_capability_detections_for_verifiable_scope", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_reset_detections_for_verifiable_scope"); err != nil {
			return err
		}
		for _, name := range [][]byte{bucketModelCapabilityDetections, bucketCapabilityDetectionIdem, bucketCapabilityDetectionIndex} {
			if tx.Bucket(name) != nil {
				if err := tx.DeleteBucket(name); err != nil {
					return err
				}
			}
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_reset_detections_for_verifiable_scope")
	}},
	// Admin mutations gained a durable audit intent so the store commit is the
	// only commit point: the audit record is written with the mutation and
	// delivered afterwards. Nothing existing has to be reshaped — an instance
	// upgrading here simply has no pending intents, which is the same state a
	// fully drained instance is in.
	{version: 27, name: "admin_audit_intents", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_create_admin_audit_intents"); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketAdminAuditIntents); err != nil {
			return err
		}
		return migrationStep(step, "after_create_admin_audit_intents")
	}},
	// provider_executed_tools joins the capability dictionary. Evidence sets are
	// validated against that dictionary as a whole — every name must be present —
	// so a record written before this migration stops loading the moment the name
	// exists. The value is reconstructible rather than invented: the capability
	// did not exist, so nothing could have declared it, and `unsupported` is the
	// only reading of a record that predates it.
	//
	// Fields are patched into the decoded object rather than re-marshalled through
	// the domain structs, so nothing this migration does not know about is lost.
	{version: 28, name: "provider_executed_tools_capability", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_provider_executed_tools_capability"); err != nil {
			return err
		}
		if err := rewriteBucketIfPresent(tx, bucketProviders, func(record map[string]json.RawMessage) error {
			if err := backfillEvidenceMember(record, "capability_evidence"); err != nil {
				return err
			}
			return patchArrayMember(record, "bindings", func(binding map[string]json.RawMessage) error {
				return backfillEvidenceMember(binding, "capability_evidence")
			})
		}); err != nil {
			return err
		}
		if err := rewriteBucketIfPresent(tx, bucketDeployments, func(record map[string]json.RawMessage) error {
			if err := backfillEvidenceMember(record, "capability_evidence"); err != nil {
				return err
			}
			encoded, ok := record["model_capability_snapshot"]
			if !ok {
				return nil
			}
			var snapshot map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &snapshot); err != nil {
				return err
			}
			if err := backfillEvidenceMember(snapshot, "evidence"); err != nil {
				return err
			}
			updated, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			record["model_capability_snapshot"] = updated
			return nil
		}); err != nil {
			return err
		}
		return migrationStep(step, "after_provider_executed_tools_capability")
	}},
	{version: 29, name: "project_allowed_models", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_project_allowed_models"); err != nil {
			return err
		}
		// The field always held public model aliases, never route IDs; the name
		// said otherwise, and the wire contract inherited the lie. Renamed in
		// place — pre-1.0.0 keeps no compatibility alias — so stored records
		// move their value to the key every reader now uses.
		if err := rewriteBucketIfPresent(tx, bucketProjects, func(record map[string]json.RawMessage) error {
			encoded, ok := record["allowed_routes"]
			if !ok {
				return nil
			}
			delete(record, "allowed_routes")
			if _, taken := record["allowed_models"]; !taken {
				record["allowed_models"] = encoded
			}
			return nil
		}); err != nil {
			return err
		}
		return migrationStep(step, "after_project_allowed_models")
	}},
	// A price version gained a cache-read rate. Records written before it have
	// no such term, and decoding a missing term as zero would retroactively make
	// every cached prompt token free — so the value is reconstructed instead:
	// until this migration a cached token was billed at the ordinary input rate,
	// and copying that rate across is the only reading that leaves existing
	// prices charging exactly what they charged yesterday.
	//
	// Proposals carry a digest over their own billing terms, so a backfilled
	// proposal is re-digested; leaving the old digest would make every stored
	// proposal fail validation on the next read.
	{version: 30, name: "deployment_price_cached_input_rate", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_deployment_price_cached_input_rate"); err != nil {
			return err
		}
		if err := rewriteBucketIfPresent(tx, bucketDeploymentPriceVersions, backfillCachedInputRate); err != nil {
			return err
		}
		if err := rewriteBucketIfPresent(tx, bucketDeploymentPriceProposals, func(record map[string]json.RawMessage) error {
			if err := backfillCachedInputRate(record); err != nil {
				return err
			}
			return redigestPriceProposal(record)
		}); err != nil {
			return err
		}
		return migrationStep(step, "after_deployment_price_cached_input_rate")
	}},
	// fetched_image splits vision into what a target can read and what it will go
	// and get. Every record that predates it records it as unsupported and nobody
	// is credited with it, which is the same shape migration 28 used for
	// provider_executed_tools.
	//
	// It is deliberately not backfilled from vision, even though a connection that
	// declared vision on OpenAI did in fact accept a fetched image before this
	// split. Backfilling would have to know each record's profile ceiling to avoid
	// crediting a Bedrock connection with something it cannot do, and a migration
	// that reaches across buckets to decide what a record may claim is a migration
	// that can get it wrong silently. Off for everyone is wrong in the direction
	// that refuses rather than the one that forwards a request doomed upstream,
	// and it costs one deliberate tick where the capability is really wanted.
	{version: 31, name: "fetched_image_capability", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_fetched_image_capability"); err != nil {
			return err
		}
		backfill := func(record map[string]json.RawMessage) error {
			if err := backfillCapabilityEvidence(record, "capability_evidence", fetchedImageEvidenceMembers); err != nil {
				return err
			}
			return patchArrayMember(record, "bindings", func(binding map[string]json.RawMessage) error {
				return backfillCapabilityEvidence(binding, "capability_evidence", fetchedImageEvidenceMembers)
			})
		}
		if err := rewriteBucketIfPresent(tx, bucketProviders, backfill); err != nil {
			return err
		}
		if err := rewriteBucketIfPresent(tx, bucketDeployments, func(record map[string]json.RawMessage) error {
			if err := backfillCapabilityEvidence(record, "capability_evidence", fetchedImageEvidenceMembers); err != nil {
				return err
			}
			encoded, ok := record["model_capability_snapshot"]
			if !ok {
				return nil
			}
			var snapshot map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &snapshot); err != nil {
				return err
			}
			if err := backfillCapabilityEvidence(snapshot, "evidence", fetchedImageEvidenceMembers); err != nil {
				return err
			}
			updated, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			record["model_capability_snapshot"] = updated
			return nil
		}); err != nil {
			return err
		}
		return migrationStep(step, "after_fetched_image_capability")
	}},
	// json_mode becomes json_object and structured_outputs. It described two
	// claims no provider serves as one — a schema-less mode that only promises
	// parseable JSON, and a schema the upstream enforces — so a record carrying
	// the old switch cannot say which of them its target actually has.
	//
	// Both halves are recorded as unsupported for everyone, the same shape
	// migrations 28 and 31 used, and for the same reason 31 gave: the honest
	// reconstruction would have to read each record's profile and decide on its
	// behalf what it may claim, and a migration that decides that silently is a
	// migration that can be silently wrong. Off refuses a request; on forwards
	// one that the upstream will reject after the budget is already reserved.
	// The cost is one deliberate tick per deployment that really wants it, and
	// the console now offers the two halves separately to tick.
	//
	// Capabilities and evidence move together because Validate holds them to a
	// biconditional — an enabled capability may not be unsupported and a
	// disabled one may not be anything else — so patching either alone would
	// leave every record refusing to load. operator_disabled is swept for the
	// same reason: it names capabilities, and an unknown name there is a
	// validation failure rather than a stale entry nobody reads.
	//
	// Stored detections are dropped rather than patched. Their results are keyed
	// by capability name and their fingerprints carry the detector contract
	// version, which moved to v4 with this split: a v3 record's json_mode result
	// answers a question no longer asked, because that probe sent response_format
	// json_object and established nothing about a schema.
	{version: 32, name: "structured_output_capability_split", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_structured_output_capability_split"); err != nil {
			return err
		}
		splitRecord := func(record map[string]json.RawMessage) error {
			if err := splitJSONModeCapabilities(record, "capabilities"); err != nil {
				return err
			}
			return splitJSONModeEvidence(record, "capability_evidence")
		}
		if err := rewriteBucketIfPresent(tx, bucketProviders, func(record map[string]json.RawMessage) error {
			if err := splitRecord(record); err != nil {
				return err
			}
			return patchArrayMember(record, "bindings", splitRecord)
		}); err != nil {
			return err
		}
		if err := rewriteBucketIfPresent(tx, bucketDeployments, func(record map[string]json.RawMessage) error {
			if err := splitRecord(record); err != nil {
				return err
			}
			if err := dropDisabledCapability(record, "operator_disabled", "json_mode"); err != nil {
				return err
			}
			encoded, ok := record["model_capability_snapshot"]
			if !ok {
				return nil
			}
			var snapshot map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &snapshot); err != nil {
				return err
			}
			if err := splitJSONModeCapabilities(snapshot, "capabilities"); err != nil {
				return err
			}
			if err := splitJSONModeEvidence(snapshot, "evidence"); err != nil {
				return err
			}
			updated, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			record["model_capability_snapshot"] = updated
			return nil
		}); err != nil {
			return err
		}
		for _, name := range [][]byte{bucketModelCapabilityDetections, bucketCapabilityDetectionIdem, bucketCapabilityDetectionIndex} {
			if tx.Bucket(name) != nil {
				if err := tx.DeleteBucket(name); err != nil {
					return err
				}
			}
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return migrationStep(step, "after_structured_output_capability_split")
	}},
	{version: 33, name: "usage_daily_rollup", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_create_usage_daily_rollup"); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketUsageDailyRollup); err != nil {
			return err
		}
		return migrationStep(step, "after_create_usage_daily_rollup")
	}},
	// Sealing gave the Ledger more than one file, so the trusted chain head has
	// to name which one it sits in: after a roll the sequence is unchanged, the
	// hash is unchanged, and the offset legitimately restarts at zero. Without
	// a generation the startup check reads that as a chain that moved and
	// refuses to start. Every checkpoint written before sealing existed
	// describes generation 1, which is what this stamps.
	{version: 34, name: "ledger_chain_checkpoint_generation", up: func(tx *bbolt.Tx, step func(string) error) error {
		if err := migrationStep(step, "before_ledger_chain_checkpoint_generation"); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		raw := meta.Get(keyLedgerChainCheckpoint)
		if raw != nil {
			var checkpoint LedgerChainCheckpoint
			if err := json.Unmarshal(raw, &checkpoint); err != nil {
				return fmt.Errorf("decode ledger chain checkpoint: %w", err)
			}
			if checkpoint.Sequence > 0 && checkpoint.Generation == 0 {
				checkpoint.Generation = 1
				encoded, err := json.Marshal(checkpoint)
				if err != nil {
					return err
				}
				if err := meta.Put(keyLedgerChainCheckpoint, encoded); err != nil {
					return err
				}
			}
		}
		return migrationStep(step, "after_ledger_chain_checkpoint_generation")
	}},
}

// splitJSONModeCapabilities replaces a stored capability set's json_mode member
// with the two that succeed it, both off. The old member is deleted rather than
// left beside them: an unknown key would survive every read that decodes into
// the struct and reappear in nothing, which is a record that disagrees with
// itself on disk.
func splitJSONModeCapabilities(object map[string]json.RawMessage, field string) error {
	encoded, ok := object[field]
	if !ok || len(encoded) == 0 {
		return nil
	}
	var capabilities map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &capabilities); err != nil {
		return err
	}
	if capabilities == nil {
		return nil
	}
	delete(capabilities, "json_mode")
	for _, name := range jsonModeSuccessors {
		capabilities[name] = json.RawMessage("false")
	}
	updated, err := json.Marshal(capabilities)
	if err != nil {
		return err
	}
	object[field] = updated
	return nil
}

// splitJSONModeEvidence is the evidence half of the same move. Both successors
// are unsupported because both capabilities are off, which is the only pairing
// CapabilityEvidenceSet.Validate accepts.
func splitJSONModeEvidence(object map[string]json.RawMessage, field string) error {
	encoded, ok := object[field]
	if !ok || len(encoded) == 0 {
		return nil
	}
	var evidence map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &evidence); err != nil {
		return err
	}
	if evidence == nil {
		return nil
	}
	delete(evidence, "json_mode")
	for _, name := range jsonModeSuccessors {
		evidence[name] = json.RawMessage(`"` + string(domain.EvidenceUnsupported) + `"`)
	}
	updated, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	object[field] = updated
	return nil
}

// jsonModeSuccessors names what json_mode became.
var jsonModeSuccessors = []string{"json_object", "structured_outputs"}

// dropDisabledCapability removes one name from a stored list of capabilities an
// operator switched off. A name the dictionary no longer carries fails
// validation on the next read, and the capability it referred to is off for
// everybody after this migration anyway, so there is nothing to carry forward.
func dropDisabledCapability(record map[string]json.RawMessage, field, name string) error {
	encoded, ok := record[field]
	if !ok || len(encoded) == 0 {
		return nil
	}
	var names []string
	if err := json.Unmarshal(encoded, &names); err != nil {
		return err
	}
	kept := make([]string, 0, len(names))
	for _, value := range names {
		if value != name {
			kept = append(kept, value)
		}
	}
	if len(kept) == len(names) {
		return nil
	}
	if len(kept) == 0 {
		delete(record, field)
		return nil
	}
	updated, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	record[field] = updated
	return nil
}

// backfillCachedInputRate copies a record's input rate onto the cache-read rate
// it predates. A record that already carries the term is left alone.
func backfillCachedInputRate(record map[string]json.RawMessage) error {
	if _, present := record["cached_input_micros_per_million"]; present {
		return nil
	}
	input, ok := record["input_micros_per_million"]
	if !ok {
		return errors.New("price record has no input rate to reconstruct a cache-read rate from")
	}
	record["cached_input_micros_per_million"] = input
	return nil
}

// redigestPriceProposal recomputes a proposal's evidence digest after its
// billing terms were brought forward. The digest is computed from the decoded
// proposal, exactly as validation computes it.
func redigestPriceProposal(record map[string]json.RawMessage) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	var proposal domain.DeploymentPriceProposal
	if err := json.Unmarshal(encoded, &proposal); err != nil {
		return err
	}
	digest, err := proposal.ComputeDigest()
	if err != nil {
		return err
	}
	patched, err := json.Marshal(digest)
	if err != nil {
		return err
	}
	record["digest"] = patched
	return nil
}

// newCapabilityEvidenceMembers names the capabilities added to the dictionary by
// migration 28. Each is recorded as unsupported on records that predate it.
var newCapabilityEvidenceMembers = []string{"provider_executed_tools"}

// fetchedImageEvidenceMembers is migration 31's equivalent. Kept apart from the
// list above because a record written between 28 and 31 already has the first
// member and needs only the second.
var fetchedImageEvidenceMembers = []string{"fetched_image"}

func backfillEvidenceMember(object map[string]json.RawMessage, field string) error {
	return backfillCapabilityEvidence(object, field, newCapabilityEvidenceMembers)
}

func backfillCapabilityEvidence(object map[string]json.RawMessage, field string, members []string) error {
	encoded, ok := object[field]
	if !ok || len(encoded) == 0 {
		return nil
	}
	var evidence map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &evidence); err != nil {
		return err
	}
	if evidence == nil {
		return nil
	}
	changed := false
	for _, name := range members {
		if _, present := evidence[name]; present {
			continue
		}
		evidence[name] = json.RawMessage(`"` + string(domain.EvidenceUnsupported) + `"`)
		changed = true
	}
	if !changed {
		return nil
	}
	updated, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	object[field] = updated
	return nil
}

func patchArrayMember(object map[string]json.RawMessage, field string, patch func(map[string]json.RawMessage) error) error {
	encoded, ok := object[field]
	if !ok || len(encoded) == 0 {
		return nil
	}
	var elements []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &elements); err != nil {
		return err
	}
	for _, element := range elements {
		if err := patch(element); err != nil {
			return err
		}
	}
	updated, err := json.Marshal(elements)
	if err != nil {
		return err
	}
	object[field] = updated
	return nil
}

func rewriteBucketIfPresent(tx *bbolt.Tx, name []byte, patch func(map[string]json.RawMessage) error) error {
	bucket := tx.Bucket(name)
	if bucket == nil {
		return nil
	}
	return rewriteBucket(bucket, func(raw []byte) ([]byte, error) {
		var record map[string]json.RawMessage
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, fmt.Errorf("record in %s is unreadable and cannot be brought forward: %w", name, err)
		}
		if err := patch(record); err != nil {
			return nil, err
		}
		return json.Marshal(record)
	})
}

// patchJSONField sets one field on an encoded object without disturbing the
// rest of it, so a migration cannot drop what it does not know about.
func patchJSONField(encoded json.RawMessage, name string, value any) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	field, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	object[name] = field
	return json.Marshal(object)
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

type AuditCheckpoint struct {
	Records  uint64   `json:"records"`
	Bytes    int64    `json:"bytes"`
	LastHash [32]byte `json:"last_hash"`
}

// LedgerChainCheckpoint is the trusted high-water mark for the Ledger's v4
// hash chain, checked at startup the same way AuditCheckpoint is: a chain
// that has gone backwards or diverged at the same sequence means the WAL was
// truncated or rewritten since this checkpoint was recorded.
type LedgerChainCheckpoint struct {
	// Generation names the WAL file the head sits in. A roll advances it and
	// resets Offset, which is the one way the offset may move without the
	// sequence moving.
	Generation uint64   `json:"generation"`
	Sequence   uint64   `json:"sequence"`
	Offset     int64    `json:"offset"`
	Hash       [32]byte `json:"hash"`
}

// MetadataWriteStats describes the metadata store's durable write path.
// BatchCalls/BatchTransactions is the mean coalescing factor — the bbolt
// counterpart of the Ledger's records-per-batch, and the only way to tell from
// outside whether batching the price pin writes (ADR 0012, "Amendment
// 2026-08-07") is actually doing anything on a given host.
type MetadataWriteStats struct {
	BatchCalls        uint64
	BatchTransactions uint64
	PageWrites        int64
	PageWriteDuration time.Duration
	FreePages         int
	PendingPages      int
}

func (s *Store) MetadataWriteStats() MetadataWriteStats {
	stats := MetadataWriteStats{
		BatchCalls:        s.batchCalls.Load(),
		BatchTransactions: s.batchTransactions.Load(),
	}
	if s.db != nil {
		dbStats := s.db.Stats()
		stats.PageWrites = dbStats.TxStats.GetWrite()
		stats.PageWriteDuration = dbStats.TxStats.GetWriteTime()
		stats.FreePages, stats.PendingPages = dbStats.FreePageN, dbStats.PendingPageN
	}
	return stats
}

// batch is db.Batch plus the bookkeeping that makes coalescing observable. The
// batched function may run more than once and several callers share one
// transaction, so transactions are counted by watching tx.ID() change rather
// than by counting calls: within one transaction bbolt runs the queued functions
// sequentially on one goroutine, and separate write transactions are serialized,
// so the swap below counts each transaction exactly once.
func (s *Store) batch(fn func(*bbolt.Tx) error) error {
	s.batchCalls.Add(1)
	return s.db.Batch(func(tx *bbolt.Tx) error {
		if id := uint64(tx.ID()); s.lastBatchTxID.Swap(id) != id {
			s.batchTransactions.Add(1)
		}
		return fn(tx)
	})
}

type Store struct {
	db *bbolt.DB

	batchCalls        atomic.Uint64
	batchTransactions atomic.Uint64
	lastBatchTxID     atomic.Uint64
	// pricingStates holds deploymentID -> *deploymentPricingState. Entries are
	// created on first use and never removed: one small struct per deployment
	// that has been priced, which is bounded by the deployment count.
	pricingStates sync.Map
}

// deploymentPricingState is the per-deployment concurrency state for pricing.
// Both members are deliberately per-deployment rather than process-wide: a
// global lock here serializes every deployment's price selection behind one
// mutex, which was measured as the Gateway's throughput ceiling (ADR 0012,
// "Amendment 2026-08-07").
type deploymentPricingState struct {
	// gate serializes price selection against timeline mutation. Selection
	// takes it shared, Admin mutation exclusively; see ADR 0012.
	gate sync.RWMutex
	// clockMu guards clock, and is held only for the read and the merge — never
	// across a bbolt transaction, or concurrent selections could not coalesce.
	clockMu  sync.Mutex
	clock    pricingClockObservation
	hasClock bool
	// timelineMu guards the audited timeline cache below. The timeline is
	// immutable between Admin mutations, and auditing it means decoding every
	// version ever created, so the Gateway keeps the audited copy rather than
	// rebuilding it on each request. Every write path that changes a version
	// record drops it; a single process owns the data directory, so no other
	// writer can leave it stale.
	timelineMu     sync.Mutex
	timeline       []domain.DeploymentPriceVersion
	timelineLoaded bool
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

// Batch tunables for the metadata store, measured rather than assumed with
// BenchmarkMetadataBatchDelay — see ADR 0012, "Amendment 2026-08-07". bbolt's
// 10ms default is the worst value on the sweep: it is on the order of a full
// F_FULLFSYNC here, so a lone writer waits longer for its batch window than for
// the durable write itself, and it costs more than half the throughput a wider
// window is supposed to buy. Zero is no better — the batch timer then fires
// before anyone can join, and the rate collapses to db.Update's.
//
// 250µs sits at the top of both curves on the reference host: an uncontended
// write stays at parity with db.Update, and eight concurrent writers coalesce to
// roughly 7.5x it. Re-run the sweep before changing this; the fsync cost it is
// balanced against is host-specific.
const (
	metadataBatchDelay = 250 * time.Microsecond
	metadataBatchSize  = 64
)

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
	db.MaxBatchDelay, db.MaxBatchSize = metadataBatchDelay, metadataBatchSize
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

func (s *Store) PutLedgerHMACEnvelope(value []byte) error {
	if len(value) == 0 {
		return errors.New("ledger HMAC envelope cannot be empty")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyLedgerHMACEnvelope) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyLedgerHMACEnvelope, value)
	})
}

func (s *Store) LedgerHMACEnvelope() ([]byte, error) {
	return s.metaBytes(keyLedgerHMACEnvelope)
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

// KeySlotInitialization is the complete cryptographic root state for a new
// external-KMS instance. It is published in one bbolt transaction so no reader
// can observe a descriptor without its matching Vault Key Check, Keyring,
// protected Audit key and Audit checkpoint.
type KeySlotInitialization struct {
	Descriptor         masterkey.KeySlotDescriptor
	Keyring            VaultKeyring
	VaultKeyCheck      []byte
	AuditHMACEnvelope  []byte
	AuditCheckpoint    AuditCheckpoint
	LedgerHMACEnvelope []byte
	Unwrapper          masterkey.SlotUnwrapper
	Verifier           masterkey.CandidateVerifier
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
	Context            context.Context
	VaultKeyCheck      []byte
	AuditHMACEnvelope  []byte
	LedgerHMACEnvelope []byte
	Keyring            VaultKeyring
	KeySlotDescriptor  *masterkey.KeySlotDescriptor
	Transform          func(domain.Credential) (domain.Credential, error)
	TransformAdminMFA  func(domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error)
	Unwrapper          masterkey.SlotUnwrapper
	Verifier           masterkey.CandidateVerifier
}

// PutLedgerChainCheckpoint advances the trusted chain-head watermark. Same
// monotonicity discipline as PutAuditCheckpoint: a sequence that goes
// backwards is rejected outright, and a conflicting record at an
// already-seen sequence means the chain diverged from what was previously
// observed — both are refused rather than silently overwritten.
func (s *Store) PutLedgerChainCheckpoint(checkpoint LedgerChainCheckpoint) error {
	if checkpoint.Offset < 0 || (checkpoint.Sequence == 0 && (checkpoint.Offset != 0 ||
		checkpoint.Hash != [32]byte{} || checkpoint.Generation != 0)) ||
		(checkpoint.Sequence > 0 && (checkpoint.Generation == 0 || checkpoint.Hash == [32]byte{})) {
		return errors.New("ledger chain checkpoint is invalid")
	}
	// A sealed generation's successor starts empty, so a head at offset zero is
	// a real position from the second generation on — but never in the first,
	// where offset zero means no frame has been written at all.
	if checkpoint.Sequence > 0 && checkpoint.Offset == 0 && checkpoint.Generation == 1 {
		return errors.New("ledger chain checkpoint is invalid")
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(keyLedgerChainCheckpoint); raw != nil {
			var current LedgerChainCheckpoint
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode current ledger chain checkpoint: %w", err)
			}
			if checkpoint.Sequence < current.Sequence {
				return errors.New("ledger chain checkpoint cannot move backwards")
			}
			// At an unchanged sequence the only legitimate movement is a roll:
			// same records, same chain head, a later generation. Anything else
			// is the chain disagreeing with itself about history it already
			// recorded.
			if checkpoint.Sequence == current.Sequence && checkpoint != current {
				rolled := checkpoint.Generation > current.Generation &&
					checkpoint.Hash == current.Hash && checkpoint.Offset == 0
				if !rolled {
					return errors.New("ledger chain checkpoint conflicts at the same sequence")
				}
			}
		}
		return meta.Put(keyLedgerChainCheckpoint, encoded)
	})
}

func (s *Store) LedgerChainCheckpoint() (LedgerChainCheckpoint, error) {
	var checkpoint LedgerChainCheckpoint
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyLedgerChainCheckpoint)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			return fmt.Errorf("decode ledger chain checkpoint: %w", err)
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
	normalizeProviderProfile(&records.Provider, domain.EvidenceDeclared)
	normalizeDeploymentProfile(&records.Deployment, records.Provider, domain.EvidenceDeclared)
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
				// The deployment is created by this same transaction, so nothing
				// can hold an audited timeline for it yet. Dropped anyway: the
				// rule is that every path writing a version record drops the
				// cache, and an exception here is one an audit has to re-derive.
				s.invalidateDeploymentPricingTimeline(records.Deployment.ID)
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
		bucketAdminAuditIntents,
		bucketPricingIdempotency,
		bucketDeploymentPriceProposals,
		bucketPricingProposalIdempotency,
		bucketCostAdjustmentIntents,
		bucketModelCapabilityDetections,
		bucketCapabilityDetectionIdem,
		bucketCapabilityDetectionIndex,
		bucketUsageDailyRollup,
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

func capabilityEvidenceRank(value domain.CapabilityEvidence) int {
	switch value {
	case domain.EvidenceVerified:
		return 3
	case domain.EvidenceDeclared:
		return 2

	default:
		return 0
	}
}

func validSHA256Label(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
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
