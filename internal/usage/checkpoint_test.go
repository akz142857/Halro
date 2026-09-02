package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// checkpointStore stands in for the metadata store: it keeps the head and the
// segments a series of checkpoint rounds wrote, applying each round's removals,
// so a test restores from exactly what the runtime would have on disk.
type checkpointStore struct {
	head     []byte
	segments map[uint64][]byte
}

func newCheckpointStore() *checkpointStore {
	return &checkpointStore{segments: map[uint64][]byte{}}
}

// round takes a checkpoint, stores it and commits it, the way the maintenance
// tick does. It returns how many bytes the round actually wrote — the number
// this whole change exists to keep small.
func (s *checkpointStore) round(t *testing.T, aggregate *Aggregate) int {
	t.Helper()
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	written := len(snapshot.Head)
	for _, segment := range snapshot.Segments {
		s.segments[segment.ID] = segment.Payload
		written += len(segment.Payload)
	}
	for _, id := range snapshot.RemovedSegments {
		delete(s.segments, id)
	}
	s.head = snapshot.Head
	aggregate.CommitCheckpoint(snapshot)
	return written
}

func (s *checkpointStore) read(id uint64) ([]byte, error) {
	payload, ok := s.segments[id]
	if !ok {
		return nil, fmt.Errorf("segment %d is missing", id)
	}
	return payload, nil
}

func (s *checkpointStore) restore(t *testing.T) *Aggregate {
	t.Helper()
	restored, err := RestoreCheckpoint(s.head, s.read)
	if err != nil {
		t.Fatal(err)
	}
	return restored
}

// bytes is what the whole checkpoint occupies, head and every segment — the
// figure the operator guide's sizing table is derived from.
func (s *checkpointStore) bytes() int {
	total := len(s.head)
	for _, payload := range s.segments {
		total += len(payload)
	}
	return total
}

// restoreOneRound rebuilds from a single round, for tests whose subject is the
// aggregate's content rather than the checkpoint's shape.
func restoreOneRound(aggregate *Aggregate) (*Aggregate, error) {
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		return nil, err
	}
	segments := map[uint64][]byte{}
	for _, segment := range snapshot.Segments {
		segments[segment.ID] = segment.Payload
	}
	aggregate.CommitCheckpoint(snapshot)
	return RestoreCheckpoint(snapshot.Head, func(id uint64) ([]byte, error) {
		payload, ok := segments[id]
		if !ok {
			return nil, fmt.Errorf("segment %d is missing", id)
		}
		return payload, nil
	})
}

// TestCheckpointWritesOnlyTheRecordsSinceTheLastOne is the whole point of the
// segmented format: a tick's cost follows what arrived, not what is resident.
// Before it, a checkpoint re-encoded every record in the window every minute.
func TestCheckpointWritesOnlyTheRecordsSinceTheLastOne(t *testing.T) {
	aggregate := NewAggregate()
	applyRecords(t, aggregate, checkpointKillPointRecords(40000))
	store := newCheckpointStore()
	first := store.round(t, aggregate)
	resident := store.bytes()
	if resident < 8*checkpointSegmentTargetBytes {
		t.Fatalf("fixture holds %d bytes, too little for the bound below to mean anything", resident)
	}

	// One more request, then another tick.
	applyRecords(t, aggregate, checkpointRecordsFrom(aggregate, 1))
	second := store.round(t, aggregate)

	// A round rewrites the head and the open tail, and the tail is bounded by
	// the segment target. That bound is the property: it does not move when the
	// window gets longer, which is exactly what the old whole-blob format could
	// not say.
	bound := len(store.head) + checkpointSegmentTargetBytes
	if second > bound {
		t.Fatalf("second round wrote %d bytes, past the head-plus-one-segment bound of %d", second, bound)
	}
	if second >= resident/4 {
		t.Fatalf("second round wrote %d bytes of a %d byte window: the tick is still O(window)", second, resident)
	}
	t.Logf("first round %d bytes, second round %d bytes, window %d bytes", first, second, resident)
	// And what it wrote is still enough to restore the whole window.
	restored := store.restore(t)
	assertSameWindow(t, aggregate, restored)
}

// TestCheckpointSealsSegmentsAndReopensOnlyTheLast pins the shape the cost
// argument rests on: everything but the tail is immutable, so a tick can never
// be asked to rewrite it.
func TestCheckpointSealsSegmentsAndReopensOnlyTheLast(t *testing.T) {
	aggregate := NewAggregate()
	applyRecords(t, aggregate, checkpointKillPointRecords(12000))
	store := newCheckpointStore()
	store.round(t, aggregate)

	state := aggregate.Checkpointed()
	if state.Segments < 2 {
		t.Fatalf("expected the window to be split across segments, got %d", state.Segments)
	}
	head := decodeCheckpointHead(t, store.head)
	for index, ref := range head.Segments {
		if index < len(head.Segments)-1 && !ref.Sealed {
			t.Fatalf("segment %d is open before the last", ref.ID)
		}
		if ref.Attempts == 0 && ref.Summaries == 0 {
			t.Fatalf("segment %d holds nothing", ref.ID)
		}
		if index > 0 && ref.FirstSequence <= head.Segments[index-1].LastSequence {
			t.Fatalf("segment %d overlaps its predecessor", ref.ID)
		}
	}

	// A second round touches the open tail and nothing else.
	before := append([]CheckpointSegmentRef(nil), head.Segments...)
	applyRecords(t, aggregate, checkpointRecordsFrom(aggregate, 1))
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Segments) != 1 {
		t.Fatalf("expected one segment to be rewritten, got %d", len(snapshot.Segments))
	}
	if snapshot.Segments[0].ID != before[len(before)-1].ID {
		t.Fatalf("rewrote segment %d rather than the tail %d",
			snapshot.Segments[0].ID, before[len(before)-1].ID)
	}
}

// TestCheckpointDeletesSegmentsTheWindowHasTrimmed closes the loop between the
// window and the checkpoint: trimming has to reach disk, or the segments would
// accumulate for the life of the instance exactly as the old blob did.
func TestCheckpointDeletesSegmentsTheWindowHasTrimmed(t *testing.T) {
	day := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	aggregate := NewAggregate()
	applyRecords(t, aggregate, checkpointKillPointRecords(12000))
	store := newCheckpointStore()
	store.round(t, aggregate)
	full := len(store.segments)
	if full < 3 {
		t.Fatalf("fixture holds %d segments, too few to prove a deletion", full)
	}

	// Trim most of the window: everything already exported and older than the
	// cutoff goes.
	result := aggregate.PruneBefore(day.Add(3*time.Hour), aggregate.Snapshot().Watermark.Sequence)
	if result.Attempts == 0 {
		t.Fatal("nothing was trimmed, so this test proves nothing")
	}
	store.round(t, aggregate)
	if len(store.segments) >= full {
		t.Fatalf("segments did not shrink with the window: %d then %d", full, len(store.segments))
	}
	restored := store.restore(t)
	if restored.Floor() != result.Floor {
		t.Fatalf("floor %d restored as %d", result.Floor, restored.Floor())
	}
	assertSameWindow(t, aggregate, restored)
}

// TestCheckpointRoundIsProposedAgainAfterAFailedWrite: the aggregate only
// believes a round happened once the store took it, so a failure costs the next
// tick nothing but the same work.
func TestCheckpointRoundIsProposedAgainAfterAFailedWrite(t *testing.T) {
	aggregate := NewAggregate()
	applyRecords(t, aggregate, checkpointKillPointRecords(50))
	failed, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	// The store rejected it, so the increment goes back and nothing is
	// committed.
	if err := aggregate.ReturnCheckpoint(failed); err != nil {
		t.Fatal(err)
	}

	store := newCheckpointStore()
	store.round(t, aggregate)
	restored := store.restore(t)
	assertSameWindow(t, aggregate, restored)
	if restored.PendingRollupRows() != 0 {
		t.Fatalf("restored aggregate carries %d pending rollup rows", restored.PendingRollupRows())
	}
	if got := len(failed.Rollup); got == 0 {
		t.Fatal("fixture produced no rollup increment, so the return path is untested")
	}
}

// TestRestoreRefusesACheckpointItCannotFullyRead — every one of these leaves a
// console that quietly disagrees with the Ledger, so each is a rebuild rather
// than a partial restore.
func TestRestoreRefusesACheckpointItCannotFullyRead(t *testing.T) {
	aggregate := NewAggregate()
	applyRecords(t, aggregate, checkpointKillPointRecords(200))
	store := newCheckpointStore()
	store.round(t, aggregate)
	for _, testCase := range []struct {
		name    string
		head    []byte
		read    func(uint64) ([]byte, error)
		wantErr string
	}{
		{
			name: "missing segment", head: store.head,
			read:    func(uint64) ([]byte, error) { return nil, errors.New("gone") },
			wantErr: "read usage checkpoint segment",
		},
		{
			name: "segment is not readable", head: store.head,
			read:    func(uint64) ([]byte, error) { return []byte("{"), nil },
			wantErr: "decode usage checkpoint segment",
		},
		{
			name: "segment holds another segment", head: store.head,
			read: func(id uint64) ([]byte, error) {
				payload, err := store.read(id)
				if err != nil {
					return nil, err
				}
				var segment checkpointSegmentPayload
				if err := json.Unmarshal(payload, &segment); err != nil {
					return nil, err
				}
				segment.ID++
				return json.Marshal(segment)
			},
			wantErr: "holds segment",
		},
		{
			name: "segment disagrees with the head", head: store.head,
			read: func(id uint64) ([]byte, error) {
				payload, err := store.read(id)
				if err != nil {
					return nil, err
				}
				var segment checkpointSegmentPayload
				if err := json.Unmarshal(payload, &segment); err != nil {
					return nil, err
				}
				segment.Attempts = segment.Attempts[1:]
				return json.Marshal(segment)
			},
			wantErr: "does not hold what the head claims",
		},
		{
			name: "records out of ledger order", head: store.head,
			read: func(id uint64) ([]byte, error) {
				payload, err := store.read(id)
				if err != nil {
					return nil, err
				}
				var segment checkpointSegmentPayload
				if err := json.Unmarshal(payload, &segment); err != nil {
					return nil, err
				}
				if len(segment.Attempts) > 1 {
					segment.Attempts[0], segment.Attempts[1] = segment.Attempts[1], segment.Attempts[0]
				}
				return json.Marshal(segment)
			},
			wantErr: "out of ledger order",
		},
		{
			name: "the format it replaced", head: previousFormatHead(t, store.head),
			read: store.read, wantErr: "version 11 is not supported",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := RestoreCheckpoint(testCase.head, testCase.read); err == nil {
				t.Fatal("expected the checkpoint to be refused")
			} else if !contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error %q does not mention %q", err, testCase.wantErr)
			}
		})
	}
}

// TestCheckpointSurvivesManyRoundsWithTrimming walks the aggregate the way an
// instance does — apply, checkpoint, apply, trim, checkpoint — and restores at
// the end. Any drift between what a round writes and what the window holds
// shows up here rather than in production.
func TestCheckpointSurvivesManyRoundsWithTrimming(t *testing.T) {
	day := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	aggregate := NewAggregate()
	store := newCheckpointStore()
	for round := 0; round < 12; round++ {
		applyRecords(t, aggregate, checkpointRecordsFrom(aggregate, 300))
		store.round(t, aggregate)
		if round == 6 {
			aggregate.PruneBefore(day.Add(30*time.Minute), aggregate.Snapshot().Watermark.Sequence)
			store.round(t, aggregate)
		}
		restored := store.restore(t)
		assertSameWindow(t, aggregate, restored)
	}
}

func assertSameWindow(t *testing.T, want, got *Aggregate) {
	t.Helper()
	wantSnapshot, gotSnapshot := want.Snapshot(), got.Snapshot()
	if wantSnapshot.Watermark != gotSnapshot.Watermark {
		t.Fatalf("watermark %+v restored as %+v", wantSnapshot.Watermark, gotSnapshot.Watermark)
	}
	if wantSnapshot.Floor != gotSnapshot.Floor {
		t.Fatalf("floor %d restored as %d", wantSnapshot.Floor, gotSnapshot.Floor)
	}
	if wantSnapshot.Totals != gotSnapshot.Totals {
		t.Fatalf("totals %+v restored as %+v", wantSnapshot.Totals, gotSnapshot.Totals)
	}
	if want.Metrics() != got.Metrics() {
		t.Fatalf("metrics %+v restored as %+v", want.Metrics(), got.Metrics())
	}
	if len(wantSnapshot.Attempts) != len(gotSnapshot.Attempts) ||
		len(wantSnapshot.Requests) != len(gotSnapshot.Requests) {
		t.Fatalf("window %d/%d restored as %d/%d",
			len(wantSnapshot.Attempts), len(wantSnapshot.Requests),
			len(gotSnapshot.Attempts), len(gotSnapshot.Requests))
	}
	for index, attempt := range wantSnapshot.Attempts {
		if attempt.AttemptID != gotSnapshot.Attempts[index].AttemptID ||
			attempt.Sequence != gotSnapshot.Attempts[index].Sequence {
			t.Fatalf("attempt %d is %s/%d, restored as %s/%d", index,
				attempt.AttemptID, attempt.Sequence,
				gotSnapshot.Attempts[index].AttemptID, gotSnapshot.Attempts[index].Sequence)
		}
	}
	for index, request := range wantSnapshot.Requests {
		if request.RequestID != gotSnapshot.Requests[index].RequestID ||
			request.Sequence != gotSnapshot.Requests[index].Sequence {
			t.Fatalf("request %d is %s/%d, restored as %s/%d", index,
				request.RequestID, request.Sequence,
				gotSnapshot.Requests[index].RequestID, gotSnapshot.Requests[index].Sequence)
		}
	}
}

func decodeCheckpointHead(t *testing.T, payload []byte) checkpointHead {
	t.Helper()
	var head checkpointHead
	if err := json.Unmarshal(payload, &head); err != nil {
		t.Fatal(err)
	}
	return head
}

// previousFormatHead stamps a head with the version this format replaced, which
// is what a data directory carrying a v11 checkpoint presents on the first
// start after the upgrade.
func previousFormatHead(t *testing.T, payload []byte) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	object["version"] = json.RawMessage("11")
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// checkpointRecordsFrom continues the fixture past whatever the aggregate has
// already seen, so a test can add rounds without re-applying what it holds.
func checkpointRecordsFrom(aggregate *Aggregate, requests int) []ledger.Record {
	watermark := aggregate.Snapshot().Watermark
	records := checkpointKillPointRecords(requests)
	shifted := make([]ledger.Record, 0, len(records))
	for index, record := range records {
		record.Sequence = watermark.Sequence + uint64(index) + 1
		record.Offset = int64(record.Sequence) * 256
		record.Event.EventID = fmt.Sprintf("%s_%d", record.Event.EventID, watermark.Sequence)
		record.Event.RequestID = fmt.Sprintf("%s_%d", record.Event.RequestID, watermark.Sequence)
		if record.Event.AttemptID != "" {
			record.Event.AttemptID = fmt.Sprintf("%s_%d", record.Event.AttemptID, watermark.Sequence)
		}
		shifted = append(shifted, record)
	}
	return shifted
}

func applyRecords(t *testing.T, aggregate *Aggregate, records []ledger.Record) {
	t.Helper()
	for _, record := range records {
		if err := aggregate.Apply(record); err != nil {
			t.Fatal(err)
		}
	}
}

func contains(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
