package governance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// memoryArchive is an Archive held in maps.
type memoryArchive struct {
	outcomes    map[string]SnapshotRecord
	heads       map[string]string
	idempotency map[string]SnapshotRecord
	through     uint64
	hash        [32]byte
}

func newMemoryArchive() *memoryArchive {
	return &memoryArchive{outcomes: map[string]SnapshotRecord{}, heads: map[string]string{}, idempotency: map[string]SnapshotRecord{}}
}

func (a *memoryArchive) WriteOutcomes(items []SnapshotRecord, through uint64, hash [32]byte) error {
	for _, item := range items {
		outcome := item.Outcome
		a.outcomes[outcome.ID] = item
		a.heads[headKey(outcome.ProjectID, outcome.WorkUnitID, outcome.DefinitionID, outcome.DefinitionVersion)] = outcome.ID
		a.idempotency[outcome.ProjectID+"\x00"+item.IdempotencyKeyHash] = item
	}
	if through >= a.through {
		a.through, a.hash = through, hash
	}
	return nil
}

func (a *memoryArchive) OutcomesThrough() (uint64, [32]byte, error) { return a.through, a.hash, nil }
func (a *memoryArchive) ResetOutcomes() error {
	*a = *newMemoryArchive()
	return nil
}

func (a *memoryArchive) ArchivedOutcome(id string) (domain.Outcome, bool, error) {
	item, ok := a.outcomes[id]
	return item.Outcome, ok, nil
}

func (a *memoryArchive) ArchivedHead(projectID, workUnitID, definitionID string, version uint64) (domain.Outcome, bool, error) {
	id, ok := a.heads[headKey(projectID, workUnitID, definitionID, version)]
	if !ok {
		return domain.Outcome{}, false, nil
	}
	return a.outcomes[id].Outcome, true, nil
}

func (a *memoryArchive) ArchivedOutcomeIdempotency(projectID, keyHash string) (string, string, bool, error) {
	item, ok := a.idempotency[projectID+"\x00"+keyHash]
	return item.RequestFingerprint, item.Outcome.ID, ok, nil
}

func (a *memoryArchive) VisitArchivedOutcomes(query OutcomeQuery, visit func(domain.Outcome) (bool, error)) error {
	ids := make([]string, 0, len(a.outcomes))
	for id := range a.outcomes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		outcome := a.outcomes[id].Outcome
		if id <= query.After || query.ProjectID != "" && outcome.ProjectID != query.ProjectID ||
			query.WorkUnitID != "" && outcome.WorkUnitID != query.WorkUnitID || query.DefinitionID != "" && outcome.DefinitionID != query.DefinitionID {
			continue
		}
		if more, err := visit(outcome); err != nil || !more {
			return err
		}
	}
	return nil
}

func outcomeRecord(sequence uint64, workUnit, outcomeID, supersedes string, revision uint64) Record {
	at := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	return Record{Sequence: sequence, Hash: [32]byte{byte(sequence), byte(sequence >> 8)}, Event: Event{
		EventID: "gov_" + strconv.FormatUint(sequence, 10), ProjectID: "prj_1", WorkUnitID: workUnit, DefinitionID: "odef_result", DefinitionVersion: 1,
		OutcomeID: outcomeID, Value: "accepted", ReporterKeyID: "key_1", ObservedAt: at, IngestedAt: at,
		SupersedesOutcomeID: supersedes, Revision: revision,
		IdempotencyKeyHash: shaLabel("idem_" + outcomeID), RequestFingerprint: shaLabel("fp_" + outcomeID),
	}}
}

// The state holds a bounded ring of heads and idempotency records however
// many Outcomes are reported; what left the ring is answered by the archive,
// and a revision of an evicted head is still checked against it.
func TestOutcomeStateStaysBoundedAndAnswersFromTheArchive(t *testing.T) {
	archive := newMemoryArchive()
	state := NewState()
	state.SetArchive(archive, 0)
	total := recentOutcomes + 500
	for index := 1; index <= total; index++ {
		if err := state.Apply(outcomeRecord(uint64(index), fmt.Sprintf("wku_%d", index), fmt.Sprintf("out_%05d", index), "", 1)); err != nil {
			t.Fatalf("apply %d: %v", index, err)
		}
		if state.QueueLen() >= outcomeFlushThreshold {
			if err := state.FlushOutcomes(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := state.FlushOutcomes(); err != nil {
		t.Fatal(err)
	}
	if len(state.heads) > recentOutcomes || len(state.idempotency) > recentOutcomes || state.QueueLen() != 0 || len(state.headOrder) > 2*recentOutcomes {
		t.Fatalf("heads=%d idem=%d queue=%d order=%d", len(state.heads), len(state.idempotency), state.QueueLen(), len(state.headOrder))
	}
	if archive.through != uint64(total) || archive.hash != [32]byte{byte(total), byte(total >> 8)} {
		t.Fatalf("archive through=%d hash=%x", archive.through, archive.hash[:2])
	}
	// out_00001 left both rings long ago.
	if record, found, err := state.Idempotency("prj_1", shaLabel("idem_out_00001")); err != nil || !found || record.OutcomeID != "out_00001" {
		t.Fatalf("evicted idempotency %#v found=%t err=%v", record, found, err)
	}
	head, found, err := state.Current("prj_1", "wku_1", "odef_result", 1)
	if err != nil || !found || head.ID != "out_00001" {
		t.Fatalf("evicted head %#v found=%t err=%v", head, found, err)
	}
	// A second revision of it is taken on trust by Apply: the head left the
	// ring, and the Manager checked it against the archive's head.
	if err := state.Apply(outcomeRecord(uint64(total+1), "wku_1", "out_revised", "out_00001", 2)); err != nil {
		t.Fatalf("revision of an evicted head: %v", err)
	}
	if head, found, err := state.Current("prj_1", "wku_1", "odef_result", 1); err != nil || !found || head.ID != "out_revised" {
		t.Fatalf("revised head %#v found=%t err=%v", head, found, err)
	}
	// A head still in the ring is checked as before.
	last := fmt.Sprintf("out_%05d", total)
	if err := state.Apply(outcomeRecord(uint64(total+2), fmt.Sprintf("wku_%d", total), "out_wrong", "out_other", 2)); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("conflicting revision of %s err=%v", last, err)
	}
	var visited int
	// After out_04500: out_04501..out_04596, then out_revised.
	if err := state.VisitOutcomes(OutcomeQuery{ProjectID: "prj_1", After: "out_04500"}, func(domain.Outcome) (bool, error) {
		visited++
		return true, nil
	}); err != nil || visited != total-4500+1 {
		t.Fatalf("visited %d of %d err=%v", visited, total-4500+1, err)
	}
}

// Through a Manager, an Outcome reported long ago is replayed idempotently
// and superseded correctly after its head and idempotency record left the
// rings.
func TestManagerReportsAgainstArchivedHeads(t *testing.T) {
	manager, log, _, _, _ := testManager(t)
	defer log.Close()
	archive := newMemoryArchive()
	manager.state.SetArchive(archive, 0)
	first, _, err := manager.Report(context.Background(), reportInput("accepted", "", "first"))
	if err != nil {
		t.Fatal(err)
	}
	if manager.state.QueueLen() != 0 || len(archive.outcomes) != 1 {
		t.Fatalf("report was not written through: queue=%d archived=%d", manager.state.QueueLen(), len(archive.outcomes))
	}
	// Push the head and the idempotency record out of the rings.
	manager.state.mu.Lock()
	manager.state.heads, manager.state.idempotency = map[string]headEntry{}, map[string]idemEntry{}
	manager.state.mu.Unlock()
	replayed, replay, err := manager.Report(context.Background(), reportInput("accepted", "", "first"))
	if err != nil || !replay || replayed.ID != first.ID {
		t.Fatalf("archived idempotent replay %#v replay=%t err=%v", replayed, replay, err)
	}
	if _, _, err := manager.Report(context.Background(), reportInput("rejected", "", "fresh-start")); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("a first revision over an archived head err=%v", err)
	}
	second, _, err := manager.Report(context.Background(), reportInput("rejected", first.ID, "second"))
	if err != nil || second.Revision != 2 || second.SupersedesOutcomeID != first.ID {
		t.Fatalf("revision over an archived head %#v err=%v", second, err)
	}
}
