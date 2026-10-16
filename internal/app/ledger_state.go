package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// Accounting state snapshots.
//
// The state is rebuilt on every start, and rebuilding it from the first frame
// takes as long as the history is. A snapshot of it is written while the
// instance runs, and a start resumes from the newest one, replaying only what
// the Ledger holds after it. The snapshot is a derivative like every other:
// one that is missing, fails its MAC, or does not meet the Ledger where it
// says it does is set aside and the state is rebuilt from the first frame.
const (
	ledgerStateSnapshotInterval = time.Minute
	ledgerStateSnapshotBytes    = int64(64) << 20
	ledgerStateSnapshotPoll     = 5 * time.Second
)

// accountingStateRuntime is what the runtime keeps for the accounting state
// beyond the state itself.
type accountingStateRuntime struct {
	// fromSnapshot reports that this start resumed from a snapshot.
	fromSnapshot   bool
	snapshots      ledgerStateSnapshots
	reconciliation usageReconciliation
}

// ledgerStateSnapshots is the snapshot writer's key and what it last wrote,
// for metrics.
type ledgerStateSnapshots struct {
	key      []byte
	sequence atomic.Uint64
	bytes    atomic.Int64
	unix     atomic.Int64
	failures atomic.Uint64
}

// openedLedgerState is the accounting state as a start rebuilt it, with what
// price pin recovery needs to know about how.
type openedLedgerState struct {
	state *ledger.State
	head  ledger.Watermark
	// fromSnapshot reports that the state resumed from a snapshot.
	fromSnapshot bool
	// suffixLeases are the priced leases replayed after the snapshot. The
	// state keeps only in-flight leases, so these are what a committed pin
	// written since the snapshot is checked against.
	suffixLeases map[string]ledger.Record
	// checkedThrough is the sequence after which every priced lease was
	// replayed this start, so a committed pin whose lease lies after it must
	// find that lease.
	checkedThrough uint64
}

func (o openedLedgerState) lease(attemptID string) (ledger.Record, bool) {
	if record, ok := o.state.AccountingLease(attemptID); ok {
		return record, true
	}
	record, ok := o.suffixLeases[attemptID]
	return record, ok
}

// openLedgerState rebuilds the accounting state, from the snapshot at path
// when there is a usable one and from the first frame otherwise.
func openLedgerState(log *ledger.Log, path string, key []byte, logger *slog.Logger) (openedLedgerState, error) {
	snapshot, chain, err := ledger.ReadStateSnapshot(path, key)
	switch {
	case err == nil:
		opened, resumeErr := resumeLedgerState(log, snapshot, chain)
		if resumeErr == nil {
			return opened, nil
		}
		logger.Warn("ledger state snapshot does not meet the Ledger; rebuilding from the first frame", "error", resumeErr)
	case errors.Is(err, os.ErrNotExist):
	default:
		logger.Warn("ledger state snapshot is unusable; rebuilding from the first frame", "error", err)
	}
	state := ledger.NewState()
	head, err := log.Replay(ledger.Watermark{}, state.Apply)
	if err != nil {
		return openedLedgerState{}, err
	}
	// A full replay keeps no settled leases either, so the committed pins of
	// settled attempts are not checked here; each was committed with the
	// sequence its own lease append returned, and the backup check replays
	// the Ledger to check them again.
	return openedLedgerState{state: state, head: head, checkedThrough: head.Sequence}, nil
}

// resumeLedgerState replays the Ledger after the snapshot's position into the
// snapshot's state.
//
// A snapshot inside a sealed generation is replayed from that generation's
// first frame: a sealed file is authenticated only by a walk from its start,
// and resuming mid-file would apply frames nobody checked on this start. The
// frames before the snapshot's position are walked, not applied, and the one
// at that position must end exactly where the snapshot says. The active
// generation was authenticated end to end when the Ledger opened, so a
// snapshot in it resumes at its own offset.
//
// Positions alone do not tie the snapshot to this Ledger: a copy written
// under the same key that went another way can be the same length, frame for
// frame. The chain head the snapshot carries does. The frame after it must
// link to it, or, when it is this Ledger's head, the head must be it.
func resumeLedgerState(log *ledger.Log, state *ledger.State, chain ledger.StateSnapshotChain) (openedLedgerState, error) {
	position := state.Watermark()
	if position.Sequence == 0 {
		return openedLedgerState{}, errors.New("snapshot is empty")
	}
	if chain.Sequence < position.Sequence || chain.Hash == ([32]byte{}) {
		return openedLedgerState{}, errors.New("the snapshot carries no chain head covering its state")
	}
	chainHead, chainHash, chained := log.ChainHead()
	switch {
	case !chained || chain.Sequence > chainHead.Sequence:
		return openedLedgerState{}, errors.New("the snapshot's chain head is ahead of the Ledger")
	case chain.Sequence == chainHead.Sequence && chain.Hash != chainHash:
		return openedLedgerState{}, errors.New("the snapshot's chain head is not this Ledger's")
	}
	linked := chain.Sequence == chainHead.Sequence
	// The snapshot is authenticated, but it may still describe another copy
	// of this Ledger — one restored over, or cut back. Neither its sequence nor
	// its offset may lie past what the Ledger holds.
	if head, _, ok := log.ChainHead(); ok && position.After(head) {
		return openedLedgerState{}, errors.New("the snapshot is ahead of the Ledger")
	}
	from, met := position, true
	if position.Generation == log.Generation() && position.Offset > log.ActiveBytes() {
		return openedLedgerState{}, errors.New("the snapshot is past the end of the active generation")
	}
	if position.Generation != log.Generation() {
		segment, found := sealedSegment(log, position.Generation)
		if !found {
			return openedLedgerState{}, fmt.Errorf("snapshot names generation %d, which the Ledger does not hold", position.Generation)
		}
		from, met = ledger.Watermark{Generation: segment.Generation, Sequence: segment.FirstSequence - 1}, false
	}
	opened := openedLedgerState{state: state, fromSnapshot: true, suffixLeases: map[string]ledger.Record{}, checkedThrough: position.Sequence}
	head, err := log.Replay(from, func(record ledger.Record) error {
		if record.Sequence == chain.Sequence+1 {
			if record.PreviousHash != chain.Hash {
				return errors.New("the snapshot's chain head is not this Ledger's")
			}
			linked = true
		}
		if record.Sequence <= position.Sequence {
			if record.Sequence == position.Sequence {
				if record.Generation != position.Generation || record.Offset != position.Offset {
					return errors.New("the record at the snapshot's sequence is not where the snapshot says")
				}
				met = true
			}
			return nil
		}
		if !met {
			return errors.New("the Ledger has no record at the snapshot's position")
		}
		if err := state.Apply(record); err != nil {
			return err
		}
		if record.Event.Kind == ledger.EventReservationCreated && record.Event.PriceSnapshot != nil &&
			record.Event.PriceSnapshot.PriceEvidenceStatus == domain.PriceEvidenceVersioned {
			opened.suffixLeases[record.Event.AttemptID] = record
		}
		return nil
	})
	if err != nil {
		return openedLedgerState{}, err
	}
	if !met || position.After(head) {
		return openedLedgerState{}, errors.New("the snapshot is ahead of the Ledger")
	}
	if !linked {
		return openedLedgerState{}, errors.New("the Ledger has no frame after the snapshot's chain head")
	}
	opened.head = head
	return opened, nil
}

func sealedSegment(log *ledger.Log, generation uint64) (ledger.Segment, bool) {
	for _, segment := range log.Segments() {
		if segment.Generation == generation {
			return segment, true
		}
	}
	return ledger.Segment{}, false
}

// runLedgerStateSnapshots writes a snapshot once a minute, or sooner once the
// Ledger has grown by ledgerStateSnapshotBytes or rolled, so a start replays a
// bounded suffix however busy the instance is. It starts after lease recovery,
// so a recovery settlement written twice is never split across a snapshot.
//
// The roll is checked on the same poll. Opening the Ledger reads the whole
// active generation, so the generation has to be held near its configured size
// for the start to stay bounded; checked only on the hourly maintenance tick,
// a busy instance wrote gigabytes past it.
func (r *Runtime) runLedgerStateSnapshots(ctx context.Context) {
	ticker := time.NewTicker(ledgerStateSnapshotPoll)
	defer ticker.Stop()
	last := time.Now()
	generation, bytes := r.ledger.Generation(), r.ledger.ActiveBytes()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r.sealLedgerGeneration()
		nowGeneration, nowBytes := r.ledger.Generation(), r.ledger.ActiveBytes()
		grown := nowGeneration != generation || nowBytes-bytes >= ledgerStateSnapshotBytes
		if !grown && time.Since(last) < ledgerStateSnapshotInterval {
			continue
		}
		r.writeLedgerStateSnapshot()
		last, generation, bytes = time.Now(), nowGeneration, nowBytes
	}
}

// writeLedgerStateSnapshot writes one snapshot unless the state has not moved
// since the last.
func (r *Runtime) writeLedgerStateSnapshot() {
	if r.state.Watermark().Sequence == r.accountingState.snapshots.sequence.Load() {
		return
	}
	watermark, size, err := ledger.WriteStateSnapshot(r.config.LedgerStatePath(), r.accountingState.snapshots.key, r.state, r.ledger)
	if err != nil {
		r.accountingState.snapshots.failures.Add(1)
		r.logger.Warn("ledger state snapshot was not written", "error", err)
		return
	}
	r.accountingState.snapshots.sequence.Store(watermark.Sequence)
	r.accountingState.snapshots.bytes.Store(int64(size))
	r.accountingState.snapshots.unix.Store(time.Now().Unix())
}
