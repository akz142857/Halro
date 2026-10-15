package bolt

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func openAuditAnchorStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestSeedInstanceIDGeneratesOnceAndIsStable(t *testing.T) {
	store := openAuditAnchorStore(t)
	first, err := store.SeedInstanceID("ins_candidate_one")
	if err != nil || first != "ins_candidate_one" {
		t.Fatalf("first seed=%q err=%v", first, err)
	}
	second, err := store.SeedInstanceID("ins_candidate_two")
	if err != nil || second != first {
		t.Fatalf("reseed changed the instance ID: first=%q second=%q err=%v", first, second, err)
	}
}

func TestAppendAuditAnchorEnforcesSequenceAndPrunesRetention(t *testing.T) {
	store := openAuditAnchorStore(t)
	now := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)

	if err := store.AppendAuditAnchor(AuditAnchor{Sequence: 2, Records: 1, InstanceID: "ins_1", ObservedAt: now}); err == nil {
		t.Fatal("first anchor must start at sequence 1")
	}
	for sequence := uint64(1); sequence <= auditAnchorRetention+10; sequence++ {
		anchor := AuditAnchor{
			Sequence: sequence, Records: sequence, InstanceID: "ins_1",
			ObservedAt: now.Add(time.Duration(sequence) * time.Minute),
		}
		if err := store.AppendAuditAnchor(anchor); err != nil {
			t.Fatalf("append sequence %d: %v", sequence, err)
		}
	}
	if err := store.AppendAuditAnchor(AuditAnchor{Sequence: 5, Records: 5, InstanceID: "ins_1", ObservedAt: now}); err == nil {
		t.Fatal("appending a sequence that does not follow the last one must fail")
	}

	latest, err := store.LatestAuditAnchor()
	if err != nil || latest.Sequence != auditAnchorRetention+10 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}

	all, err := store.AuditAnchorsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != auditAnchorRetention {
		t.Fatalf("retained anchors=%d, want %d — the ring did not prune", len(all), auditAnchorRetention)
	}
	oldestRetained := all[0].Sequence
	wantOldest := uint64(auditAnchorRetention + 10 - auditAnchorRetention + 1)
	if oldestRetained != wantOldest {
		t.Fatalf("oldest retained sequence=%d, want %d", oldestRetained, wantOldest)
	}
}

// Pruning normally has exactly one key to drop per append, because the
// sequence advances by one. That is the only case the ring has ever been
// exercised in, and it is the one case where deleting through a cursor cannot
// go wrong. This seeds the bucket past its retention so a single append has to
// drop several at once — the shape a lowered retention constant or a restored
// longer ring produces — and pins that every key at or below the cutoff is
// actually gone.
func TestAppendAuditAnchorPrunesEveryKeyBelowTheCutoff(t *testing.T) {
	store := openAuditAnchorStore(t)
	now := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)
	const overshoot = 5
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAuditAnchors)
		for sequence := uint64(1); sequence <= auditAnchorRetention+overshoot; sequence++ {
			encoded, err := json.Marshal(AuditAnchor{
				Sequence: sequence, Records: sequence, InstanceID: "ins_1",
				ObservedAt: now.Add(time.Duration(sequence) * time.Minute),
			})
			if err != nil {
				return err
			}
			if err := bucket.Put(auditAnchorKey(sequence), encoded); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	next := uint64(auditAnchorRetention + overshoot + 1)
	if err := store.AppendAuditAnchor(AuditAnchor{
		Sequence: next, Records: next, InstanceID: "ins_1", ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := store.AuditAnchorsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	// The append pushed the cutoff to overshoot+1, so that many keys had to go
	// in one transaction.
	if len(all) != auditAnchorRetention {
		t.Fatalf("retained anchors=%d, want %d — the prune skipped keys it walked over", len(all), auditAnchorRetention)
	}
	if all[0].Sequence != overshoot+2 {
		t.Fatalf("oldest retained sequence=%d, want %d", all[0].Sequence, overshoot+2)
	}
	for index := 1; index < len(all); index++ {
		if all[index].Sequence != all[index-1].Sequence+1 {
			t.Fatalf("retained sequences are not contiguous at %d: %d follows %d",
				index, all[index].Sequence, all[index-1].Sequence)
		}
	}
}

// The sharper half of the same defect. When the keys being pruned share a leaf
// page with the key just written, that page is already a node in this
// transaction, and a cursor that deletes as it walks steps over every second
// element. Seeding a sparse ring puts the whole thing on one page, which is the
// arrangement that shows it: without the fix this leaves 2, 4 and 6 behind.
func TestAppendAuditAnchorPrunesKeysSharingTheWrittenPage(t *testing.T) {
	store := openAuditAnchorStore(t)
	now := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAuditAnchors)
		for _, sequence := range []uint64{1, 2, 3, 4, 5, 6, auditAnchorRetention + 6} {
			encoded, err := json.Marshal(AuditAnchor{
				Sequence: sequence, Records: sequence, InstanceID: "ins_1", ObservedAt: now,
			})
			if err != nil {
				return err
			}
			if err := bucket.Put(auditAnchorKey(sequence), encoded); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	next := uint64(auditAnchorRetention + 7)
	if err := store.AppendAuditAnchor(AuditAnchor{
		Sequence: next, Records: next, InstanceID: "ins_1", ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := store.AuditAnchorsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var retained []uint64
	for _, anchor := range all {
		retained = append(retained, anchor.Sequence)
	}
	// cutoff is next-retention == 7, so every seeded key from 1 to 6 is expired.
	want := []uint64{auditAnchorRetention + 6, next}
	if !slices.Equal(retained, want) {
		t.Fatalf("retained=%v, want %v — the prune stepped over keys it walked", retained, want)
	}
}

func TestAuditAnchorsSinceReturnsOnlyNewerEntriesInOrder(t *testing.T) {
	store := openAuditAnchorStore(t)
	now := time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC)
	for sequence := uint64(1); sequence <= 5; sequence++ {
		if err := store.AppendAuditAnchor(AuditAnchor{
			Sequence: sequence, Records: sequence * 10, InstanceID: "ins_1",
			ObservedAt: now.Add(time.Duration(sequence) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	since, err := store.AuditAnchorsSince(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 2 || since[0].Sequence != 4 || since[1].Sequence != 5 {
		t.Fatalf("since=%#v", since)
	}
	all, err := store.AuditAnchorsSince(0)
	if err != nil || len(all) != 5 {
		t.Fatalf("all=%#v err=%v", all, err)
	}
}

func TestLatestAuditAnchorIsNotFoundBeforeAnyEmission(t *testing.T) {
	store := openAuditAnchorStore(t)
	if _, err := store.LatestAuditAnchor(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	since, err := store.AuditAnchorsSince(0)
	if err != nil || len(since) != 0 {
		t.Fatalf("since=%#v err=%v", since, err)
	}
}
