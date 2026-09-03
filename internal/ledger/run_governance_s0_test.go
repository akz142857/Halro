package ledger

// This file is an S0-only capacity probe. It records the current Accounting
// State's lower-bound cardinality cost before Run and Work Unit fields are
// added; it does not define or change a production event format.

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestS0AccountingStateCapacityProfile(t *testing.T) {
	if os.Getenv("HALRO_RUN_GOVERNANCE_S0_PROFILE") != "1" {
		t.Skip("set HALRO_RUN_GOVERNANCE_S0_PROFILE=1 for the S0 Accounting State profile")
	}
	for _, records := range []int{10_000, 100_000, 1_000_000} {
		t.Run(fmt.Sprintf("records=%d", records), func(t *testing.T) {
			runtime.GC()
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			state := NewState()
			started := time.Now()
			for index := 1; index <= records; index++ {
				id := strconv.Itoa(index)
				record := Record{
					Generation: 1,
					Offset:     int64(index),
					Sequence:   uint64(index),
					Event: Event{
						EventID:    "evt_s0_" + id,
						Kind:       EventRequestAccepted,
						RequestID:  "req_s0_" + id,
						ProjectID:  "project_s0",
						PeriodID:   "2026-09-04",
						OccurredAt: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
					},
				}
				if err := state.Apply(record); err != nil {
					t.Fatalf("apply %d: %v", index, err)
				}
			}
			duration := time.Since(started)
			runtime.GC()
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			heapDelta := after.HeapAlloc - before.HeapAlloc
			t.Logf("records=%d apply=%s apply_records_s=%.2f heap_alloc_delta=%d bytes_per_event=%.2f",
				records, duration, float64(records)/duration.Seconds(), heapDelta, float64(heapDelta)/float64(records))
			runtime.KeepAlive(state)
		})
	}
}
