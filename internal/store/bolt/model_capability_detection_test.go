package bolt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func storedDetection(now time.Time) domain.ModelCapabilityDetection {
	return domain.ModelCapabilityDetection{ID: "mcd_one", ProviderID: "prv_one", ProviderRevision: 1, CredentialRevision: 1,
		ProviderModel: "model", ModelRevision: "sha256:model",
		Candidates: []domain.DetectionBindingCandidate{{BindingID: "binding", ProfileID: domain.ProfileOpenAIChatEmbeddings,
			AccessSurface: domain.SurfaceOpenAI, ModelRevision: "sha256:model", Status: domain.ProbeNotProbed}},
		BindingID: "binding", ProfileID: domain.ProfileOpenAIChatEmbeddings,
		AccessSurface: domain.SurfaceOpenAI, TargetKind: domain.TargetModelID, CanonicalTarget: "model",
		SelectionFingerprint: "sha256:selection", TargetFingerprint: "sha256:target",
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
	created, replayed, err := store.CreateModelCapabilityDetection(context.Background(), original, time.Now().UTC())
	if err != nil || replayed || created.Revision != 1 {
		t.Fatalf("created=%#v replayed=%v err=%v", created, replayed, err)
	}
	retry := original
	retry.ID = "mcd_retry"
	got, replayed, err := store.CreateModelCapabilityDetection(context.Background(), retry, time.Now().UTC())
	if err != nil || !replayed || got.ID != original.ID {
		t.Fatalf("got=%#v replayed=%v err=%v", got, replayed, err)
	}
	conflict := retry
	conflict.RequestHash = "sha256:different"
	if _, _, err := store.CreateModelCapabilityDetection(context.Background(), conflict, time.Now().UTC()); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err=%v", err)
	}
	otherKey := retry
	otherKey.IdempotencyKeyHash = "sha256:other-key"
	got, replayed, err = store.CreateModelCapabilityDetection(context.Background(), otherKey, time.Now().UTC())
	if err != nil || !replayed || got.ID != original.ID {
		t.Fatalf("singleflight got=%#v replayed=%v err=%v", got, replayed, err)
	}
	otherKey.RequestHash = "sha256:different-after-singleflight"
	if _, _, err := store.CreateModelCapabilityDetection(context.Background(), otherKey, time.Now().UTC()); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("singleflight idempotency conflict err=%v", err)
	}

	// The assertions above are sequential, and a sequential replay of a record
	// that already exists is not single-flight — it is a lookup. The property
	// the name claims only shows up under a race: distinct callers arriving at
	// the same target at the same moment must collapse onto one detection,
	// because each one that does not is a billable Provider call nobody asked
	// for.
	racing, err := Open(filepath.Join(t.TempDir(), "racing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer racing.Close()
	const racers = 8
	var wait sync.WaitGroup
	var fresh, collapsed atomic.Int64
	identities := make([]string, racers)
	var identityMu sync.Mutex
	for index := 0; index < racers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			candidate := storedDetection(now)
			candidate.ID = fmt.Sprintf("mcd_race_%d", index)
			candidate.IdempotencyKeyHash = fmt.Sprintf("sha256:race-key-%d", index)
			stored, replayed, err := racing.CreateModelCapabilityDetection(context.Background(), candidate, time.Now().UTC())
			if err != nil {
				t.Errorf("racer %d: %v", index, err)
				return
			}
			if replayed {
				collapsed.Add(1)
			} else {
				fresh.Add(1)
			}
			identityMu.Lock()
			identities[index] = stored.ID
			identityMu.Unlock()
		}(index)
	}
	wait.Wait()
	if fresh.Load() != 1 || collapsed.Load() != racers-1 {
		t.Fatalf("%d concurrent creates produced %d detections and %d replays", racers, fresh.Load(), collapsed.Load())
	}
	for index, id := range identities {
		if id != identities[0] {
			t.Fatalf("racer %d landed on detection %q while racer 0 landed on %q", index, id, identities[0])
		}
	}
}

func TestResetMigrationDiscardsExistingDetections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	project := domain.Project{ID: "prj_preserved", Name: "preserved", Enabled: false, CreatedAt: now, UpdatedAt: now}
	if _, err := store.PutProject(context.Background(), project, 0, nil); err != nil {
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
		for index, name := range [][]byte{bucketModelCapabilityDetections, bucketCapabilityDetectionIdem, bucketCapabilityDetectionIndex} {
			if err := tx.Bucket(name).Put([]byte("stale"), []byte{byte(index + 1)}); err != nil {
				return err
			}
		}
		var schema [8]byte
		binary.BigEndian.PutUint64(schema[:], 24)
		if err := tx.Bucket(bucketMeta).Put(keySchemaVersion, schema[:]); err != nil {
			return err
		}
		for _, version := range []uint64{25, 26, 27} {
			if err := tx.Bucket(bucketMigrationHistory).Delete(versionKey(version)); err != nil {
				return err
			}
		}
		return nil
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
	if err := store.db.View(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketModelCapabilityDetections, bucketCapabilityDetectionIdem, bucketCapabilityDetectionIndex} {
			if keys := tx.Bucket(name).Stats().KeyN; keys != 0 {
				t.Errorf("reset bucket %q retained %d records", name, keys)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	preserved, err := store.ListProjects(context.Background())
	if err != nil || len(preserved) != 1 || !slices.Equal([]string{preserved[0].ID}, []string{project.ID}) {
		t.Fatalf("unrelated project was changed: %#v err=%v", preserved, err)
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
	d.Calls = []domain.DetectionProviderCall{{Sequence: 1, BindingID: d.BindingID, Capability: "chat", ProbeKind: "minimal_chat", Status: "running", StartedAt: &now}}
	d.Results["chat"] = domain.CapabilityProbeResult{Status: domain.ProbeInconclusive, BindingID: d.BindingID, ProbeKind: "minimal_chat", StartedAt: &now}
	d, _, err = store.CreateModelCapabilityDetection(context.Background(), d, time.Now().UTC())
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

// Startup recovery is the only thing that moves possibly-billable calls to a
// terminal state, so it has to reach every in-flight record, not most of them.
//
// This is a regression guard rather than a reproduction: the walk used to write
// under its own cursor, which bbolt leaves undefined rather than deterministically
// wrong, so the defect state does not reliably fail. What the test pins is the
// property that matters — nothing in flight is left behind, and the count
// reports what was actually written — across enough records to span many pages,
// with values that grow when rewritten.
func TestEveryInFlightDetectionIsInterruptedAcrossManyPages(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	const inFlight = 600
	for i := range inFlight {
		d := storedDetection(now)
		d.ID = fmt.Sprintf("mcd_%04d", i)
		d.IdempotencyKeyHash = fmt.Sprintf("sha256:key-%04d", i)
		d.SelectionFingerprint = fmt.Sprintf("sha256:selection-%04d", i)
		d.Status, d.StartedAt = domain.DetectionRunning, &now
		d.ProviderCalls = 1
		d.Calls = []domain.DetectionProviderCall{{
			Sequence: 1, BindingID: d.BindingID, Capability: "chat",
			ProbeKind: "minimal_chat", Status: "running", StartedAt: &now,
		}}
		d.Results["chat"] = domain.CapabilityProbeResult{
			Status: domain.ProbeInconclusive, BindingID: d.BindingID, ProbeKind: "minimal_chat", StartedAt: &now,
		}
		if _, _, err := store.CreateModelCapabilityDetection(context.Background(), d, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	// A terminal record must be left alone, so the walk cannot pass by writing to
	// everything it sees.
	settled := storedDetection(now)
	settled.ID, settled.IdempotencyKeyHash, settled.SelectionFingerprint = "mcd_settled", "sha256:key-settled", "sha256:selection-settled"
	settled.Status, settled.CompletedAt = domain.DetectionFailed, &now
	if _, _, err := store.CreateModelCapabilityDetection(context.Background(), settled, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	recoveredAt := now.Add(time.Minute)
	count, err := store.InterruptModelCapabilityDetections(context.Background(), recoveredAt)
	if err != nil {
		t.Fatal(err)
	}
	if count != inFlight {
		t.Fatalf("recovery reported %d interrupted detections, want %d", count, inFlight)
	}
	all, err := store.ListModelCapabilityDetections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != inFlight+1 {
		t.Fatalf("recovery changed how many detections exist: %d", len(all))
	}
	for _, d := range all {
		if d.ID == settled.ID {
			if d.Status != domain.DetectionFailed {
				t.Fatalf("a settled detection was rewritten by recovery: %#v", d)
			}
			continue
		}
		if d.Status != domain.DetectionInterrupted {
			t.Fatalf("%s was left in flight after recovery: status=%s", d.ID, d.Status)
		}
		if len(d.Calls) != 1 || d.Calls[0].Status != "unknown" {
			t.Fatalf("%s kept a possibly-billable call in a non-terminal state: %#v", d.ID, d.Calls)
		}
	}
}
