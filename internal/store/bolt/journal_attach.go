package bolt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

// MetadataJournalFileName is the journal's name inside the data directory
// (docs/todo/halro-ha-architecture.zh-CN.md §5.1). It sits beside halro.db
// rather than under a subdirectory because the two are one unit: a data
// directory holding one without the other is not a state either of them can
// describe.
const MetadataJournalFileName = "metadata.journal"

// JournalState is where the journal and its projection stand relative to each
// other after an attach. It is what `halro doctor` reports and what a backup
// manifest records.
type JournalState struct {
	Path     string `json:"path"`
	Epoch    uint64 `json:"epoch"`
	Sequence uint64 `json:"sequence"`
	// Applied is the projection's position, read out of bbolt. After a
	// successful attach it equals Sequence; the two differ only in the window
	// between a frame's fsync and its transaction's commit.
	Applied uint64 `json:"applied"`
	// Replayed counts the transactions this attach had to re-apply, which is
	// how many times the process died in that window. Non-zero is a normal
	// crash-recovery outcome, not an error, and it is reported because an
	// instance that replays on every start has something else wrong with it.
	Replayed uint64 `json:"replayed"`
	// TrimmedThrough is the sequence the file's head was cut at, zero if it
	// still begins at its epoch header.
	TrimmedThrough uint64 `json:"trimmed_through"`
	// StartedEpoch says this attach published a new epoch rather than
	// continuing one — a fresh directory, a migration, or a whole-file publish.
	StartedEpoch bool `json:"started_epoch"`
}

// MetadataJournalPath is where this store's journal lives.
func (s *Store) MetadataJournalPath() string {
	return filepath.Join(filepath.Dir(s.db.Path()), MetadataJournalFileName)
}

// AttachMetadataJournal installs the journal and brings the projection level
// with it. Nothing this package records may be written before it returns.
//
// It cannot happen at Open. The journal's HMAC key is derived from the Master
// Key, the Master Key is unwrapped through the Vault, and the Vault's key check
// lives in this database — so the store has to be open before the key exists.
// Everything written in that window (schema creation, migrations, the key
// envelopes themselves) is therefore the *starting state* of an epoch rather
// than a set of operations inside one, which is the same rule §6.1.4 applies to
// every other whole-file publish.
//
// reason is recorded in the epoch header when a new epoch is started, so an
// operator reading a journal can see what replaced the one before it.
func (s *Store) AttachMetadataJournal(key []byte, reason string) (JournalState, error) {
	if s.journal != nil {
		return JournalState{}, errors.New("metadata journal is already attached")
	}
	path := s.MetadataJournalPath()
	state := JournalState{Path: path}
	stored, err := s.journalPosition()
	if err != nil {
		return state, err
	}
	_, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		// No journal yet: a directory initialised before this existed, or one
		// whose epoch was published by a restore that did not carry the file.
		// Either way the database on disk is the starting projection.
		return s.startEpoch(key, path, stored, state, reason)
	case statErr != nil:
		return state, statErr
	case s.migrated:
		// A migration rewrote the projection as a whole. Recording DDL as
		// operations is the thing physical replication was chosen to avoid, so
		// the migrated database becomes the next epoch's starting state.
		return s.startEpoch(key, path, stored, state, reason)
	}
	log, err := metadatajournal.Open(path, key)
	if err != nil {
		return state, fmt.Errorf("open metadata journal: %w", err)
	}
	head := log.Head()
	state.Epoch, state.Sequence, state.TrimmedThrough = head.Epoch, head.Sequence, head.TrimmedThrough
	state.Applied = stored.sequence
	if err := s.reconcileJournal(log, stored, &state, key, path); err != nil {
		log.Close()
		return state, err
	}
	s.journal, s.journalKey = log, append([]byte(nil), key...)
	return state, nil
}

type journalPosition struct {
	epoch    uint64
	sequence uint64
	// present says the projection has ever recorded a position. A database
	// created before the journal existed has not, and is not the same thing as
	// one that sits at sequence zero.
	present bool
}

func (s *Store) journalPosition() (journalPosition, error) {
	var position journalPosition
	err := s.view(func(tx *Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		epoch, hasEpoch := decodeUint64(meta.Get(keyMetadataJournalEpoch))
		sequence, hasSequence := decodeUint64(meta.Get(keyAppliedJournalSequence))
		position = journalPosition{epoch: epoch, sequence: sequence, present: hasEpoch && hasSequence}
		return nil
	})
	return position, err
}

// startEpoch publishes a fresh journal whose starting projection is the
// database as it stands right now.
func (s *Store) startEpoch(key []byte, path string, stored journalPosition, state JournalState, reason string) (JournalState, error) {
	header := metadatajournal.EpochHeader{Epoch: stored.epoch + 1, PreviousEpoch: stored.epoch, Reason: reason}
	if stored.present {
		// The chain head of the epoch being replaced, where it is still
		// readable. A restore hands over a directory whose journal belongs to
		// somebody else's chain, so this is best-effort by design: the epoch
		// header records what it could see, and the absence is itself the fact.
		if head, err := metadatajournal.Verify(path, key); err == nil && head.Epoch == stored.epoch {
			header.PreviousChainHead = head.Hash[:]
		}
	}
	log, err := metadatajournal.StartEpoch(path, key, header)
	if err != nil {
		return state, fmt.Errorf("start metadata journal epoch: %w", err)
	}
	if err := s.writeJournalPosition(header.Epoch, 0); err != nil {
		log.Close()
		return state, err
	}
	s.journal, s.journalKey = log, append([]byte(nil), key...)
	state.Epoch, state.Sequence, state.Applied, state.StartedEpoch = header.Epoch, 0, 0, true
	return state, nil
}

// reconcileJournal brings the projection level with the journal, and refuses
// every disagreement it cannot close.
//
// The journal is ahead of the projection or level with it, never behind: the
// frame is fsynced before its transaction commits. So a journal ahead is a
// crash in that window and is replayed — every recorded operation is idempotent
// by construction, which is what makes replaying safe rather than merely
// plausible. A projection ahead of the journal cannot be produced by any
// ordering this code performs, so it is a directory that was edited, restored
// by hand, or written by another binary, and it fails closed.
func (s *Store) reconcileJournal(log *metadatajournal.Log, stored journalPosition, state *JournalState, key []byte, path string) error {
	head := log.Head()
	if !stored.present {
		return fmt.Errorf(
			"%w: a metadata journal exists at %s but this database records no position in it",
			ErrJournalDiverged, path)
	}
	if stored.epoch != head.Epoch {
		return fmt.Errorf(
			"%w: this database follows metadata journal epoch %d and the file holds epoch %d",
			ErrJournalDiverged, stored.epoch, head.Epoch)
	}
	if stored.sequence > head.Sequence {
		return fmt.Errorf(
			"%w: this database applied metadata journal sequence %d and the file ends at %d",
			ErrJournalDiverged, stored.sequence, head.Sequence)
	}
	if stored.sequence == head.Sequence {
		return nil
	}
	if head.TrimmedThrough > stored.sequence {
		return fmt.Errorf(
			"%w: this database applied metadata journal sequence %d and the file was trimmed through %d, so the gap cannot be replayed",
			ErrJournalDiverged, stored.sequence, head.TrimmedThrough)
	}
	return s.replayJournal(stored, head, state, key, path)
}

// ErrJournalDiverged says the journal and the projection describe different
// states and nothing in this process can decide which is right.
//
// It is fail-closed by design: the alternatives are to trust the projection,
// which discards recorded authoritative writes, or to trust the journal, which
// replays over a database that may already hold newer state. Refusing to start
// is the only answer that loses nothing.
var ErrJournalDiverged = errors.New("metadata journal and its projection have diverged")

func (s *Store) replayJournal(stored journalPosition, head metadatajournal.Head, state *JournalState, key []byte, path string) error {
	var replayed uint64
	err := s.db.Update(func(tx *bbolt.Tx) error {
		wrapped := &Tx{tx: tx}
		if _, err := metadatajournal.Replay(path, key, func(record metadatajournal.Record) error {
			if record.Kind != metadatajournal.KindOperations || record.Sequence <= stored.sequence {
				return nil
			}
			if err := applyOps(wrapped, record.Ops); err != nil {
				return fmt.Errorf("replay metadata journal sequence %d: %w", record.Sequence, err)
			}
			replayed++
			return nil
		}); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		return meta.Put(keyAppliedJournalSequence, encodeUint64(head.Sequence))
	})
	if err != nil {
		return err
	}
	state.Applied, state.Replayed = head.Sequence, replayed
	return nil
}

// applyOps replays one recorded transaction.
//
// The replay is deliberately unrecorded: these operations are already in the
// journal, and recording them again would append frames describing work that
// has a sequence of its own. Every operation is idempotent, so a replay that is
// itself interrupted simply happens again on the next start.
func applyOps(tx *Tx, ops []metadatajournal.Op) error {
	for _, op := range ops {
		bucket, err := resolveBucket(tx, op.Path, op.Kind == metadatajournal.OpCreateBucket)
		if err != nil {
			return err
		}
		switch op.Kind {
		case metadatajournal.OpCreateBucket:
			// resolveBucket created it.
		case metadatajournal.OpPut:
			if bucket == nil {
				return fmt.Errorf("bucket %v is missing for a recorded put", op.Path)
			}
			if err := bucket.bucket.Put(op.Key, op.Value); err != nil {
				return err
			}
		case metadatajournal.OpDelete:
			if bucket == nil {
				// Deleting from a bucket that is not there is the state the
				// operation was asking for.
				continue
			}
			if err := bucket.bucket.Delete(op.Key); err != nil {
				return err
			}
		case metadatajournal.OpDeleteBucket:
			if err := deleteRecordedBucket(tx, op.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveBucket(tx *Tx, path []string, create bool) (*Bucket, error) {
	if len(path) == 0 {
		return nil, errors.New("recorded operation has no bucket path")
	}
	var current *Bucket
	for index, name := range path {
		var err error
		if index == 0 {
			if create {
				current, err = tx.CreateBucketIfNotExists([]byte(name))
			} else {
				current = tx.Bucket([]byte(name))
			}
		} else {
			if current == nil {
				return nil, nil
			}
			if create {
				current, err = current.CreateBucketIfNotExists([]byte(name))
			} else {
				current = current.Bucket([]byte(name))
			}
		}
		if err != nil {
			return nil, err
		}
		if current == nil && !create {
			return nil, nil
		}
	}
	return current, nil
}

func deleteRecordedBucket(tx *Tx, path []string) error {
	if len(path) == 1 {
		if tx.tx.Bucket([]byte(path[0])) == nil {
			return nil
		}
		return tx.tx.DeleteBucket([]byte(path[0]))
	}
	parent, err := resolveBucket(tx, path[:len(path)-1], false)
	if err != nil || parent == nil {
		return err
	}
	leaf := []byte(path[len(path)-1])
	if parent.bucket.Bucket(leaf) == nil {
		return nil
	}
	return parent.bucket.DeleteBucket(leaf)
}

func (s *Store) writeJournalPosition(epoch, sequence uint64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(bucketMeta)
		if err != nil {
			return err
		}
		if err := meta.Put(keyMetadataJournalEpoch, encodeUint64(epoch)); err != nil {
			return err
		}
		return meta.Put(keyAppliedJournalSequence, encodeUint64(sequence))
	})
}

// MetadataJournalState reports where the journal stands, for diagnostics.
func (s *Store) MetadataJournalState() (JournalState, error) {
	state := JournalState{Path: s.MetadataJournalPath()}
	stored, err := s.journalPosition()
	if err != nil {
		return state, err
	}
	state.Applied = stored.sequence
	if s.journal == nil {
		state.Epoch = stored.epoch
		return state, nil
	}
	head := s.journal.Head()
	state.Epoch, state.Sequence, state.TrimmedThrough = head.Epoch, head.Sequence, head.TrimmedThrough
	return state, nil
}

// TrimMetadataJournal drops frames the projection no longer needs.
//
// In Standalone the projection is the only reader, so everything at or below
// the applied sequence is droppable: bbolt already holds what those frames
// described. Under HA the cut is bounded by the slowest member still eligible
// for incremental catch-up (§11.2), and this call takes the sequence rather
// than deciding it.
func (s *Store) TrimMetadataJournal(throughSequence uint64) (JournalState, error) {
	if s.journal == nil {
		return JournalState{}, ErrJournalUnavailable
	}
	applied, err := s.journalPosition()
	if err != nil {
		return JournalState{}, err
	}
	if throughSequence > applied.sequence {
		return JournalState{}, fmt.Errorf(
			"cannot trim the metadata journal through %d: the projection has only applied %d",
			throughSequence, applied.sequence)
	}
	path := s.MetadataJournalPath()
	key := s.journalKey
	if err := s.journal.Close(); err != nil {
		return JournalState{}, err
	}
	s.journal = nil
	if _, err := metadatajournal.Trim(path, key, throughSequence); err != nil {
		return JournalState{}, err
	}
	log, err := metadatajournal.Open(path, key)
	if err != nil {
		return JournalState{}, err
	}
	s.journal = log
	return s.MetadataJournalState()
}

// PutMetadataJournalHMACEnvelope stores the wrapped journal key. Like the
// Ledger's and the Audit log's, it is written once and re-wrapped rather than
// replaced: rotating the Master Key must not invalidate the frames the old key
// signed.
func (s *Store) PutMetadataJournalHMACEnvelope(value []byte) error {
	if len(value) == 0 {
		return errors.New("metadata journal HMAC envelope cannot be empty")
	}
	return s.update(func(tx *Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyMetadataHMACEnvelope) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyMetadataHMACEnvelope, value)
	})
}

func (s *Store) MetadataJournalHMACEnvelope() ([]byte, error) {
	return s.metaBytes(keyMetadataHMACEnvelope)
}

// OpenForWholeFilePublish opens a metadata database that is being built to
// replace another one, rather than served.
//
// The Master Key rotation bridge, `backup restore` and CompactSnapshot all work
// the same way: stage a copy, write into it, rename it into place. Writes into
// the staged file are not operations inside a projection — they are how the
// next projection is built — so they record nothing, and the rename is what
// starts the next journal epoch (§6.1.4). The attach that follows the rename
// publishes it, which is also what deletes the frames the old file described.
//
// It is a named constructor rather than a flag on Open because it is the one
// way to write metadata without recording it, and that should be visible at the
// call site rather than inferred from a parameter.
func OpenForWholeFilePublish(path string) (*Store, error) {
	store, err := Open(path)
	if err != nil {
		return nil, err
	}
	store.publishing = true
	return store, nil
}

// DeleteMetadataJournalHMACEnvelopeForTest removes the sealed journal key.
//
// It exists so a test can stage the one state this code has to survive and
// cannot otherwise produce: a data directory written before the journal
// existed, which has neither a journal nor an envelope. Nothing in production
// removes it — the envelope is re-wrapped on rotation, never deleted.
func (s *Store) DeleteMetadataJournalHMACEnvelopeForTest() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		if err := meta.Delete(keyMetadataHMACEnvelope); err != nil {
			return err
		}
		if err := meta.Delete(keyAppliedJournalSequence); err != nil {
			return err
		}
		return nil
	})
}
