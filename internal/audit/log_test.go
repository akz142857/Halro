package audit

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditRoundTripAndVerification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	for index, action := range []string{"system.startup", "key.create", "system.shutdown"} {
		if _, err := log.Append(context.Background(), validEvent(index, action)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := Verify(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records != 3 || summary.Bytes == 0 || summary.LastHash == [32]byte{} {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestAppendBatchProducesOneConsecutiveDurableChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{
		validEvent(1, "project.create"),
		validEvent(2, "provider.update"),
		validEvent(3, "route.delete"),
	}
	records, err := log.AppendBatch(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(events) || records[0].Sequence != 1 || records[2].Sequence != 3 {
		t.Fatalf("records=%#v", records)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := Verify(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records != 3 || summary.LastHash != records[2].Hash {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestAppendIsIdempotentByEventIDAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	event := validEvent(7, "admin.mfa.disabled")
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	first, err := log.Append(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	second, err := log.Append(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != second.Sequence || log.Summary().Records != 1 {
		t.Fatalf("duplicate event appended: first=%#v second=%#v summary=%#v", first, second, log.Summary())
	}
	if err = log.Close(); err != nil {
		t.Fatal(err)
	}
	log, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	third, err := log.Append(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if third.Sequence != first.Sequence || log.Summary().Records != 1 {
		t.Fatalf("reopened duplicate appended: %#v", third)
	}
}

func TestAppendRejectsConflictingPayloadForEventID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	event := validEvent(8, "security.master_key_slot.revoked")
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	log, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	conflict := event
	conflict.ReasonCode = "incident_retirement"
	if _, err := log.Append(context.Background(), conflict); err == nil {
		t.Fatal("duplicate event ID accepted a different payload")
	}
	if log.Summary().Records != 1 {
		t.Fatalf("conflicting event changed Audit summary: %#v", log.Summary())
	}
}

func TestAuditDetectsTamperingAndWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validEvent(1, "key.create")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, randomKey(t)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong key must fail verification: %v", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, frameHeaderSize+5); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, key); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampering must fail verification: %v", err)
	}
}

func TestOpenTruncatesOnlyPartialAuditTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validEvent(1, "system.startup")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("HAUD")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	log, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Size() != info.Size() {
		t.Fatalf("repaired size=%d want=%d", repaired.Size(), info.Size())
	}
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, auditHMACKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// The dedup index answers "did I already append this event ID". Answering it
// from every historical record turns an append-only log into unbounded resident
// memory and pins every audited payload in the heap, so the index keeps a tail
// window. This test pins both halves of that trade: the window stays bounded,
// and it still covers the recent events duplicates actually arrive for.
func TestDedupIndexKeepsABoundedTailWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	total := maxTrackedEventIDs + 1024
	for start := 0; start < total; start += 512 {
		batch := make([]Event, 0, 512)
		for index := start; index < start+512 && index < total; index++ {
			batch = append(batch, sequencedEvent(index))
		}
		if _, err := log.AppendBatch(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if tracked := len(log.recordsByEventID); tracked > maxTrackedEventIDs {
		t.Fatalf("dedup index holds %d records after %d events, want at most %d",
			tracked, total, maxTrackedEventIDs)
	}
	if ordered := len(log.trackedOrder); ordered != len(log.recordsByEventID) {
		t.Fatalf("eviction order holds %d ids for %d indexed records", ordered, len(log.recordsByEventID))
	}

	sequenceBefore := log.sequence
	record, err := log.Append(context.Background(), sequencedEvent(total-1))
	if err != nil {
		t.Fatal(err)
	}
	if log.sequence != sequenceBefore {
		t.Fatalf("re-appending a tracked event wrote a new frame: sequence %d -> %d", sequenceBefore, log.sequence)
	}
	if record.Sequence != uint64(total) {
		t.Fatalf("deduplicated record returned sequence %d, want %d", record.Sequence, total)
	}

	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// Open rebuilds the index by scanning the whole file, so it has to apply the
	// same bound the append path does.
	reopened, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if tracked := len(reopened.recordsByEventID); tracked > maxTrackedEventIDs {
		t.Fatalf("reopening rebuilt an unbounded index: %d records, want at most %d",
			tracked, maxTrackedEventIDs)
	}
}

func sequencedEvent(index int) Event {
	return Event{
		EventID:    fmt.Sprintf("audit_%08d", index),
		OccurredAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		ActorType:  "system", Action: "system.probe", TargetType: "gateway",
		TargetID: "local", Outcome: "success",
	}
}

func validEvent(index int, action string) Event {
	return Event{
		EventID:    "audit_" + string(rune('a'+index)),
		OccurredAt: time.Date(2026, 7, 31, 12, index, 0, 0, time.UTC),
		ActorType:  "system", Action: action, TargetType: "gateway",
		TargetID: "local", Outcome: "success",
	}
}
