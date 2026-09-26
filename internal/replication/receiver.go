package replication

import (
	"errors"
	"fmt"
	"sync"
)

var ErrMemberRequiresFullReseed = errors.New("member requires full reseed")

type StoreCursor struct {
	Generation uint64
	Sequence   uint64
}

// DurableFrameSink owns the store-specific validation and fsync. Persist must
// not return nil until the exact frame bytes are durable in the named store.
// It must be idempotent only at its own native frame boundary; ReplicaReceiver
// filters transport retransmissions before calling it.
type DurableFrameSink interface {
	Persist(frame Frame) error
}

type ReplicaReceiver struct {
	mu             sync.Mutex
	clusterID      string
	incarnation    string
	nodeID         string
	primaryNodeID  string
	term           uint64
	promisedTerm   uint64
	durableIndex   uint64
	confirmedIndex uint64
	appliedIndex   uint64
	anchorSeen     bool
	storeCursors   map[Store]StoreCursor
	journal        *OrderingJournal
	sink           DurableFrameSink
	poisoned       error
}

func NewReplicaReceiver(clusterID, incarnation, nodeID, primaryNodeID string, term, promisedTerm, confirmedIndex, appliedIndex uint64, journal *OrderingJournal, sink DurableFrameSink) (*ReplicaReceiver, error) {
	if len(clusterID) == 0 || len(clusterID) > MaxIdentityBytes || len(incarnation) == 0 || len(incarnation) > MaxIdentityBytes ||
		len(nodeID) == 0 || len(nodeID) > MaxIdentityBytes || len(primaryNodeID) == 0 || len(primaryNodeID) > MaxIdentityBytes ||
		primaryNodeID == nodeID || term == 0 || promisedTerm < term {
		return nil, errors.New("replica receiver identity or term is invalid")
	}
	if journal == nil || sink == nil {
		return nil, errors.New("replica receiver requires a journal and durable frame sink")
	}
	durableIndex, durableTerm, _ := journal.Head()
	if durableTerm > term || durableTerm > promisedTerm {
		return nil, errors.New("replica receiver term is behind its ordering journal")
	}
	if appliedIndex > confirmedIndex || confirmedIndex > durableIndex {
		return nil, errors.New("replica receiver applied/confirmed indexes exceed their allowed prefixes")
	}
	orderedCursors, err := journal.StoreCursors()
	if err != nil {
		return nil, fmt.Errorf("reconstruct replica ordering cursors: %w", err)
	}
	cursors := make(map[Store]StoreCursor, len(orderedCursors))
	for store := StoreLedger; store <= StoreMetadata; store++ {
		cursors[store] = orderedCursors[store-StoreLedger]
	}
	return &ReplicaReceiver{
		clusterID: clusterID, incarnation: incarnation, nodeID: nodeID, primaryNodeID: primaryNodeID,
		term: term, promisedTerm: promisedTerm, durableIndex: durableIndex, confirmedIndex: confirmedIndex, appliedIndex: appliedIndex,
		anchorSeen: durableTerm == term, storeCursors: cursors, journal: journal, sink: sink,
	}, nil
}

func (r *ReplicaReceiver) Receive(encoded []byte) (Acknowledgement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return Acknowledgement{}, fmt.Errorf("replica receiver requires restart after uncertain persistence: %w", r.poisoned)
	}
	frame, digest, err := UnmarshalFrameAndDigest(encoded)
	if err != nil {
		return Acknowledgement{}, err
	}
	if frame.ClusterID != r.clusterID || frame.Incarnation != r.incarnation {
		return Acknowledgement{}, errors.New("replication frame identity does not match this member")
	}
	if frame.Term < r.promisedTerm {
		return Acknowledgement{}, errors.New("replication frame term is below this member's durable promise")
	}
	if frame.Term != r.term {
		return Acknowledgement{}, errors.New("replication frame term is not the active replica term")
	}
	if frame.ConfirmedIndex > r.durableIndex {
		return Acknowledgement{}, errors.New("replication frame confirms a prefix this member has not durably received")
	}
	if frame.Index <= r.durableIndex {
		record, err := r.journal.Record(frame.Index)
		if err != nil {
			return Acknowledgement{}, err
		}
		if record.FrameDigest != digest {
			return Acknowledgement{}, errors.New("replication index already has different frame bytes")
		}
		if frame.ConfirmedIndex > r.confirmedIndex {
			r.confirmedIndex = frame.ConfirmedIndex
		}
		return r.acknowledgement(), nil
	}
	if frame.Index != r.durableIndex+1 {
		return Acknowledgement{}, fmt.Errorf("replication frame index %d does not continue durable index %d", frame.Index, r.durableIndex)
	}
	if !r.anchorSeen {
		if frame.Kind != KindLeadershipEstablished {
			return Acknowledgement{}, errors.New("replica term must begin with leadership_established")
		}
	} else if frame.Kind == KindLeadershipEstablished {
		return Acknowledgement{}, errors.New("replica term already has a leadership anchor")
	}
	if frame.Kind == KindData {
		cursor := r.storeCursors[frame.Store]
		if cursor.Generation == 0 {
			if frame.StoreSequenceFirst != 1 {
				return Acknowledgement{}, fmt.Errorf("replication store %d starts at sequence %d without a seeded cursor", frame.Store, frame.StoreSequenceFirst)
			}
		} else if frame.StoreGeneration != cursor.Generation {
			if frame.Store == StoreMetadata {
				return Acknowledgement{}, fmt.Errorf("%w: metadata journal epoch changed from %d to %d", ErrMemberRequiresFullReseed, cursor.Generation, frame.StoreGeneration)
			}
			return Acknowledgement{}, fmt.Errorf("replication store %d generation %d does not continue generation %d", frame.Store, frame.StoreGeneration, cursor.Generation)
		} else if frame.StoreSequenceFirst != cursor.Sequence+1 {
			return Acknowledgement{}, fmt.Errorf("replication store %d sequence %d does not continue %d", frame.Store, frame.StoreSequenceFirst, cursor.Sequence)
		}
	} else if frame.Kind == KindLedgerRoll {
		roll, err := DecodeLedgerRollMetadata(frame.Metadata)
		if err != nil {
			return Acknowledgement{}, err
		}
		cursor := r.storeCursors[StoreLedger]
		if cursor.Generation != roll.Generation || cursor.Sequence != roll.LastSequence {
			return Acknowledgement{}, errors.New("ledger roll does not match the Replica ledger cursor")
		}
	}
	if err := r.sink.Persist(frame); err != nil {
		r.poisoned = err
		return Acknowledgement{}, fmt.Errorf("persist replicated frame: %w", err)
	}
	record := OrderingRecord{
		Kind: frame.Kind, Store: frame.Store, Index: frame.Index, Term: frame.Term,
		ConfirmedIndex: frame.ConfirmedIndex, Metadata: append([]byte(nil), frame.Metadata...),
		StoreGeneration:    frame.StoreGeneration,
		StoreSequenceFirst: frame.StoreSequenceFirst, StoreSequenceLast: frame.StoreSequenceLast,
		FrameDigest: digest,
	}
	if _, err := r.journal.Append(record); err != nil {
		r.poisoned = err
		return Acknowledgement{}, fmt.Errorf("persist replicated ordering record: %w", err)
	}
	r.durableIndex = frame.Index
	if frame.ConfirmedIndex > r.confirmedIndex {
		r.confirmedIndex = frame.ConfirmedIndex
	}
	if frame.Kind == KindLeadershipEstablished {
		r.anchorSeen = true
	}
	if frame.Kind == KindData {
		r.storeCursors[frame.Store] = StoreCursor{Generation: frame.StoreGeneration, Sequence: frame.StoreSequenceLast}
	} else if frame.Kind == KindLedgerRoll {
		roll, _ := DecodeLedgerRollMetadata(frame.Metadata)
		r.storeCursors[StoreLedger] = StoreCursor{Generation: roll.Generation + 1, Sequence: roll.LastSequence}
	}
	return r.acknowledgement(), nil
}

func (r *ReplicaReceiver) AdvanceApplied(index uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < r.appliedIndex || index > r.confirmedIndex {
		return errors.New("replica applied index must advance monotonically within the confirmed prefix")
	}
	r.appliedIndex = index
	return nil
}

func (r *ReplicaReceiver) Confirm(notice CommitNotice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := notice.Validate(); err != nil {
		return err
	}
	if notice.ClusterID != r.clusterID || notice.Incarnation != r.incarnation || notice.NodeID != r.primaryNodeID || notice.Term != r.term {
		return errors.New("commit notice identity or term does not match the active Primary")
	}
	if notice.ConfirmedIndex > r.durableIndex {
		return errors.New("commit notice exceeds the Replica durable prefix")
	}
	if notice.ConfirmedIndex > r.confirmedIndex {
		r.confirmedIndex = notice.ConfirmedIndex
	}
	return nil
}

func (r *ReplicaReceiver) acknowledgement() Acknowledgement {
	return Acknowledgement{
		ClusterID: r.clusterID, Incarnation: r.incarnation, NodeID: r.nodeID,
		Index: r.durableIndex, Term: r.term, DurableIndex: r.durableIndex, AppliedIndex: r.appliedIndex,
	}
}

func (r *ReplicaReceiver) Progress() (durable, confirmed, applied uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.durableIndex, r.confirmedIndex, r.appliedIndex
}
