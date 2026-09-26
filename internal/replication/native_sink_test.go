package replication

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
)

func TestNativeSinkAuthenticatesAuditBeforeWriting(t *testing.T) {
	directory := t.TempDir()
	key := []byte("0123456789abcdef0123456789abcdef")
	var native [][]byte
	primary, err := audit.OpenWithOptions(filepath.Join(directory, "primary.log"), key, audit.Options{
		AfterDurable: func(batch audit.DurableBatch) error {
			native = append(native, append([]byte(nil), batch.Frames...))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Append(context.Background(), audit.Event{
		EventID: "evt_1", OccurredAt: time.Now().UTC(), ActorType: "system",
		Action: "test", Outcome: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Append(context.Background(), audit.Event{
		EventID: "evt_2", OccurredAt: time.Now().UTC(), ActorType: "system",
		Action: "test", Outcome: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	replicaPath := filepath.Join(directory, "replica.log")
	replica, err := audit.OpenWithOptions(replicaPath, key, audit.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	sink := &NativeSink{audit: replica}
	frame := Frame{Kind: KindData, Store: StoreAudit, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: native[0]}
	if err := sink.Persist(frame); err != nil {
		t.Fatal(err)
	}
	if err := sink.Persist(frame); err != nil {
		t.Fatalf("exact native retransmission was not idempotent: %v", err)
	}
	if replica.Summary().Records != 1 {
		t.Fatalf("records=%d", replica.Summary().Records)
	}

	broken := append([]byte(nil), native[1]...)
	broken[len(broken)-1] ^= 1
	frame.StoreSequenceFirst, frame.StoreSequenceLast, frame.Payload = 2, 2, broken
	if err := sink.Persist(frame); err == nil || !strings.Contains(err.Error(), "HMAC") {
		t.Fatalf("invalid native frame error=%v", err)
	}
	if replica.Summary().Records != 1 {
		t.Fatalf("invalid frame changed durable records to %d", replica.Summary().Records)
	}
}
