package ledger

import (
	"encoding/json"
	"testing"
	"time"
)

// AppendStats is serialized straight into the Admin API, so its wire shape is a
// contract. This pins it because a time.Duration field would otherwise reach an
// operator's screen as a bare nanosecond integer.
func TestAppendStatsWireShape(t *testing.T) {
	encoded, err := json.Marshal(AppendStats{
		Batches: 3, Records: 12, Errors: 0, Syncs: 3,
		SyncDuration: 27 * time.Millisecond, QueueDepth: 1, QueueCapacity: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"batches", "records", "errors", "syncs", "sync_seconds", "queue_depth", "queue_capacity"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("key %q is missing from %s", key, encoded)
		}
	}
	if _, leaked := decoded["SyncDuration"]; leaked {
		t.Fatalf("the raw nanosecond duration reached the wire: %s", encoded)
	}
	if seconds, _ := decoded["sync_seconds"].(float64); seconds != 0.027 {
		t.Fatalf("sync_seconds=%v, want 0.027", decoded["sync_seconds"])
	}
}
