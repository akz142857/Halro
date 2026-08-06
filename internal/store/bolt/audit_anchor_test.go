package bolt

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
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
