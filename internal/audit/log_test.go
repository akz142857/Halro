package audit

import (
	"context"
	"crypto/rand"
	"errors"
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

func validEvent(index int, action string) Event {
	return Event{
		EventID:    "audit_" + string(rune('a'+index)),
		OccurredAt: time.Date(2026, 7, 31, 12, index, 0, 0, time.UTC),
		ActorType:  "system", Action: action, TargetType: "gateway",
		TargetID: "local", Outcome: "success",
	}
}
