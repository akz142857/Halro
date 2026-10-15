package replication

import (
	"crypto/sha256"
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

// ReplicaProgress is the complete mutable portion of Replica member state.
// Projection.Index follows AppliedIndex even when the confirmed range contains
// no metadata record; the epoch/sequence identify the last metadata prefix
// actually materialized in bbolt.
type ReplicaProgress struct {
	DurableIndex    uint64
	ConfirmedIndex  uint64
	AppliedIndex    uint64
	OrderingHeadMAC [sha256.Size]byte
	Projection      ProjectionState
}

// ReplicaStateWriter durably publishes the complete progress tuple after the
// native store and ordering journal have reached progress.DurableIndex. A nil return
// must mean state.json and its directory entry are durable. The ordering head
// is supplied so the persisted index is bound to the exact authenticated
// journal prefix rather than merely to a number.
type ReplicaStateWriter func(progress ReplicaProgress) error

type ReplicaReceiverOptions struct {
	PersistState ReplicaStateWriter
	Projection   ProjectionState
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
	projection     ProjectionState
	anchorSeen     bool
	storeCursors   map[Store]StoreCursor
	journal        *OrderingJournal
	sink           DurableFrameSink
	persistState   ReplicaStateWriter
	poisoned       error
}

func NewReplicaReceiver(clusterID, incarnation, nodeID, primaryNodeID string, term, promisedTerm, confirmedIndex, appliedIndex uint64, journal *OrderingJournal, sink DurableFrameSink, receiverOptions ...ReplicaReceiverOptions) (*ReplicaReceiver, error) {
	if len(clusterID) == 0 || len(clusterID) > MaxIdentityBytes || len(incarnation) == 0 || len(incarnation) > MaxIdentityBytes ||
		len(nodeID) == 0 || len(nodeID) > MaxIdentityBytes || len(primaryNodeID) > MaxIdentityBytes ||
		primaryNodeID == nodeID || term == 0 || promisedTerm < term {
		return nil, errors.New("replica receiver identity or term is invalid")
	}
	if journal == nil || sink == nil {
		return nil, errors.New("replica receiver requires a journal and durable frame sink")
	}
	if len(receiverOptions) > 1 {
		return nil, errors.New("replica receiver accepts at most one options value")
	}
	var options ReplicaReceiverOptions
	if len(receiverOptions) == 1 {
		options = receiverOptions[0]
	}
	if options.Projection.Index > appliedIndex {
		return nil, errors.New("replica projection index exceeds applied index")
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
		projection: options.Projection, anchorSeen: durableTerm == term, storeCursors: cursors, journal: journal, sink: sink, persistState: options.PersistState,
	}, nil
}

// BindPrimary selects the one authenticated Primary observed during startup.
// Member state intentionally stores roles, not a separately configured leader
// ID, so a Replica learns this from peer Hello adjudication. A second distinct
// Primary in the same process lifetime is a split-brain signal and is refused.
func (r *ReplicaReceiver) BindPrimary(nodeID string) error {
	if len(nodeID) == 0 || len(nodeID) > MaxIdentityBytes || nodeID == r.nodeID {
		return errors.New("replica Primary identity is invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primaryNodeID == "" {
		r.primaryNodeID = nodeID
		return nil
	}
	if r.primaryNodeID != nodeID {
		return errors.New("replica observed more than one Primary for the active term")
	}
	return nil
}

// Promise serializes the durable promised_term publication with frame
// admission. Once persist returns, no lower-term frame can slip through the
// in-memory receiver before promisedTerm is updated.
func (r *ReplicaReceiver) Promise(term uint64, persist func() error) error {
	if persist == nil {
		return errors.New("replica promise persistence callback is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return fmt.Errorf("replica receiver requires restart after uncertain persistence: %w", r.poisoned)
	}
	if term <= r.promisedTerm {
		return errors.New("proposed term must be greater than promised_term")
	}
	if err := persist(); err != nil {
		r.poisoned = err
		return fmt.Errorf("persist Replica promised term: %w", err)
	}
	r.promisedTerm = term
	return nil
}

// AdoptTerm activates the term already promised during prepare. It is called
// only after an authenticated Primary for that exact term appears. Holding the
// receiver lock across state publication prevents an old-term frame from
// being admitted between the durable role change and the in-memory one.
func (r *ReplicaReceiver) AdoptTerm(term uint64, primaryNodeID string, persist func() error) error {
	if persist == nil || primaryNodeID == "" || primaryNodeID == r.nodeID {
		return errors.New("replica term adoption requires a Primary and persistence callback")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return fmt.Errorf("replica receiver requires restart after uncertain persistence: %w", r.poisoned)
	}
	if term <= r.term || term != r.promisedTerm {
		return errors.New("adopted term must equal the newer durable promise")
	}
	if err := persist(); err != nil {
		r.poisoned = err
		return fmt.Errorf("persist Replica active term: %w", err)
	}
	r.term = term
	r.primaryNodeID = primaryNodeID
	r.anchorSeen = false
	return nil
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
		// The Primary durably records a frame before it can send it. This
		// Replica's authenticated journal already contains the same frame, so
		// together they are the two durable votes required by both supported
		// topologies. Persist that quorum fact even when the original ACK or a
		// later commit notice was lost.
		if frame.Index > r.confirmedIndex {
			if err := r.persistProgress(r.durableIndex, frame.Index, r.appliedIndex, r.projection); err != nil {
				r.poisoned = err
				return Acknowledgement{}, fmt.Errorf("persist replica confirmation watermark: %w", err)
			}
			r.confirmedIndex = frame.Index
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
	persistedRecord, err := r.journal.Append(record)
	if err != nil {
		r.poisoned = err
		return Acknowledgement{}, fmt.Errorf("persist replicated ordering record: %w", err)
	}
	// Primary local durability plus this Replica's sink and ordering fsync is a
	// quorum in the supported two- and three-member groups. Confirmation is a
	// durability fact, not dependent on whether the ACK reaches the Primary.
	confirmedIndex := frame.Index
	if err := r.persistProgress(frame.Index, confirmedIndex, r.appliedIndex, r.projection, persistedRecord.MAC); err != nil {
		r.poisoned = err
		return Acknowledgement{}, fmt.Errorf("persist replica durable watermark: %w", err)
	}
	r.durableIndex = frame.Index
	r.confirmedIndex = confirmedIndex
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
	projection := r.projection
	r.mu.Unlock()
	projection.Index = index
	return r.AdvanceAppliedWithProjection(index, projection)
}

// AdvanceAppliedWithProjection publishes derived-state progress only after the
// caller has made both Ledger State and the metadata projection visible.
func (r *ReplicaReceiver) AdvanceAppliedWithProjection(index uint64, projection ProjectionState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return fmt.Errorf("replica receiver requires restart after uncertain persistence: %w", r.poisoned)
	}
	if index < r.appliedIndex || index > r.confirmedIndex {
		return errors.New("replica applied index must advance monotonically within the confirmed prefix")
	}
	if projection.Index != index || projection.MetadataEpoch < r.projection.MetadataEpoch ||
		projection.MetadataEpoch == r.projection.MetadataEpoch && projection.MetadataSequence < r.projection.MetadataSequence {
		return errors.New("replica projection must match applied index and advance metadata monotonically")
	}
	if index == r.appliedIndex && projection == r.projection {
		return nil
	}
	if err := r.persistProgress(r.durableIndex, r.confirmedIndex, index, projection); err != nil {
		r.poisoned = err
		return fmt.Errorf("persist replica applied watermark: %w", err)
	}
	r.appliedIndex = index
	r.projection = projection
	return nil
}

func (r *ReplicaReceiver) Confirm(notice CommitNotice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return fmt.Errorf("replica receiver requires restart after uncertain persistence: %w", r.poisoned)
	}
	if err := notice.Validate(); err != nil {
		return err
	}
	if r.promisedTerm != r.term {
		return errors.New("commit notice term is below this member's durable promise")
	}
	if notice.ClusterID != r.clusterID || notice.Incarnation != r.incarnation || notice.NodeID != r.primaryNodeID || notice.Term != r.term {
		return errors.New("commit notice identity or term does not match the active Primary")
	}
	if notice.ConfirmedIndex > r.durableIndex {
		return errors.New("commit notice exceeds the Replica durable prefix")
	}
	if notice.ConfirmedIndex > r.confirmedIndex {
		if err := r.persistProgress(r.durableIndex, notice.ConfirmedIndex, r.appliedIndex, r.projection); err != nil {
			r.poisoned = err
			return fmt.Errorf("persist replica confirmation watermark: %w", err)
		}
		r.confirmedIndex = notice.ConfirmedIndex
	}
	return nil
}

// Acknowledgement returns the current durable/apply progress for a commit
// notice response. Index zero has no valid wire representation and is refused.
func (r *ReplicaReceiver) Acknowledgement() (Acknowledgement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return Acknowledgement{}, fmt.Errorf("replica receiver requires restart after uncertain persistence: %w", r.poisoned)
	}
	ack := r.acknowledgement()
	if err := ack.Validate(); err != nil {
		return Acknowledgement{}, err
	}
	return ack, nil
}

func (r *ReplicaReceiver) persistProgress(durableIndex, confirmedIndex, appliedIndex uint64, projection ProjectionState, heads ...[sha256.Size]byte) error {
	if r.persistState == nil {
		return nil
	}
	var head [sha256.Size]byte
	if len(heads) > 1 {
		return errors.New("replica progress accepts at most one ordering head")
	}
	if len(heads) == 1 {
		head = heads[0]
	} else {
		journalIndex, _, journalHead := r.journal.Head()
		if journalIndex != durableIndex {
			return fmt.Errorf("ordering head %d does not match replica durable index %d", journalIndex, durableIndex)
		}
		head = journalHead
	}
	return r.persistState(ReplicaProgress{
		DurableIndex: durableIndex, ConfirmedIndex: confirmedIndex, AppliedIndex: appliedIndex,
		OrderingHeadMAC: head, Projection: projection,
	})
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

func (r *ReplicaReceiver) ProjectionProgress() ProjectionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.projection
}

func (r *ReplicaReceiver) HealthError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.poisoned
}
