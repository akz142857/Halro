package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// memoryArchive is an Archive held in maps, for tests that cannot import the
// bbolt one.
type memoryArchive struct {
	workUnits   map[string]ArchivedWorkUnit
	runs        map[string]domain.Run
	idempotency map[string]GovernanceIdempotency
	through     uint64
	chain       StateSnapshotChain
	writes      int
}

func newMemoryArchive() *memoryArchive {
	return &memoryArchive{workUnits: map[string]ArchivedWorkUnit{}, runs: map[string]domain.Run{}, idempotency: map[string]GovernanceIdempotency{}}
}

func (a *memoryArchive) ArchivedWorkUnit(id string) (ArchivedWorkUnit, bool, error) {
	item, ok := a.workUnits[id]
	return item, ok, nil
}

func (a *memoryArchive) ArchivedRun(id string) (domain.Run, bool, error) {
	run, ok := a.runs[id]
	return run, ok, nil
}

func (a *memoryArchive) ArchivedIdempotency(projectID, operation, keyHash string) (GovernanceIdempotency, bool, error) {
	record, ok := a.idempotency[governanceIdempotencyMapKey(projectID, operation, keyHash)]
	return record, ok, nil
}

func (a *memoryArchive) VisitArchivedWorkUnits(query ArchiveQuery, visit func(ArchivedWorkUnit) (bool, error)) error {
	ids := make([]string, 0, len(a.workUnits))
	for id := range a.workUnits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := a.workUnits[id]
		if id <= query.After || query.ProjectID != "" && item.WorkUnit.ProjectID != query.ProjectID {
			continue
		}
		if query.CohortStart != "" && (item.WorkUnit.PeriodID < query.CohortStart || item.WorkUnit.PeriodID > query.CohortEnd) {
			continue
		}
		if more, err := visit(item); err != nil || !more {
			return err
		}
	}
	return nil
}

func (a *memoryArchive) VisitArchivedRuns(projectID, after string, visit func(domain.Run) (bool, error)) error {
	ids := make([]string, 0, len(a.runs))
	for id := range a.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		run := a.runs[id]
		if id <= after || projectID != "" && run.ProjectID != projectID {
			continue
		}
		if more, err := visit(run); err != nil || !more {
			return err
		}
	}
	return nil
}

func (a *memoryArchive) WriteArchived(items []ArchivedWorkUnit, through uint64, chain StateSnapshotChain) error {
	a.chain = chain
	a.writes++
	for _, item := range items {
		a.workUnits[item.WorkUnit.ID] = item
		for _, run := range item.Runs {
			a.runs[run.ID] = run
		}
		for _, record := range item.Idempotency {
			a.idempotency[governanceIdempotencyMapKey(record.ProjectID, record.Operation, record.KeyHash)] = record
		}
	}
	a.through = max(a.through, through)
	return nil
}

func (a *memoryArchive) ArchivedThrough() (uint64, StateSnapshotChain, error) {
	return a.through, a.chain, nil
}

func (a *memoryArchive) ResetArchived() error {
	*a = *newMemoryArchive()
	return nil
}

func (a *memoryArchive) encoded(t *testing.T) []byte {
	t.Helper()
	ids := make([]string, 0, len(a.workUnits))
	for id := range a.workUnits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var items []ArchivedWorkUnit
	for _, id := range ids {
		items = append(items, a.workUnits[id])
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// governedTask is the events of one Work Unit with one Run and one request,
// closed in the given order. Each event gets its own idempotency key.
func governedTask(index int, now time.Time, closeBeforeSettle bool) []Event {
	wu, run, req, att := fmt.Sprintf("wku_%d", index), fmt.Sprintf("run_%d", index), fmt.Sprintf("req_%d", index), fmt.Sprintf("att_%d", index)
	event := func(name string, kind EventKind) Event {
		e := governanceEvent(fmt.Sprintf("evt_%s_%d", name, index), kind, now)
		e.Operation = name
		e.IdempotencyKeyHash = fmt.Sprintf("sha256:%064x", index*16+int(kind))
		return e
	}
	created := event("wu.create", EventWorkUnitCreated)
	created.WorkUnitID = wu
	runCreated := event("run.create", EventRunCreated)
	runCreated.WorkUnitID, runCreated.RunID = wu, run
	runCreated.RunBudgetMicrosUSD, runCreated.RunExpiresAt = 1_000, now.Add(time.Hour)
	accepted := event("accepted", EventRequestAccepted)
	accepted.RequestID, accepted.WorkUnitID, accepted.RunID = req, wu, run
	reserved := event("reserved", EventReservationCreated)
	reserved.RequestID, reserved.AttemptID, reserved.WorkUnitID, reserved.RunID = req, att, wu, run
	reserved.ReservationMicrosUSD = MicrosUSD(100)
	settled := event("settled", EventAttemptSettled)
	settled.RequestID, settled.AttemptID, settled.WorkUnitID, settled.RunID = req, att, wu, run
	settled.CommittedMicrosUSD = MicrosUSD(int64(10 + index))
	finalized := event("finalized", EventRequestFinalized)
	finalized.RequestID, finalized.WorkUnitID, finalized.RunID = req, wu, run
	runClosed := event("run.close", EventRunClosed)
	runClosed.WorkUnitID, runClosed.RunID, runClosed.CloseReason = wu, run, "completed"
	wuClosed := event("wu.close", EventWorkUnitClosed)
	wuClosed.WorkUnitID = wu
	if closeBeforeSettle {
		return []Event{created, runCreated, accepted, reserved, runClosed, wuClosed, settled, finalized}
	}
	return []Event{created, runCreated, accepted, reserved, settled, finalized, runClosed, wuClosed}
}

func applyEvents(t *testing.T, state *State, events []Event, first uint64) uint64 {
	t.Helper()
	sequence := first
	for _, event := range events {
		if err := state.Apply(Record{Generation: 1, Sequence: sequence, Offset: int64(sequence * 100), Epoch: frameVersionRunAttribution, Event: event}); err != nil {
			t.Fatalf("apply %d (%v): %v", sequence, event.Kind, err)
		}
		sequence++
	}
	return sequence
}

// A closed Work Unit with nothing in flight leaves the state with its Runs,
// cost and idempotency records, and is found in the queue and then the
// Archive exactly as it was.
func TestArchivedWorkUnitLeavesTheStateAndStaysFindable(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	archive := newMemoryArchive()
	state := NewState()
	state.SetArchive(archive)
	applyEvents(t, state, governedTask(1, now, false), 1)

	if len(state.workUnits) != 0 || len(state.runs) != 0 || len(state.workUnitCosts) != 0 || len(state.governanceIdempotency) != 0 || len(state.runsByWorkUnit) != 0 {
		t.Fatalf("the finished Work Unit is still held: wu=%d runs=%d costs=%d idem=%d", len(state.workUnits), len(state.runs), len(state.workUnitCosts), len(state.governanceIdempotency))
	}
	check := func(where string) {
		t.Helper()
		workUnit, found, err := state.WorkUnit("wku_1")
		if err != nil || !found || workUnit.Status != domain.WorkUnitClosed {
			t.Fatalf("%s: work unit %#v found=%t err=%v", where, workUnit, found, err)
		}
		run, found, err := state.Run("run_1")
		if err != nil || !found || run.Status != domain.RunClosed || run.CommittedMicrosUSD != 11 || run.ReservedMicrosUSD != 0 {
			t.Fatalf("%s: run %#v found=%t err=%v", where, run, found, err)
		}
		record, found, err := state.GovernanceIdempotency("prj_1", "run.close", fmt.Sprintf("sha256:%064x", 16+int(EventRunClosed)))
		if err != nil || !found || record.RunID != "run_1" {
			t.Fatalf("%s: idempotency %#v found=%t err=%v", where, record, found, err)
		}
		view, found, err := state.WorkUnitView("wku_1")
		if err != nil || !found || view.Cost.KnownMicrosUSD != 11 || len(view.Runs) != 1 {
			t.Fatalf("%s: view %#v found=%t err=%v", where, view, found, err)
		}
	}
	if state.ArchiveQueueLen() != 1 {
		t.Fatalf("queue=%d", state.ArchiveQueueLen())
	}
	check("queued")
	if err := state.FlushArchive(fixedChainHead{sequence: state.Watermark().Sequence}); err != nil {
		t.Fatal(err)
	}
	if state.ArchiveQueueLen() != 0 || archive.through != state.Watermark().Sequence {
		t.Fatalf("queue=%d through=%d", state.ArchiveQueueLen(), archive.through)
	}
	check("archived")
}

// A Work Unit closed while an attempt is in flight stays until the attempt
// settles and its request finishes, and the late settlement is in the cost
// it is archived with.
func TestInFlightAttemptsKeepAClosedWorkUnit(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	state := NewState()
	state.SetArchive(newMemoryArchive())
	events := governedTask(1, now, true)
	next := applyEvents(t, state, events[:6], 1)
	if _, held := state.workUnits["wku_1"]; !held {
		t.Fatal("a Work Unit with an attempt in flight was archived")
	}
	next = applyEvents(t, state, events[6:7], next)
	if _, held := state.workUnits["wku_1"]; !held {
		t.Fatal("a Work Unit with a request in flight was archived")
	}
	applyEvents(t, state, events[7:], next)
	view, found, err := state.WorkUnitView("wku_1")
	if err != nil || !found || state.ArchiveQueueLen() != 1 || view.Cost.KnownMicrosUSD != 11 || view.Runs[0].CommittedMicrosUSD != 11 {
		t.Fatalf("archived view %#v found=%t err=%v queue=%d", view, found, err, state.ArchiveQueueLen())
	}
}

// A Run left open keeps its closed Work Unit until a later record's clock
// passes the Run's expiry.
func TestAnExpiringRunKeepsAClosedWorkUnitUntilItExpires(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	state := NewState()
	state.SetArchive(newMemoryArchive())
	events := governedTask(1, now, false)
	// Drop the Run's close: the Work Unit closes with its Run still active.
	open := append(append([]Event{}, events[:6]...), events[7])
	next := applyEvents(t, state, open, 1)
	if _, held := state.workUnits["wku_1"]; !held {
		t.Fatal("a Work Unit with a live Run was archived")
	}
	later := governanceEvent("evt_other", EventRequestAccepted, now.Add(59*time.Minute))
	later.RequestID = "req_other"
	next = applyEvents(t, state, []Event{later}, next)
	if _, held := state.workUnits["wku_1"]; !held {
		t.Fatal("archived before the Run expired")
	}
	expired := governanceEvent("evt_after", EventRequestFinalized, now.Add(time.Hour))
	expired.RequestID = "req_other"
	applyEvents(t, state, []Event{expired}, next)
	run, found, err := state.Run("run_1")
	if _, held := state.workUnits["wku_1"]; held || err != nil || !found || run.Status != domain.RunActive {
		t.Fatalf("held=%t run=%#v found=%t err=%v", held, run, found, err)
	}
}

// Archiving depends only on the records, so a state restored from a snapshot
// taken anywhere and replayed on archives the same Work Units at the same
// records, and ends in the same state, as replaying everything.
func TestArchivingIsTheSameFromAnySnapshot(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	var events []Event
	for index := 0; index < 4; index++ {
		events = append(events, governedTask(index, now.Add(time.Duration(index)*time.Minute), index%2 == 0)...)
	}
	full := NewState()
	fullArchive := newMemoryArchive()
	full.SetArchive(fullArchive)
	applyEvents(t, full, events, 1)
	if err := full.FlushArchive(fixedChainHead{sequence: full.Watermark().Sequence}); err != nil {
		t.Fatal(err)
	}
	wantState, wantArchive := encodedState(t, full), fullArchive.encoded(t)
	if len(fullArchive.workUnits) != 4 {
		t.Fatalf("archived %d of 4", len(fullArchive.workUnits))
	}
	for cut := 0; cut <= len(events); cut++ {
		archive := newMemoryArchive()
		prefix := NewState()
		prefix.SetArchive(archive)
		applyEvents(t, prefix, events[:cut], 1)
		path := t.TempDir() + "/state.snap"
		key := bytes.Repeat([]byte{3}, 32)
		if _, _, err := WriteStateSnapshot(path, key, prefix, fixedChainHead{sequence: uint64(cut)}); err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		restored, _, err := ReadStateSnapshot(path, key)
		if err != nil {
			t.Fatal(err)
		}
		restored.SetArchive(archive)
		applyEvents(t, restored, events[cut:], uint64(cut)+1)
		if err := restored.FlushArchive(fixedChainHead{sequence: restored.Watermark().Sequence}); err != nil {
			t.Fatal(err)
		}
		if got := encodedState(t, restored); !bytes.Equal(got, wantState) {
			t.Fatalf("cut %d: state differs\n got %s\nwant %s", cut, got, wantState)
		}
		if got := archive.encoded(t); !bytes.Equal(got, wantArchive) {
			t.Fatalf("cut %d: archive differs\n got %s\nwant %s", cut, got, wantArchive)
		}
	}
}

// A listing merges what the state holds with what was archived, in ID order,
// each Work Unit once, and resumes after a cursor.
func TestVisitWorkUnitsMergesHeldAndArchivedInOrder(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	state := NewState()
	state.SetArchive(newMemoryArchive())
	next := uint64(1)
	for index := 1; index <= 5; index++ {
		events := governedTask(index, now, false)
		if index%2 == 0 {
			events = events[:2] // left open
		}
		next = applyEvents(t, state, events, next)
		if index == 3 {
			if err := state.FlushArchive(fixedChainHead{sequence: state.Watermark().Sequence}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var got []string
	err := state.VisitWorkUnits(ArchiveQuery{ProjectID: "prj_1", After: "wku_1"}, func(view WorkUnitView) (bool, error) {
		got = append(got, view.WorkUnit.ID+":"+string(view.WorkUnit.Status))
		return len(got) < 3, nil
	})
	if err != nil || strings.Join(got, ",") != "wku_2:open,wku_3:closed,wku_4:open" {
		t.Fatalf("visited %v err=%v", got, err)
	}
	var runs []string
	err = state.VisitRuns("prj_1", "", func(run domain.Run) (bool, error) {
		runs = append(runs, run.ID)
		return true, nil
	})
	if err != nil || strings.Join(runs, ",") != "run_1,run_2,run_3,run_4,run_5" {
		t.Fatalf("runs %v err=%v", runs, err)
	}
	if state.OpenWorkUnitCount("prj_1") != 2 || len(state.LiveRuns("prj_1")) != 2 {
		t.Fatalf("open=%d live runs=%d", state.OpenWorkUnitCount("prj_1"), len(state.LiveRuns("prj_1")))
	}
}
