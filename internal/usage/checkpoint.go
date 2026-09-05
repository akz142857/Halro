package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// The console's aggregate is persisted as a small head plus a series of
// immutable record segments, and a checkpoint writes only what changed.
//
// The window bounded what the aggregate holds; it did not bound what a
// checkpoint costs. Every tick re-serialized the whole resident set — at ten
// requests a second and a thirty-day window, 27 GB and forty-five seconds of a
// sixty-second interval, of which 99.998% was rewriting bytes that had not
// changed since the previous tick. Worse, the encode ran under the aggregate's
// write lock, so the collector stalled for the duration.
//
// Records are only ever appended in ledger order and dropped as a prefix (see
// PruneBefore), which is exactly the shape a segmented log fits: a tick
// re-encodes the open tail segment and nothing else, segments that fall wholly
// below the window's floor are deleted whole, and everything in between is
// already on disk and is not touched. The cost of a checkpoint is therefore
// O(records since the last one), not O(records resident).
//
// What this does not change: the aggregate itself still lives in memory, so a
// long window at high throughput still costs what it costs to hold. Serving
// the console from disk is a different question and not this one.

// checkpointVersion 13 adds Work Unit and Run attribution to request and
// attempt rows. Older checkpoints are replayed from the authenticated Ledger.
//
// checkpointVersion 12 replaced the single whole-aggregate blob with a head
// plus segments. A checkpoint written before this is refused rather than
// migrated — it is a derivative, and the Ledger rebuilds it.
//
// Version 11 recorded the console window's floor: the lowest ledger sequence
// the aggregate still holds after being pruned. Without it a restart would
// restore a windowed aggregate that claims to hold everything, and
// reconciliation would refuse — every archived-and-trimmed record read as
// missing. Version 10 carried the upstream's own identifiers for a failed
// attempt — the provider code, the provider request ID, and the phase the
// failure happened in — so a support ticket raised days after the fact can
// still name the request the upstream saw. Version 9 stamped each request
// summary with the ledger sequence that finalized it, so the failed-request
// list can page the way the attempt list does: a cursor over a slice position
// would not survive a restart, and one over the completion instant cannot
// separate two requests that finished in the same millisecond. Version 8
// recorded the accounting period on attempts and request summaries — without
// it the aggregate could say when a call finished but not which accounting day
// it was charged to, and the daily rollup, which keys on the period stamped at
// admission, had nothing to key on. Version 7 persisted the dedup window;
// version 6 dropped the duplicate cost columns.
const checkpointVersion = 13

// checkpointSegmentTargetBytes is the size an open segment is allowed to reach
// before it is sealed and a new one starts. It bounds the only work a tick
// repeats: the tail is re-encoded from the live records each time, so a tick
// costs at most this much encoding plus whatever arrived since the last one.
//
// Four mebibytes is small enough that re-encoding it is a few milliseconds and
// large enough that a busy instance does not accumulate segments faster than
// the window trims them: at ten requests a second a thirty-day window holds
// roughly seven thousand of them.
const checkpointSegmentTargetBytes = 4 << 20

// checkpointEstimatedRecordBytes is where the segment splitter starts before it
// has measured anything. One encoded attempt is roughly a kilobyte; the first
// segment of a round corrects the figure from what it actually wrote, so the
// only cost of the guess being wrong is one mis-sized segment.
const checkpointEstimatedRecordBytes = 1024

// CheckpointSegmentRef is the head's record of one segment: which records it
// holds, and whether it is still open for appending.
//
// FirstSequence and LastSequence are ledger sequences, so a segment that has
// fallen wholly below the window's floor can be identified and deleted without
// reading it.
type CheckpointSegmentRef struct {
	ID            uint64 `json:"id"`
	FirstSequence uint64 `json:"first_sequence"`
	LastSequence  uint64 `json:"last_sequence"`
	Attempts      int    `json:"attempts"`
	Summaries     int    `json:"summaries"`
	Bytes         int    `json:"bytes"`
	// Sealed marks a segment that will never be written again. The last ref is
	// the only one that may be unsealed.
	Sealed bool `json:"sealed,omitempty"`
}

// CheckpointSegment is one segment's encoded payload, ready for the store.
type CheckpointSegment struct {
	ID      uint64
	Payload []byte
}

// checkpointHead is everything whose size is set by the window's shape rather
// than its length: the totals, the histograms, the hourly buckets, the requests
// still in flight, the dedup window, and the index of segments holding the
// records. It is rewritten on every tick, which is affordable precisely because
// none of it grows with the number of records held.
type checkpointHead struct {
	Version   int              `json:"version"`
	Watermark ledger.Watermark `json:"watermark"`
	// Floor is the console window's lower edge — see PruneBefore. Absent on a
	// checkpoint that was never pruned, which reads correctly as "holds
	// everything". Restore drops any record below it, which is what lets a
	// partially trimmed segment stay on disk untouched until the floor passes
	// its last record and it can be deleted whole.
	Floor     uint64                    `json:"floor,omitempty"`
	Started   map[string]time.Time      `json:"started"`
	Active    map[string]RequestSummary `json:"active_requests"`
	Hourly    map[int64]Bucket          `json:"hourly"`
	Totals    Bucket                    `json:"totals"`
	Metrics   Metrics                   `json:"metrics"`
	Segments  []CheckpointSegmentRef    `json:"segments,omitempty"`
	NextSegID uint64                    `json:"next_segment_id,omitempty"`
	// EventIDs is the dedup window. Without it a checkpoint taken between the
	// two physical frames of a re-emitted event resumed with an empty index and
	// counted the second copy again — the aggregate then disagreed with a full
	// replay of the same WAL.
	EventIDs []string `json:"event_ids,omitempty"`
}

type checkpointSegmentPayload struct {
	Version   int              `json:"version"`
	ID        uint64           `json:"id"`
	Attempts  []AttemptEvent   `json:"attempts"`
	Summaries []RequestSummary `json:"request_summaries"`
}

// TakeCheckpoint captures what has to be written to bring the stored checkpoint
// up to the aggregate's current position, and drains the pending rollup
// increment, in one critical section — so the two describe exactly the same
// prefix of the Ledger.
//
// The caller writes the head, the segments and the increment in a single
// transaction. On success it must call CommitCheckpoint, which is what moves
// the aggregate's own record of what is on disk; on failure it must call
// ReturnCheckpoint, which hands the increment back. Nothing here mutates that
// record, so a failed write simply means the next tick proposes the same work
// again.
func (a *Aggregate) TakeCheckpoint() (CheckpointSnapshot, error) {
	a.mu.Lock()
	// Nothing has happened since the last round, so there is nothing to write.
	// Without this an idle instance re-encodes and re-fsyncs the open segment
	// and the head every interval for as long as it runs — byte-identical work,
	// forever, which is the same shape of waste this format exists to remove,
	// just at a smaller scale. The rollup increment is checked too: a round
	// that drained it without writing would lose it.
	if a.watermark.Sequence == a.persistedThrough && a.persistedThrough > 0 &&
		len(a.rollupDelta) == 0 && !a.checkpointNeedsTrim() {
		a.mu.Unlock()
		return CheckpointSnapshot{}, nil
	}
	head := checkpointHead{
		Version: checkpointVersion, Watermark: a.watermark, Floor: a.floor,
		Started: cloneStarted(a.started), Hourly: cloneHourly(a.hourly),
		Totals: a.totals, Metrics: a.metrics,
		EventIDs: append([]string(nil), a.eventIDOrder...),
	}
	head.Active = make(map[string]RequestSummary, len(a.requests))
	for requestID, accumulator := range a.requests {
		head.Active[requestID] = accumulator.summary
	}

	// Everything at or below the floor is gone from memory, so a segment whose
	// last record is below it holds nothing and can be deleted unread.
	kept := make([]CheckpointSegmentRef, 0, len(a.segments)+1)
	var removed []uint64
	for _, ref := range a.segments {
		if a.floor > 0 && ref.LastSequence < a.floor {
			removed = append(removed, ref.ID)
			continue
		}
		kept = append(kept, ref)
	}

	// The open tail is re-encoded from the live records; a sealed run before it
	// is already on disk and is not read, encoded or copied here.
	from := a.persistedThrough + 1
	tailID, reopened := uint64(0), false
	if len(kept) > 0 && !kept[len(kept)-1].Sealed {
		tail := kept[len(kept)-1]
		kept = kept[:len(kept)-1]
		tailID, from, reopened = tail.ID, tail.FirstSequence, true
	}
	attempts := attemptsFrom(a.attempts, from)
	summaries := summariesFrom(a.summaries, from)
	nextID := a.nextSegmentID
	watermark := a.watermark
	rollup := a.takeRollupDelta()
	a.mu.Unlock()

	// Encoding happens outside the lock. Under it, an encode of the whole
	// resident set was what stalled the collector for most of every interval.
	snapshot := CheckpointSnapshot{Watermark: watermark, Rollup: rollup, RemovedSegments: removed}
	bytesPerRecord := checkpointEstimatedRecordBytes
	for len(attempts) > 0 || len(summaries) > 0 {
		perSegment := checkpointSegmentTargetBytes / bytesPerRecord
		if perSegment < 1 {
			perSegment = 1
		}
		chunkAttempts, chunkSummaries := splitCheckpointRecords(&attempts, &summaries, perSegment)
		id := nextID
		if len(snapshot.Segments) == 0 && reopened {
			id = tailID
		} else {
			nextID++
		}
		payload, err := json.Marshal(checkpointSegmentPayload{
			Version: checkpointVersion, ID: id, Attempts: chunkAttempts, Summaries: chunkSummaries,
		})
		if err != nil {
			return CheckpointSnapshot{}, a.returnOnEncodeFailure(rollup,
				fmt.Errorf("encode usage checkpoint segment: %w", err))
		}
		// What the last chunk actually encoded to sizes the next one. A first
		// checkpoint after a cold replay has the whole window as its delta, and
		// a fixed guess would cut it into segments of the wrong size for this
		// install's records rather than converging on the target after one.
		if count := len(chunkAttempts) + len(chunkSummaries); count > 0 {
			if measured := len(payload) / count; measured > 0 {
				bytesPerRecord = measured
			}
		}
		kept = append(kept, CheckpointSegmentRef{
			ID: id, FirstSequence: firstSequence(chunkAttempts, chunkSummaries),
			LastSequence: lastRecordSequence(chunkAttempts, chunkSummaries),
			Attempts:     len(chunkAttempts), Summaries: len(chunkSummaries), Bytes: len(payload),
			// Everything but the final chunk is finished by construction; the
			// final one stays open unless it already reached the target.
			Sealed: len(attempts) > 0 || len(summaries) > 0 || len(payload) >= checkpointSegmentTargetBytes,
		})
		snapshot.Segments = append(snapshot.Segments, CheckpointSegment{ID: id, Payload: payload})
	}
	if len(snapshot.Segments) == 0 && reopened {
		// The tail was reopened and holds nothing: every record it had has been
		// trimmed. Drop it rather than write an empty segment.
		snapshot.RemovedSegments = append(snapshot.RemovedSegments, tailID)
	}

	head.Segments = kept
	head.NextSegID = nextID
	payload, err := json.Marshal(head)
	if err != nil {
		return CheckpointSnapshot{}, a.returnOnEncodeFailure(rollup,
			fmt.Errorf("encode usage checkpoint head: %w", err))
	}
	snapshot.Head = payload
	snapshot.segments = kept
	snapshot.nextSegmentID = nextID
	return snapshot, nil
}

// splitCheckpointRecords moves the first count records, in ledger order, off
// the two runs and returns them. Attempts and request summaries interleave by
// sequence, so a segment has to be cut across both at once or its sequence
// range would overlap its neighbour's and the head could no longer say which
// segment a record lives in.
func splitCheckpointRecords(
	attempts *[]AttemptEvent, summaries *[]RequestSummary, count int,
) ([]AttemptEvent, []RequestSummary) {
	takenAttempts, takenSummaries := 0, 0
	for taken := 0; taken < count; taken++ {
		hasAttempt, hasSummary := takenAttempts < len(*attempts), takenSummaries < len(*summaries)
		switch {
		case hasAttempt && hasSummary:
			if (*attempts)[takenAttempts].Sequence <= (*summaries)[takenSummaries].Sequence {
				takenAttempts++
			} else {
				takenSummaries++
			}
		case hasAttempt:
			takenAttempts++
		case hasSummary:
			takenSummaries++
		default:
			taken = count
		}
	}
	chunkAttempts, chunkSummaries := (*attempts)[:takenAttempts], (*summaries)[:takenSummaries]
	*attempts, *summaries = (*attempts)[takenAttempts:], (*summaries)[takenSummaries:]
	return chunkAttempts, chunkSummaries
}

// checkpointNeedsTrim reports whether the window has moved past a segment the
// stored checkpoint still holds. Called with the lock held.
//
// A quiet instance still trims — the window is measured in days, not in
// requests — so "no new records" is not on its own a reason to skip a round.
func (a *Aggregate) checkpointNeedsTrim() bool {
	if a.floor == 0 {
		return false
	}
	for _, ref := range a.segments {
		if ref.LastSequence < a.floor {
			return true
		}
	}
	return false
}

// returnOnEncodeFailure hands the drained increment back before reporting an
// encode failure. TakeCheckpoint drains it inside the lock, so every path that
// does not reach the caller has to put it back or those events are counted in
// no stored row and in no later increment.
func (a *Aggregate) returnOnEncodeFailure(rollup map[string]domain.DailyRollup, cause error) error {
	if err := a.ReturnCheckpoint(CheckpointSnapshot{Rollup: rollup}); err != nil {
		return fmt.Errorf("%w (and the rollup increment could not be restored: %v)", cause, err)
	}
	return cause
}

// CommitCheckpoint records what the store durably accepted. Until it is called
// the aggregate still believes the previous checkpoint is the one on disk, so a
// write that failed is simply proposed again on the next tick.
func (a *Aggregate) CommitCheckpoint(snapshot CheckpointSnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.segments = append([]CheckpointSegmentRef(nil), snapshot.segments...)
	a.nextSegmentID = snapshot.nextSegmentID
	a.persistedThrough = snapshot.Watermark.Sequence
}

// CheckpointState is what the last committed checkpoint costs, for metrics: how
// many segments hold the window, and how many bytes the last tick actually
// wrote as opposed to how many the window holds.
type CheckpointState struct {
	Segments int
	Bytes    int
	// Persisted is the ledger sequence the stored segments cover.
	Persisted uint64
	// OpenSegmentBytes is the one segment a tick rewrites. Zero when the last
	// round sealed everything, which means the next tick opens a fresh segment
	// and writes only what has arrived since.
	OpenSegmentBytes int
}

// Checkpointed reports the shape of the stored checkpoint.
func (a *Aggregate) Checkpointed() CheckpointState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	state := CheckpointState{Segments: len(a.segments), Persisted: a.persistedThrough}
	for _, ref := range a.segments {
		state.Bytes += ref.Bytes
		if !ref.Sealed {
			state.OpenSegmentBytes = ref.Bytes
		}
	}
	return state
}

// RestoreCheckpoint rebuilds an aggregate from a head and the segments it
// names, reading each segment through readSegment as it goes so no more than
// one is held encoded at a time.
//
// Every rejection is fail-closed: a missing segment, a segment that disagrees
// with the head about what it holds, or records out of ledger order all mean
// the checkpoint is refused and the Ledger replayed, because a checkpoint that
// silently dropped records would show a console that quietly disagrees with the
// accounting authority.
func RestoreCheckpoint(head []byte, readSegment func(id uint64) ([]byte, error)) (*Aggregate, error) {
	if len(head) == 0 {
		return nil, errors.New("usage checkpoint is empty")
	}
	var saved checkpointHead
	if err := json.Unmarshal(head, &saved); err != nil {
		return nil, fmt.Errorf("decode usage checkpoint: %w", err)
	}
	if saved.Version != checkpointVersion {
		return nil, fmt.Errorf("usage checkpoint version %d is not supported", saved.Version)
	}
	if saved.Watermark.Sequence == 0 && (saved.Watermark.Offset != 0 || saved.Watermark.Generation != 0) {
		return nil, errors.New("usage checkpoint has an invalid empty watermark")
	}
	// Any generation from the first on. Pinning this to 1 would refuse to
	// restore an aggregate the moment the Ledger sealed a generation, and the
	// checkpoint would be rebuilt from a full replay every start — correct, but
	// silently paying the cost sealing exists to avoid.
	if saved.Watermark.Sequence > 0 && (saved.Watermark.Offset <= 0 || saved.Watermark.Generation == 0) {
		return nil, errors.New("usage checkpoint has an invalid watermark")
	}
	if readSegment == nil && len(saved.Segments) > 0 {
		return nil, errors.New("usage checkpoint names segments but none can be read")
	}

	aggregate := NewAggregate()
	aggregate.watermark = saved.Watermark
	aggregate.floor = saved.Floor
	aggregate.started = cloneStarted(saved.Started)
	aggregate.hourly = cloneHourly(saved.Hourly)
	aggregate.totals = saved.Totals
	aggregate.metrics = saved.Metrics
	aggregate.segments = append([]CheckpointSegmentRef(nil), saved.Segments...)
	aggregate.nextSegmentID = saved.NextSegID
	aggregate.persistedThrough = saved.Watermark.Sequence

	previousSegment := uint64(0)
	for index, ref := range saved.Segments {
		if index > 0 && ref.ID <= previousSegment {
			return nil, errors.New("usage checkpoint segments are out of order")
		}
		// Only the last segment may be open. An earlier one left open would
		// mean a tick appended to a segment it had already sealed, and the
		// records after it would be in two places at once.
		if !ref.Sealed && index != len(saved.Segments)-1 {
			return nil, errors.New("usage checkpoint has an unsealed segment before its last")
		}
		previousSegment = ref.ID
		if ref.ID >= aggregate.nextSegmentID {
			return nil, errors.New("usage checkpoint segment id is beyond the next id")
		}
		payload, err := readSegment(ref.ID)
		if err != nil {
			return nil, fmt.Errorf("read usage checkpoint segment %d: %w", ref.ID, err)
		}
		var segment checkpointSegmentPayload
		if err := json.Unmarshal(payload, &segment); err != nil {
			return nil, fmt.Errorf("decode usage checkpoint segment %d: %w", ref.ID, err)
		}
		if segment.Version != checkpointVersion {
			return nil, fmt.Errorf("usage checkpoint segment %d version %d is not supported", ref.ID, segment.Version)
		}
		if segment.ID != ref.ID {
			return nil, fmt.Errorf("usage checkpoint segment %d holds segment %d", ref.ID, segment.ID)
		}
		if len(segment.Attempts) != ref.Attempts || len(segment.Summaries) != ref.Summaries {
			return nil, fmt.Errorf("usage checkpoint segment %d does not hold what the head claims", ref.ID)
		}
		for _, attempt := range segment.Attempts {
			if attempt.Sequence < saved.Floor {
				continue
			}
			if last := lastSequence(aggregate.attempts); last > 0 && attempt.Sequence <= last {
				return nil, errors.New("usage checkpoint attempts are out of ledger order")
			}
			aggregate.attempts = append(aggregate.attempts, attempt)
		}
		for _, summary := range segment.Summaries {
			if summary.Sequence < saved.Floor {
				continue
			}
			if last := lastSummarySequence(aggregate.summaries); last > 0 && summary.Sequence <= last {
				return nil, errors.New("usage checkpoint request summaries are out of ledger order")
			}
			aggregate.summaries = append(aggregate.summaries, summary)
		}
	}
	if last := lastSequence(aggregate.attempts); last > saved.Watermark.Sequence {
		return nil, errors.New("usage checkpoint holds an attempt past its watermark")
	}
	if last := lastSummarySequence(aggregate.summaries); last > saved.Watermark.Sequence {
		return nil, errors.New("usage checkpoint holds a request summary past its watermark")
	}

	for _, eventID := range saved.EventIDs {
		aggregate.rememberEventID(eventID)
	}
	for requestID, summary := range saved.Active {
		if requestID == "" || summary.RequestID != requestID {
			return nil, errors.New("usage checkpoint has an invalid active request")
		}
		aggregate.requests[requestID] = &requestAccumulator{summary: summary}
	}
	return aggregate, nil
}

// attemptsFrom returns a copy of the attempts at or after a ledger sequence.
// The slice is appended in ledger order, so the run is a suffix and a binary
// search finds where it starts.
func attemptsFrom(attempts []AttemptEvent, from uint64) []AttemptEvent {
	start := sort.Search(len(attempts), func(index int) bool {
		return attempts[index].Sequence >= from
	})
	if start == len(attempts) {
		return nil
	}
	return append([]AttemptEvent(nil), attempts[start:]...)
}

func summariesFrom(summaries []RequestSummary, from uint64) []RequestSummary {
	start := sort.Search(len(summaries), func(index int) bool {
		return summaries[index].Sequence >= from
	})
	if start == len(summaries) {
		return nil
	}
	return append([]RequestSummary(nil), summaries[start:]...)
}

func firstSequence(attempts []AttemptEvent, summaries []RequestSummary) uint64 {
	first := uint64(0)
	if len(attempts) > 0 {
		first = attempts[0].Sequence
	}
	if len(summaries) > 0 && (first == 0 || summaries[0].Sequence < first) {
		first = summaries[0].Sequence
	}
	return first
}

func lastRecordSequence(attempts []AttemptEvent, summaries []RequestSummary) uint64 {
	last := lastSequence(attempts)
	if summary := lastSummarySequence(summaries); summary > last {
		last = summary
	}
	return last
}
