package ledger

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestHotStateCostProbe measures what one entry of each kind the hot state
// is bounded by costs in heap and in the snapshot
// (docs/todo/memory-bounded-redesign.zh-CN.md §6, §12.2): an in-flight
// request with its reservation, an open Work Unit, an active Run, and a
// (project, day) balance. It is a measurement, not a gate:
//
//	HALRO_HOT_STATE_PROBE=1 go test ./internal/ledger -run TestHotStateCostProbe -v
func TestHotStateCostProbe(t *testing.T) {
	if os.Getenv("HALRO_HOT_STATE_PROBE") != "1" {
		t.Skip("set HALRO_HOT_STATE_PROBE=1 to measure")
	}
	const count = 20_000
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ident := func(prefix string, index int) string { return fmt.Sprintf("%s_%026d", prefix, index) }
	project := ident("prj", 0)
	event := func(kind EventKind, at time.Time, index int, name string) Event {
		e := governanceEvent(ident("evt"+name, index), kind, at)
		e.ProjectID, e.KeyID = project, ident("key", 0)
		e.Operation = name
		e.IdempotencyKeyHash = fmt.Sprintf("sha256:%064x", index)
		return e
	}
	kinds := []struct {
		name   string
		events func(index int) []Event
	}{
		{"in-flight request + reservation", func(index int) []Event {
			accepted := event(EventRequestAccepted, base, index, "acc")
			accepted.RequestID = ident("req", index)
			reserved := event(EventReservationCreated, base, index, "res")
			reserved.RequestID, reserved.AttemptID, reserved.ReservationMicrosUSD = accepted.RequestID, ident("att", index), MicrosUSD(100)
			return []Event{accepted, reserved}
		}},
		{"open work unit", func(index int) []Event {
			created := event(EventWorkUnitCreated, base, index, "wu.create")
			created.WorkUnitID = ident("wku", index)
			return []Event{created}
		}},
		{"open work unit + 1 active run", func(index int) []Event {
			created := event(EventWorkUnitCreated, base, index, "wu.create")
			created.WorkUnitID = ident("wku", index)
			run := event(EventRunCreated, base, index, "run.create")
			run.IdempotencyKeyHash = fmt.Sprintf("sha256:%064x", 1<<40+index)
			run.WorkUnitID, run.RunID, run.RunBudgetMicrosUSD, run.RunExpiresAt = created.WorkUnitID, ident("run", index), 1_000, base.Add(time.Hour)
			return []Event{created, run}
		}},
		{"balance (project, day)", func(index int) []Event {
			at := base.Add(time.Duration(index%365) * 24 * time.Hour)
			e := func(kind EventKind, name string) Event {
				ev := event(kind, at, index, name)
				ev.ProjectID = ident("prj", index/365)
				ev.PeriodID = at.Format("2006-01-02")
				ev.RequestID, ev.AttemptID = ident("req", index), ident("att", index)
				return ev
			}
			reserved := e(EventReservationCreated, "res")
			reserved.ReservationMicrosUSD = MicrosUSD(100)
			settled := e(EventAttemptSettled, "set")
			settled.CommittedMicrosUSD = MicrosUSD(75)
			accepted := e(EventRequestAccepted, "acc")
			finalized := e(EventRequestFinalized, "fin")
			return []Event{accepted, reserved, settled, finalized}
		}},
	}
	key := bytes.Repeat([]byte{7}, 32)
	for _, kind := range kinds {
		state := NewState()
		state.SetArchive(newMemoryArchive())
		sequence := uint64(1)
		// Fill the event digest window first, so it is not counted.
		for index := 0; index < 70_000; index++ {
			warm := event(EventRequestAccepted, base, index, "warm")
			warm.ProjectID, warm.RequestID = ident("prw", 0), ident("rqw", index)
			done := event(EventRequestFinalized, base, index, "warmfin")
			done.ProjectID, done.RequestID = warm.ProjectID, warm.RequestID
			sequence = applyEvents(t, state, []Event{warm, done}, sequence)
		}
		before := heapAfterGC()
		snapBefore := snapshotSize(t, state, key, sequence-1)
		for index := 0; index < count; index++ {
			sequence = applyEvents(t, state, kind.events(index), sequence)
		}
		after := heapAfterGC()
		snapAfter := snapshotSize(t, state, key, sequence-1)
		runtime.KeepAlive(state)
		perHeap := float64(after-before) / count
		perSnap := float64(snapAfter-snapBefore) / count
		t.Logf("%-34s heap %6.0f B/entry  snapshot %5.0f B/entry  → 128 MiB holds %7.0f", kind.name, perHeap, perSnap, 128*(1<<20)/perHeap)
	}
}

func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func snapshotSize(t *testing.T, state *State, key []byte, sequence uint64) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.snap")
	if _, _, err := WriteStateSnapshot(path, key, state, fixedChainHead{sequence: sequence}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Size())
}
