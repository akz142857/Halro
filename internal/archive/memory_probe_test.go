package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
)

// TestGovernanceMemoryProbe measures what finished Work Units and their
// Outcomes leave in memory (docs/todo/memory-bounded-redesign.zh-CN.md §13.7):
// the heap after GC once 10k and 100k of them — each with one Run, one
// settled request and one Outcome — have been applied and archived. It is a
// measurement, not a gate, so it runs only when asked:
//
//	HALRO_GOVERNANCE_PROBE=1 go test ./internal/archive -run TestGovernanceMemoryProbe -v -timeout 30m
func TestGovernanceMemoryProbe(t *testing.T) {
	if os.Getenv("HALRO_GOVERNANCE_PROBE") != "1" {
		t.Skip("set HALRO_GOVERNANCE_PROBE=1 to measure")
	}
	for _, count := range []int{10_000, 100_000} {
		heap, file := probeGovernance(t, count)
		t.Logf("work units=%d heap=%.1f MiB archive=%.1f MiB", count, float64(heap)/(1<<20), float64(file)/(1<<20))
	}
}

func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func probeGovernance(t *testing.T, count int) (uint64, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	store, _, err := Open(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accounting := ledger.NewState()
	accounting.SetArchive(store)
	outcomes := governance.NewState()
	outcomes.SetArchive(store, 0)

	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	sequence, governanceSequence := uint64(0), uint64(0)
	apply := func(event ledger.Event) {
		sequence++
		if err := accounting.Apply(ledger.Record{Generation: 1, Sequence: sequence, Offset: int64(sequence), Epoch: 5, Event: event}); err != nil {
			t.Fatalf("apply %d: %v", sequence, err)
		}
	}
	for index := 0; index < count; index++ {
		now := base.Add(time.Duration(index) * time.Millisecond)
		wu, run, req, att := id("wku", index), id("run", index), id("req", index), id("att", index)
		event := func(kind ledger.EventKind, operation string) ledger.Event {
			return ledger.Event{
				EventID: id("evt", int(sequence)+1), Kind: kind, ProjectID: "prj_01jabcdefghjkmnpqrstvwxyz0", KeyID: "key_01jabcdefghjkmnpqrstvwxyz0",
				PeriodID: "2026-09-04", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
				PeriodStartMicros: base.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: base.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(),
				OccurredAt: now, Operation: operation,
				IdempotencyKeyHash: fmt.Sprintf("sha256:%064x", sequence+1), RequestFingerprint: fmt.Sprintf("sha256:%064x", sequence+1),
			}
		}
		created := event(ledger.EventWorkUnitCreated, "work_unit.create")
		created.WorkUnitID = wu
		created.OutcomeDefinitions = []domain.OutcomeDefinitionRef{{ID: "odef_taskresult", Version: 1}}
		apply(created)
		runCreated := event(ledger.EventRunCreated, "run.create")
		runCreated.WorkUnitID, runCreated.RunID, runCreated.RunBudgetMicrosUSD, runCreated.RunExpiresAt = wu, run, 1_000, now.Add(time.Hour)
		apply(runCreated)
		accepted := event(ledger.EventRequestAccepted, "")
		accepted.RequestID, accepted.WorkUnitID, accepted.RunID = req, wu, run
		apply(accepted)
		reserved := event(ledger.EventReservationCreated, "")
		reserved.RequestID, reserved.AttemptID, reserved.WorkUnitID, reserved.RunID = req, att, wu, run
		reserved.ReservationMicrosUSD = ledger.MicrosUSD(100)
		apply(reserved)
		settled := event(ledger.EventAttemptSettled, "")
		settled.RequestID, settled.AttemptID, settled.WorkUnitID, settled.RunID = req, att, wu, run
		settled.CommittedMicrosUSD = ledger.MicrosUSD(75)
		apply(settled)
		finalized := event(ledger.EventRequestFinalized, "")
		finalized.RequestID, finalized.WorkUnitID, finalized.RunID = req, wu, run
		apply(finalized)
		runClosed := event(ledger.EventRunClosed, "run.close")
		runClosed.WorkUnitID, runClosed.RunID, runClosed.CloseReason = wu, run, "completed"
		apply(runClosed)
		wuClosed := event(ledger.EventWorkUnitClosed, "work_unit.close")
		wuClosed.WorkUnitID = wu
		apply(wuClosed)
		if accounting.ArchiveQueueLen() >= ledger.ArchiveQueueLimit {
			if err := accounting.FlushArchive(probeChain{sequence}); err != nil {
				t.Fatal(err)
			}
		}

		governanceSequence++
		outcome := governance.Record{Sequence: governanceSequence, Hash: [32]byte{byte(governanceSequence)}, Event: governance.Event{
			EventID: id("gov", index), ProjectID: "prj_01jabcdefghjkmnpqrstvwxyz0", WorkUnitID: wu, DefinitionID: "odef_taskresult", DefinitionVersion: 1,
			OutcomeID: id("out", index), Value: "accepted", ReporterKeyID: "key_01jabcdefghjkmnpqrstvwxyz0",
			EvidenceSHA256: fmt.Sprintf("%064x", index), EvidenceRef: "ticket-123456", ObservedAt: now, IngestedAt: now, Revision: 1,
			IdempotencyKeyHash: fmt.Sprintf("sha256:%064x", index), RequestFingerprint: fmt.Sprintf("sha256:%064x", index+1),
		}}
		if err := outcomes.Apply(outcome); err != nil {
			t.Fatalf("outcome %d: %v", index, err)
		}
		if outcomes.QueueLen() >= 1024 {
			if err := outcomes.FlushOutcomes(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := accounting.FlushArchive(probeChain{sequence}); err != nil {
		t.Fatal(err)
	}
	if err := outcomes.FlushOutcomes(); err != nil {
		t.Fatal(err)
	}
	heap := heapAfterGC()
	runtime.KeepAlive(accounting)
	runtime.KeepAlive(outcomes)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return heap, info.Size()
}

// id is a governance-shaped identifier of realistic length.
func id(prefix string, index int) string {
	value := strconv.Itoa(index)
	for len(value) < 26 {
		value = "0" + value
	}
	return prefix + "_" + value
}

type probeChain struct{ sequence uint64 }

func (c probeChain) ChainHead() (ledger.Watermark, [32]byte, bool) {
	return ledger.Watermark{Generation: 1, Sequence: c.sequence}, [32]byte{1}, true
}
