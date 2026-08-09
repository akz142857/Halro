package bolt

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func storedDetection(now time.Time) domain.ModelCapabilityDetection {
	return domain.ModelCapabilityDetection{ID: "mcd_one", ProviderID: "prv_one", ProviderRevision: 1, CredentialRevision: 1,
		ProviderModel: "model", ModelRevision: "sha256:model", BindingID: "binding", ProfileID: domain.ProfileOpenAIChatEmbeddings,
		AccessSurface: domain.SurfaceOpenAI, TargetKind: domain.TargetModelID, CanonicalTarget: "model", TargetFingerprint: "sha256:target",
		DetectorVersion: "v1", RiskTier: "safe_automatic", Status: domain.DetectionQueued, Source: "verified_probe",
		Results: map[string]domain.CapabilityProbeResult{}, MaxProviderCalls: 8, CreatedBy: "admin",
		IdempotencyKeyHash: "sha256:key", RequestHash: "sha256:request", CreatedAt: now, UpdatedAt: now}
}

func TestSchema24CreatesAndRequiresCapabilityDetectionBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket(bucketCapabilityDetectionIndex) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("schema 24 opened without its fingerprint index")
	}

	// Rewind only migration 24 to model a valid schema-23 data directory. The
	// forward migration must recreate all three empty buckets.
	db, err = bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		var schema [8]byte
		binary.BigEndian.PutUint64(schema[:], 23)
		if err := tx.Bucket(bucketMeta).Put(keySchemaVersion, schema[:]); err != nil {
			return err
		}
		return tx.Bucket(bucketMigrationHistory).Delete(versionKey(24))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("schema 23 forward migration: %v", err)
	}
	defer store.Close()
}

func TestCapabilityDetectionCreateIsIdempotentAndSingleFlight(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	original := storedDetection(now)
	created, replayed, err := store.CreateModelCapabilityDetection(context.Background(), original)
	if err != nil || replayed || created.Revision != 1 {
		t.Fatalf("created=%#v replayed=%v err=%v", created, replayed, err)
	}
	retry := original
	retry.ID = "mcd_retry"
	got, replayed, err := store.CreateModelCapabilityDetection(context.Background(), retry)
	if err != nil || !replayed || got.ID != original.ID {
		t.Fatalf("got=%#v replayed=%v err=%v", got, replayed, err)
	}
	conflict := retry
	conflict.RequestHash = "sha256:different"
	if _, _, err := store.CreateModelCapabilityDetection(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err=%v", err)
	}
	otherKey := retry
	otherKey.IdempotencyKeyHash = "sha256:other-key"
	got, replayed, err = store.CreateModelCapabilityDetection(context.Background(), otherKey)
	if err != nil || !replayed || got.ID != original.ID {
		t.Fatalf("singleflight got=%#v replayed=%v err=%v", got, replayed, err)
	}
	otherKey.RequestHash = "sha256:different-after-singleflight"
	if _, _, err := store.CreateModelCapabilityDetection(context.Background(), otherKey); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("singleflight idempotency conflict err=%v", err)
	}
}

func TestCapabilityDetectionRecoveryInterruptsWithoutReplayingCalls(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	d := storedDetection(now)
	d.Status, d.StartedAt = domain.DetectionRunning, &now
	d.ProviderCalls = 1
	d.Calls = []domain.DetectionProviderCall{{Sequence: 1, Capability: "chat", ProbeKind: "minimal_chat", Status: "running", StartedAt: &now}}
	d.Results["chat"] = domain.CapabilityProbeResult{Status: domain.ProbeInconclusive, BindingID: d.BindingID, ProbeKind: "minimal_chat", StartedAt: &now}
	d, _, err = store.CreateModelCapabilityDetection(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	recoveredAt := now.Add(time.Minute)
	count, err := store.InterruptModelCapabilityDetections(context.Background(), recoveredAt)
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	got, err := store.GetModelCapabilityDetection(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.DetectionInterrupted || got.Calls[0].Status != "unknown" || got.ProviderCalls != 1 {
		t.Fatalf("got=%#v", got)
	}
}
