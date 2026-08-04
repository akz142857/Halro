package bolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func TestDeploymentPriceTimelineCreateSelectAndCancel(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	seedPricingDeployment(t, store, "dep_price", 0, 0, 0)

	first, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_one", "dep_price", now.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.Revision != 1 {
		t.Fatalf("first version=%d revision=%d", first.Version, first.Revision)
	}
	second, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_two", "dep_price", now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 {
		t.Fatalf("second version=%d", second.Version)
	}
	duplicate := newStoredPrice("price_duplicate", "dep_price", second.EffectiveFrom)
	if _, err := store.CreateDeploymentPriceVersion(ctx, duplicate); !errors.Is(err, domain.ErrPriceTimelineConflict) {
		t.Fatalf("duplicate effective time error=%v", err)
	}
	selected, err := store.SelectDeploymentPriceVersion(ctx, "dep_price", now)
	if err != nil || selected.ID != first.ID {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	selected, err = store.SelectDeploymentPriceVersion(ctx, "dep_price", now.Add(2*time.Hour))
	if err != nil || selected.ID != second.ID {
		t.Fatalf("future selected=%#v err=%v", selected, err)
	}
	second, err = store.CancelDeploymentPriceVersion(ctx, "dep_price", second.ID, "admin_one", now, second.Revision)
	if err != nil || second.CancelledAt == nil || second.Revision != 2 {
		t.Fatalf("cancelled=%#v err=%v", second, err)
	}
	selected, err = store.SelectDeploymentPriceVersion(ctx, "dep_price", now.Add(2*time.Hour))
	if err != nil || selected.ID != first.ID {
		t.Fatalf("post-cancel selected=%#v err=%v", selected, err)
	}
	if _, err := store.CancelDeploymentPriceVersion(ctx, "dep_price", first.ID, "admin_one", now, first.Revision); !errors.Is(err, domain.ErrPriceVersionUnavailable) {
		t.Fatalf("active cancellation error=%v", err)
	}
	if _, err := store.CancelDeploymentPriceVersion(ctx, "dep_price", second.ID, "admin_one", now, 1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale cancellation error=%v", err)
	}
}

func TestVersionedPricingMigrationPreservesLegacyPriceAsEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedPricingDeployment(t, store, "dep_metered", 100, 200, 3)
	seedPricingDeployment(t, store, "dep_unknown", 0, 0, 0)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range [][]byte{bucketDeploymentPriceVersions, bucketDeploymentPriceTimeline, bucketDeploymentPriceNext, bucketDeploymentPricingHighWater, bucketPricingAuditIntents, bucketPricingIdempotency} {
			if err := tx.DeleteBucket(bucket); err != nil && !errors.Is(err, bbolt.ErrBucketNotFound) {
				return err
			}
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], 10)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encoded[:])
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	metered, err := store.ListDeploymentPriceVersions(context.Background(), "dep_metered")
	if err != nil || len(metered) != 1 {
		t.Fatalf("metered versions=%#v err=%v", metered, err)
	}
	price := metered[0]
	if price.Version != 1 || price.BillingMode != domain.BillingModeMetered ||
		price.InputMicrosPerMillion != 100 || price.OutputMicrosPerMillion != 200 || price.FixedRequestMicrosUSD != 3 ||
		price.Source.Type != domain.PriceSourceMigration || price.Source.OriginalResourceID != "dep_metered" {
		t.Fatalf("migrated price=%#v", price)
	}
	unknown, err := store.ListDeploymentPriceVersions(context.Background(), "dep_unknown")
	if err != nil || len(unknown) != 0 {
		t.Fatalf("zero legacy price was guessed instead of remaining unknown: %#v err=%v", unknown, err)
	}
}

func TestVersionedPricingMigrationRejectsEnabledAmbiguousZeroPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedPricingDeployment(t, store, "dep_ambiguous", 0, 0, 0)
	deployment, err := store.GetDeployment(context.Background(), "dep_ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	deployment.Enabled = true
	if _, err := store.PutDeployment(context.Background(), deployment, deployment.Revision); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range [][]byte{bucketDeploymentPriceVersions, bucketDeploymentPriceTimeline, bucketDeploymentPriceNext, bucketDeploymentPricingHighWater, bucketPricingAuditIntents, bucketPricingIdempotency} {
			if err := tx.DeleteBucket(bucket); err != nil && !errors.Is(err, bbolt.ErrBucketNotFound) {
				return err
			}
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], 10)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encoded[:])
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "pricing migration readiness failed") {
		t.Fatalf("upgrade error=%v", err)
	}
}

func TestPricingMutationAndAuditIntentCommitAtomically(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_audit", 0, 0, 0)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	price := newStoredPrice("price_audit", "dep_audit", now.Add(time.Hour))
	intent := domain.PricingAuditIntent{
		EventID: "aud_price_create", OccurredAt: now, ActorID: "admin_one",
		Action: "deployment_price.create", TargetType: "deployment_price_version", TargetID: price.ID,
		RequestSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	stored, err := store.CreateDeploymentPriceVersionWithAuditIntent(ctx, price, intent)
	if err != nil || stored.Version != 1 {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	pending, err := store.ListPendingPricingAuditIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != intent.EventID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	if err := store.MarkPricingAuditIntentDelivered(ctx, intent.EventID); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingPricingAuditIntents(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("delivered intent remained pending=%#v err=%v", pending, err)
	}

	conflict := intent
	conflict.EventID = "aud_wrong_target"
	conflict.TargetID = "another_price"
	if _, err := store.CreateDeploymentPriceVersionWithAuditIntent(ctx, newStoredPrice("price_second", "dep_audit", now.Add(2*time.Hour)), conflict); err == nil {
		t.Fatal("mismatched audit intent was accepted")
	}
	versions, err := store.ListDeploymentPriceVersions(ctx, "dep_audit")
	if err != nil || len(versions) != 1 {
		t.Fatalf("failed atomic mutation changed timeline=%#v err=%v", versions, err)
	}
}

func TestDeploymentPricePinPersistsHighWaterAndCommitsLedgerSequence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_pin", 0, 0, 0)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	price, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_pin", "dep_pin", now.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	selected, snapshot, pin, err := store.PrepareDeploymentPricePin(ctx, "dep_pin", "att_pin", now, 2*time.Second, 30*time.Second)
	if err != nil || selected.ID != price.ID || pin.State != domain.PricePinPrepared {
		t.Fatalf("selected=%#v pin=%#v err=%v", selected, pin, err)
	}
	digest, _ := snapshot.Digest()
	if digest != pin.SnapshotSHA256 {
		t.Fatalf("snapshot digest=%q pin=%q", digest, pin.SnapshotSHA256)
	}
	committed, err := store.CommitDeploymentPricePin(ctx, pin.AttemptID, digest, 42, now.Add(time.Second))
	if err != nil || committed.State != domain.PricePinCommitted || committed.LedgerSequence != 42 {
		t.Fatalf("committed=%#v err=%v", committed, err)
	}
	var highWater domain.DeploymentPricingHighWater
	if err := store.db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte("dep_pin")), &highWater)
	}); err != nil || !highWater.LatestSelectedAt.Equal(now) || !highWater.LatestObservedEffectiveFrom.Equal(price.EffectiveFrom) {
		t.Fatalf("high-water=%#v err=%v", highWater, err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_pin", "att_rollback", now.Add(-3*time.Second), 2*time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("rollback error=%v", err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_pin", "att_after_quarantine", now.Add(time.Second), 2*time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("quarantine was not persistent: %v", err)
	}
	if err := store.PricingReadiness(ctx); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("pricing readiness error=%v", err)
	}
}

func TestDeploymentPricePinQuarantinesUnexplainedForwardJump(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_forward", 0, 0, 0)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_forward_1", "dep_forward", now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_forward_2", "dep_forward", now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_forward", "att_forward_1", now, 2*time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_forward", "att_forward_2", now.Add(2*time.Minute), 2*time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("forward jump error=%v", err)
	}
	var highWater domain.DeploymentPricingHighWater
	if err := store.db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte("dep_forward")), &highWater)
	}); err != nil || highWater.QuarantineReason != "wall_clock_forward_jump" {
		t.Fatalf("high-water=%#v err=%v", highWater, err)
	}
}

func TestScheduledPriceCancellationIsBlockedByDurablePin(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_cancel_pin", 0, 0, 0)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	scheduled, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_cancel_pin", "dep_cancel_pin", now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_cancel_pin", "att_cancel_pin", now.Add(2*time.Hour), time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelDeploymentPriceVersion(ctx, "dep_cancel_pin", scheduled.ID, "admin", now, scheduled.Revision); !errors.Is(err, domain.ErrPriceVersionUnavailable) {
		t.Fatalf("pinned cancellation error=%v", err)
	}
	if err := store.DeletePreparedDeploymentPricePin(ctx, "att_cancel_pin"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelDeploymentPriceVersion(ctx, "dep_cancel_pin", scheduled.ID, "admin", now, scheduled.Revision); err != nil {
		t.Fatalf("unpinned cancellation error=%v", err)
	}
}

func TestPreparedPricePinRecoveryUsesLedgerSnapshotOrDeletesOrphan(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_recover_pin", 0, 0, 0)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_recover_pin", "dep_recover_pin", now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	_, snapshot, durablePin, err := store.PrepareDeploymentPricePin(ctx, "dep_recover_pin", "att_durable", now, time.Second, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_recover_pin", "att_orphan", now, time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	state := ledger.NewState()
	if err := state.Apply(ledger.Record{Sequence: 1, Offset: 100, Event: ledger.Event{
		EventID: "evt_lease", Kind: ledger.EventReservationCreated, RequestID: "req_pin", AttemptID: durablePin.AttemptID,
		ProjectID: "prj_pin", PeriodID: "2026-08-04", OccurredAt: now, ReservationMicrosUSD: ledger.MicrosUSD(1),
		LeaseMode: ledger.LeaseModeMetered, PriceSnapshot: &snapshot,
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverDeploymentPricePins(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := store.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeploymentPricePins)
		if bucket.Get([]byte("att_orphan")) != nil {
			return errors.New("orphan prepared pin was not deleted")
		}
		var recovered domain.PricePinIntent
		if err := json.Unmarshal(bucket.Get([]byte("att_durable")), &recovered); err != nil {
			return err
		}
		if recovered.State != domain.PricePinCommitted || recovered.LedgerSequence != 1 {
			return fmt.Errorf("recovered pin=%#v", recovered)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreIncoherentPricingHighWaterIsPersistentlyQuarantined(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_restore", 0, 0, 0)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_restore", "dep_restore", now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_restore", "att_restore", now, time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePreparedDeploymentPricePin(ctx, "att_restore"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeploymentPricingHighWater)
		var highWater domain.DeploymentPricingHighWater
		if err := json.Unmarshal(bucket.Get([]byte("dep_restore")), &highWater); err != nil {
			return err
		}
		highWater.LatestObservedPriceVersionID = "price_missing_after_restore"
		encoded, err := json.Marshal(highWater)
		if err != nil {
			return err
		}
		return bucket.Put([]byte("dep_restore"), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverDeploymentPricePins(ctx, ledger.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := store.PricingReadiness(ctx); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("pricing readiness error=%v", err)
	}
}

func TestRestoreQuarantinesNewlyDueScheduledPriceUntilAuditedConfirmation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedPricingDeployment(t, store, "dep_restore_scheduled", 0, 0, 0)
	backupAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	scheduled := newStoredPrice("price_restore_scheduled", "dep_restore_scheduled", backupAt.Add(time.Hour))
	if _, err := store.CreateDeploymentPriceVersion(ctx, scheduled); err != nil {
		t.Fatal(err)
	}
	quarantined, err := store.QuarantineRestoredScheduledPrices(ctx, backupAt, backupAt.Add(2*time.Hour))
	if err != nil || quarantined != 1 {
		t.Fatalf("quarantined=%d err=%v", quarantined, err)
	}
	if err := store.PricingReadiness(ctx); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("pricing readiness error=%v", err)
	}
	if _, _, _, err := store.PrepareDeploymentPricePin(ctx, "dep_restore_scheduled", "att_blocked", backupAt.Add(2*time.Hour), time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("prepare while quarantined error=%v", err)
	}
	intent := domain.PricingAuditIntent{
		EventID: "aud_restore_confirm", OccurredAt: backupAt.Add(2 * time.Hour), ActorID: "admin",
		Action: "deployment_price.restore_confirm", TargetType: "deployment_price_version",
		TargetID: "dep_restore_scheduled", RequestSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if err := store.ConfirmRestoredPricingWithAuditIntent(ctx, "dep_restore_scheduled", intent); err != nil {
		t.Fatal(err)
	}
	if err := store.PricingReadiness(ctx); err != nil {
		t.Fatalf("pricing remained quarantined: %v", err)
	}
	pending, err := store.ListPendingPricingAuditIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != intent.EventID {
		t.Fatalf("pending audit intents=%#v err=%v", pending, err)
	}
}

func seedPricingDeployment(t *testing.T, store *Store, deploymentID string, input, output, fixed int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	credentialID := "cred_" + deploymentID
	providerID := "provider_" + deploymentID
	credential, err := store.PutCredential(ctx, domain.Credential{
		ID: credentialID, Name: credentialID, Type: domain.ProviderOpenAI,
		AccessSurface: profile.AccessSurface, Scheme: profile.CredentialScheme,
		Audience: "audience", Ciphertext: []byte("ciphertext"), KeyVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := domain.ProviderCapabilities{Chat: true, Streaming: true}
	provider, err := store.PutProvider(ctx, domain.ProviderInstance{
		ID: providerID, Name: providerID, Type: domain.ProviderOpenAI,
		BaseURL: "https://api.openai.com", CredentialID: credential.ID,
		AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID, CredentialScheme: profile.CredentialScheme,
		AllowedHosts: []string{"api.openai.com"}, Capabilities: capabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
		Enabled:            true, CreatedAt: now, UpdatedAt: now,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.PutDeployment(ctx, domain.Deployment{
		ID: deploymentID, Name: deploymentID, ProviderID: provider.ID, ProviderModel: "gpt-test",
		TargetKind: domain.TargetModelID, AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID,
		BindingID:    domain.DefaultProviderProfileBindingID(provider.ID, profile.ProfileID),
		Capabilities: capabilities, CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
		InputMicrosPerMillion: input, OutputMicrosPerMillion: output, FixedRequestMicrosUSD: fixed,
		Weight: 1, Enabled: false, CreatedAt: now, UpdatedAt: now,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
}

func newStoredPrice(id, deploymentID string, effective time.Time) domain.DeploymentPriceVersion {
	received := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	return domain.DeploymentPriceVersion{
		ID: id, DeploymentID: deploymentID, BillingMode: domain.BillingModeMetered,
		Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 400_000, OutputMicrosPerMillion: 1_600_000,
		EffectiveFrom: effective, CreatedBy: "admin_one", CreatedAt: received,
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: received, ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Reference: "contract schedule", AssertedWithoutArchive: true},
	}
}
