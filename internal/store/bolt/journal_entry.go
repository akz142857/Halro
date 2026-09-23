package bolt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

// view runs a read-only transaction. It records nothing and exists so that a
// helper shared between a read and a write path has one signature.
func (s *Store) view(fn func(*Tx) error) error {
	return s.db.View(func(tx *bbolt.Tx) error { return fn(&Tx{tx: tx}) })
}

// update is the transaction entry of §6.1.2. Every authoritative write in this
// package goes through it, including the ones the offline CLI makes.
//
//	Begin(true)
//	→ the callback writes through the recorder
//	→ journal frame appended and fsynced
//	→ applied_journal_sequence put in the same transaction
//	→ Commit()
//
// The order is the whole design. bbolt has only a post-commit hook, so the
// entry holds the transaction itself rather than borrowing db.Update: the frame
// has to be durable before the commit, so that the journal is level with the
// projection or ahead of it and never behind. A crash between the fsync and the
// commit leaves a frame describing work the database has not done, which
// open-time reconciliation replays; the reverse would leave a committed write
// that no log describes, which nothing could recover.
func (s *Store) update(fn func(*Tx) error) error {
	return s.updateAll([]func(*Tx) error{fn})
}

// updateAll runs several callbacks in one transaction and one frame. It is the
// merge layer that replaces db.Batch — see batch below for why db.Batch could
// not be kept.
func (s *Store) updateAll(callbacks []func(*Tx) error) error {
	if len(callbacks) == 0 {
		return nil
	}
	tx, err := s.db.Begin(true)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	recorder := newRecorder()
	wrapped := &Tx{tx: tx, recorder: recorder}
	for _, callback := range callbacks {
		if err := callback(wrapped); err != nil {
			return err
		}
	}
	if err := recorder.finish(); err != nil {
		return err
	}
	if len(recorder.ops) > 0 && !s.publishing {
		if s.journal == nil {
			return fmt.Errorf("%w: refusing a metadata write that nothing would record", ErrJournalUnavailable)
		}
		record, appendErr := s.journal.Append(recorder.ops)
		if appendErr != nil {
			return fmt.Errorf("record metadata transaction: %w", appendErr)
		}
		// Written on the raw transaction rather than through the recorder: a
		// frame that recorded the sequence it had just been assigned would be
		// describing itself, and a Replica applying it would overwrite its own
		// position with the Primary's.
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		if err := meta.Put(keyAppliedJournalSequence, encodeUint64(record.Sequence)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// migrate runs a writable transaction that is deliberately not recorded.
//
// Schema creation and migration rewrite the projection as a whole rather than
// operating on it, so they are published as a new journal epoch — the database
// they leave behind *is* the epoch's starting state. Recording them instead
// would mean a Replica replaying DDL, which is the thing physical replication
// was chosen to avoid.
func (s *Store) migrate(fn func(*Tx) error) error {
	return s.db.Update(func(tx *bbolt.Tx) error { return fn(&Tx{tx: tx}) })
}

func encodeUint64(value uint64) []byte {
	encoded := make([]byte, 8)
	binary.BigEndian.PutUint64(encoded, value)
	return encoded
}

func decodeUint64(encoded []byte) (uint64, bool) {
	if len(encoded) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(encoded), true
}

// batchState is the merge layer that replaces db.Batch.
//
// db.Batch could not be kept. It calls db.Update internally, may run a callback
// more than once, and has no point between "the callbacks have run" and "the
// transaction commits" — which is exactly where the frame has to be written. So
// the coalescing is rebuilt above the entry: callers queue, one of them runs
// the whole queue in a single transaction, and the queue's writes become one
// frame and one fsync.
//
// ADR 0012's three preconditions for a re-run callback are unchanged and still
// load-bearing here: the expected outcome must not be returned as an error,
// captured variables must be reset on each entry, and the callback must be
// idempotent. What changes is only who re-runs it.
type batchState struct {
	mu      sync.Mutex
	pending []*batchCall
	timer   *time.Timer
	// delay and size are the tunables, held per store rather than read from
	// the constants directly so BenchmarkMetadataBatchDelay can sweep them.
	// They were bbolt's MaxBatchDelay and MaxBatchSize until db.Batch was
	// replaced; the numbers are the same and their meaning is unchanged.
	delay time.Duration
	size  int
}

type batchCall struct {
	fn   func(*Tx) error
	done chan error
}

// batch queues a callback and returns when its transaction has committed.
//
// A failing callback is dropped and its siblings are re-run in a fresh
// transaction, so one caller's error cannot fail another's write — the property
// db.Batch had and TestPricePinPreparationSurvivesBatchSiblingFailures pins. A
// failed attempt's operations never reach the journal, because the frame is
// written after every surviving callback has run.
func (s *Store) batch(fn func(*Tx) error) error {
	s.batchCalls.Add(1)
	call := &batchCall{fn: fn, done: make(chan error, 1)}
	s.batches.mu.Lock()
	s.batches.pending = append(s.batches.pending, call)
	first := len(s.batches.pending) == 1
	full := len(s.batches.pending) >= s.batches.size
	if first {
		s.batches.timer = time.AfterFunc(s.batches.delay, s.runBatch)
	}
	if full && s.batches.timer != nil && s.batches.timer.Stop() {
		s.batches.mu.Unlock()
		go s.runBatch()
		return <-call.done
	}
	s.batches.mu.Unlock()
	return <-call.done
}

// runBatch takes whatever has queued and commits it as one transaction.
func (s *Store) runBatch() {
	s.batches.mu.Lock()
	queued := s.batches.pending
	s.batches.pending, s.batches.timer = nil, nil
	s.batches.mu.Unlock()
	if len(queued) == 0 {
		return
	}
	s.commitBatch(queued)
}

// commitBatch runs the queue, and on a callback error drops that caller and
// retries the survivors.
//
// The retry is bounded by the queue length rather than by an attempt count:
// each pass either commits or removes exactly one caller, so a queue of n
// callers costs at most n transactions even when every one of them fails.
func (s *Store) commitBatch(queued []*batchCall) {
	for len(queued) > 0 {
		failedIndex := -1
		callbacks := make([]func(*Tx) error, len(queued))
		for index, call := range queued {
			index, call := index, call
			callbacks[index] = func(tx *Tx) error {
				if err := call.fn(tx); err != nil {
					failedIndex = index
					return err
				}
				return nil
			}
		}
		err := s.updateAll(callbacks)
		if err == nil {
			s.batchTransactions.Add(1)
			for _, call := range queued {
				call.done <- nil
			}
			return
		}
		if failedIndex < 0 {
			// The transaction itself failed — the journal append, the commit,
			// or the mixed-write check. That is nobody's callback in
			// particular, so everyone in the batch hears about it rather than
			// one caller being blamed for it.
			s.batchTransactions.Add(1)
			for _, call := range queued {
				call.done <- err
			}
			return
		}
		queued[failedIndex].done <- err
		queued = append(queued[:failedIndex], queued[failedIndex+1:]...)
	}
}

// flushBatches runs anything still queued. Close calls it so a caller blocked
// on the batch delay is not left waiting on a store that is going away.
func (s *Store) flushBatches() {
	s.batches.mu.Lock()
	timer := s.batches.timer
	s.batches.timer = nil
	s.batches.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	s.runBatch()
}

// attachedJournal is what a Store holds once its key is available.
type attachedJournal = metadatajournal.Log

// viewRaw and publishInto wrap a database this Store does not own.
//
// The offline pricing migration opens the metadata file directly, builds a
// staged copy, writes into the copy, and renames it over the original. None of
// that is an operation inside a projection — it replaces the projection — so it
// is deliberately unrecorded and the rename is what starts the next journal
// epoch. Keeping the wrappers named for what they are is the point: a raw
// *bbolt.Tx anywhere in this package would be a way to write without recording.
func viewRaw(db *bbolt.DB, fn func(*Tx) error) error {
	return db.View(func(tx *bbolt.Tx) error { return fn(&Tx{tx: tx}) })
}

func publishInto(db *bbolt.DB, fn func(*Tx) error) error {
	return db.Update(func(tx *bbolt.Tx) error { return fn(&Tx{tx: tx}) })
}
