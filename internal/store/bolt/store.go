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

const schemaVersion uint64 = 27

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
	keySchemaVersion                 = []byte("schema_version")
	keyVaultCheck                    = []byte("vault_key_check")
	keyUsageCheckpoint               = []byte("usage_checkpoint")
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
	Sequence uint64   `json:"sequence"`
	Offset   int64    `json:"offset"`
	Hash     [32]byte `json:"hash"`
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
		checkpoint.Hash != [32]byte{})) ||
		(checkpoint.Sequence > 0 && (checkpoint.Offset == 0 || checkpoint.Hash == [32]byte{})) {
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
			if checkpoint.Sequence == current.Sequence && checkpoint != current {
				return errors.New("ledger chain checkpoint conflicts at the same sequence")
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
