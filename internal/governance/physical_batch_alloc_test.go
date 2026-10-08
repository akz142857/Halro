package governance

import (
	"context"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// The native physical window charges PhysicalBatchRAMBytes for one encoder.
// Total allocation during a maximum HA batch (replication hooks set, so the
// whole encoded batch is retained) bounds the encoder's live peak from above.
func TestGovernanceHAMaximumBatchAllocationFitsPhysicalBatchFee(t *testing.T) {
	path, key := filepath.Join(t.TempDir(), "governance.wal"), make([]byte, 32)
	log, err := OpenWithOptions(path, key, Options{
		BeforeDurable: func(DurableBatch) (AppendAdmission, error) { return nil, nil },
		AfterDurable:  func(DurableBatch) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events := make([]Event, MaxBatchEvents)
	for i := range events {
		events[i] = projectionEvent(i)
		events[i].ProjectID = strings.Repeat("x", 15800)
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := log.AppendBatch(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	summary := log.Summary()
	allocated := int64(after.TotalAlloc - before.TotalAlloc)
	t.Logf("maximum HA batch: %d native bytes, %d bytes allocated (fee %d)", summary.Bytes, allocated, PhysicalBatchRAMBytes)
	if summary.Bytes < MaxBatchPayloadBytes*15/16 {
		t.Fatal("fixture is not a near-maximum batch", summary.Bytes)
	}
	if allocated > PhysicalBatchRAMBytes {
		t.Fatal("maximum batch allocation exceeds the encoder fee", allocated, PhysicalBatchRAMBytes)
	}
}
