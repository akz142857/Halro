package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrReplicationUnavailable = errors.New("replication unavailable")

// OutboundQueue keeps data frames and commit notices in separate namespaces.
// A nil QueueFrame return retains the frame until a current-term durable ACK;
// a nil QueueCommitNotice return retains the latest notice until every
// configured peer reports applied_index at or beyond it. A later notice
// subsumes an earlier one.
// Network delivery itself may be asynchronous.
type OutboundQueue interface {
	QueueFrame(index uint64, encoded []byte) error
	QueueCommitNotice(confirmedIndex uint64, encoded []byte) error
	Acknowledge(nodeID string, durableIndex, appliedIndex uint64)
}

type LocalCommit struct {
	Index                uint64
	Encoded              []byte
	ReplicationAvailable bool
}

type PrimaryProgress struct {
	DurableIndex    uint64
	ConfirmedIndex  uint64
	OrderingHeadMAC [sha256.Size]byte
	Projection      ProjectionState
}

// PrimaryStateWriter atomically publishes the complete authenticated member
// progress. Returning nil must mean the state file and its directory entry are
// durable. Both local durability and quorum confirmation pass through it so
// state.json is a live watermark rather than merely a restart lower bound.
type PrimaryStateWriter func(progress PrimaryProgress) error

// PrimaryCoordinator is the single global index allocator shared by all four
// store commit paths. RecordDurable must be called only after the source bytes'
// fsync and before that store reports success to its caller.
type PrimaryCoordinator struct {
	mu                 sync.Mutex
	clusterID          string
	incarnation        string
	nodeID             string
	term               uint64
	journal            *OrderingJournal
	confirmations      *ConfirmationTracker
	outbound           OutboundQueue
	persistState       PrimaryStateWriter
	confirmed          uint64
	projection         ProjectionState
	available          bool
	unavailable        error
	changed            chan struct{}
	pending            map[uint64][]byte
	pendingNotice      []byte
	pendingNoticeIndex uint64
	failedFrameIndex   uint64
	peerApplied        map[string]uint64
	noticeUnavailable  bool
	manualUnavailable  error
	poisoned           error
}

// NewPrimaryCoordinator accepts the exact encoded frames for any authenticated
// ordering suffix above confirmed. Callers reconstruct those bytes from the
// four stores during startup; omitting a non-empty suffix fails closed instead
// of either losing it or pretending it was confirmed.
func NewPrimaryCoordinator(clusterID, incarnation, nodeID string, term, confirmed uint64, peers []string, journal *OrderingJournal, outbound OutboundQueue, persistState PrimaryStateWriter, recovered ...LocalCommit) (*PrimaryCoordinator, error) {
	if journal == nil || outbound == nil || persistState == nil {
		return nil, errors.New("primary coordinator requires an ordering journal, outbound queue and state writer")
	}
	if len(nodeID) == 0 || len(nodeID) > MaxIdentityBytes {
		return nil, errors.New("primary coordinator node identity is invalid")
	}
	index, journalTerm, _ := journal.Head()
	if index < confirmed {
		return nil, fmt.Errorf("primary coordinator confirmed index %d exceeds ordering head %d", confirmed, index)
	}
	if journalTerm > term {
		return nil, errors.New("primary coordinator term is behind its ordering journal")
	}
	established := false
	if confirmed > 0 {
		record, recordErr := journal.Record(confirmed)
		if recordErr != nil {
			return nil, fmt.Errorf("read confirmed ordering record: %w", recordErr)
		}
		established = record.Term == term
	}
	tracker, err := NewConfirmationTracker(clusterID, incarnation, term, confirmed, peers, established)
	if err != nil {
		return nil, err
	}
	cursors, err := journal.StoreCursorsThrough(confirmed)
	if err != nil {
		return nil, fmt.Errorf("reconstruct confirmed primary projection: %w", err)
	}
	metadataCursor := cursors[StoreMetadata-StoreLedger]
	projection := ProjectionState{Index: confirmed, MetadataEpoch: metadataCursor.Generation, MetadataSequence: metadataCursor.Sequence}
	coordinator := &PrimaryCoordinator{
		clusterID: clusterID, incarnation: incarnation, nodeID: nodeID, term: term, journal: journal,
		confirmations: tracker, outbound: outbound, persistState: persistState,
		confirmed: confirmed, projection: projection, available: true, changed: make(chan struct{}),
		pending: make(map[uint64][]byte), peerApplied: make(map[string]uint64, len(peers)),
	}
	for _, peer := range peers {
		coordinator.peerApplied[peer] = 0
	}
	if uint64(len(recovered)) != index-confirmed {
		return nil, fmt.Errorf("primary coordinator needs %d recovered suffix frames, got %d", index-confirmed, len(recovered))
	}
	for offset, commit := range recovered {
		expectedIndex := confirmed + uint64(offset) + 1
		if commit.Index != expectedIndex {
			return nil, fmt.Errorf("recovered frame index %d does not continue %d", commit.Index, expectedIndex-1)
		}
		frame, digest, decodeErr := UnmarshalFrameAndDigest(commit.Encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode recovered frame %d: %w", commit.Index, decodeErr)
		}
		record, recordErr := journal.Record(expectedIndex)
		if recordErr != nil {
			return nil, recordErr
		}
		if frame.Index != expectedIndex || frame.ClusterID != clusterID || frame.Incarnation != incarnation || frame.Term != term ||
			record.Term != frame.Term || record.ConfirmedIndex != frame.ConfirmedIndex || !bytes.Equal(record.Metadata, frame.Metadata) ||
			record.FrameDigest != digest || record.Kind != frame.Kind || record.Store != frame.Store ||
			record.StoreGeneration != frame.StoreGeneration || record.StoreSequenceFirst != frame.StoreSequenceFirst ||
			record.StoreSequenceLast != frame.StoreSequenceLast {
			return nil, fmt.Errorf("recovered frame %d does not match its authenticated ordering record", expectedIndex)
		}
		if registerErr := tracker.Register(frame); registerErr != nil {
			return nil, fmt.Errorf("register recovered frame %d: %w", expectedIndex, registerErr)
		}
		coordinator.pending[expectedIndex] = append([]byte(nil), commit.Encoded...)
	}
	_, _, orderingHead := journal.Head()
	if err := persistState(PrimaryProgress{DurableIndex: index, ConfirmedIndex: confirmed, OrderingHeadMAC: orderingHead, Projection: projection}); err != nil {
		return nil, fmt.Errorf("persist recovered primary member state: %w", err)
	}
	// Authenticate the complete suffix before any network-visible enqueue. A
	// corrupt later record must not leak a verified prefix to peers.
	for _, commit := range recovered {
		if queueErr := outbound.QueueFrame(commit.Index, append([]byte(nil), commit.Encoded...)); queueErr != nil {
			coordinator.noteFrameQueueFailure(commit.Index, queueErr)
		}
	}
	if confirmed > 0 && established {
		if err := coordinator.queueCommitNotice(confirmed); err != nil {
			coordinator.noticeUnavailable = true
			coordinator.unavailable = err
			coordinator.available = false
		}
	}
	return coordinator, nil
}

func (c *PrimaryCoordinator) RecordDurable(frame Frame) (LocalCommit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.poisoned != nil {
		return LocalCommit{}, fmt.Errorf("primary coordinator requires restart after an uncertain durable transition: %w", c.poisoned)
	}
	if c.manualUnavailable != nil {
		return LocalCommit{}, fmt.Errorf("%w: %v", ErrReplicationUnavailable, c.manualUnavailable)
	}
	index, _, _ := c.journal.Head()
	frame.Index = index + 1
	frame.Term = c.term
	frame.ConfirmedIndex = c.confirmed
	frame.ClusterID = c.clusterID
	frame.Incarnation = c.incarnation
	encoded, digest, err := frame.MarshalBinaryAndDigest()
	if err != nil {
		return LocalCommit{}, err
	}
	record := OrderingRecord{
		Kind: frame.Kind, Store: frame.Store, Index: frame.Index, Term: frame.Term,
		ConfirmedIndex: frame.ConfirmedIndex, Metadata: append([]byte(nil), frame.Metadata...),
		StoreGeneration:    frame.StoreGeneration,
		StoreSequenceFirst: frame.StoreSequenceFirst, StoreSequenceLast: frame.StoreSequenceLast,
		FrameDigest: digest,
	}
	persistedRecord, err := c.journal.Append(record)
	if err != nil {
		return LocalCommit{}, err
	}
	if err := c.confirmations.Register(frame); err != nil {
		c.poisoned = err
		return LocalCommit{}, fmt.Errorf("register durable frame: %w", err)
	}
	if err := c.persistState(PrimaryProgress{DurableIndex: frame.Index, ConfirmedIndex: c.confirmed, OrderingHeadMAC: persistedRecord.MAC, Projection: c.projection}); err != nil {
		c.poisoned = err
		c.available = false
		c.unavailable = err
		c.signalChanged()
		return LocalCommit{}, fmt.Errorf("persist primary durable watermark: %w", err)
	}
	c.pending[frame.Index] = append([]byte(nil), encoded...)
	commit := LocalCommit{Index: frame.Index, Encoded: encoded, ReplicationAvailable: c.available}
	if err := c.outbound.QueueFrame(frame.Index, append([]byte(nil), encoded...)); err != nil {
		c.noteFrameQueueFailure(frame.Index, err)
		commit.ReplicationAvailable = false
		c.signalChanged()
	}
	return commit, nil
}

func (c *PrimaryCoordinator) Acknowledge(ack Acknowledgement) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.poisoned != nil {
		return c.confirmed, fmt.Errorf("primary coordinator requires restart after an uncertain durable transition: %w", c.poisoned)
	}
	if c.manualUnavailable != nil {
		return c.confirmed, fmt.Errorf("%w: %v", ErrReplicationUnavailable, c.manualUnavailable)
	}
	confirmed, err := c.confirmations.Acknowledge(ack)
	if err != nil {
		return c.confirmed, err
	}
	advanced := confirmed != c.confirmed
	if advanced {
		durableIndex, _, orderingHead := c.journal.Head()
		projection, projectionErr := c.projectionThrough(confirmed)
		if projectionErr != nil {
			c.poisoned = projectionErr
			c.available = false
			c.unavailable = projectionErr
			c.signalChanged()
			return c.confirmed, fmt.Errorf("reconstruct confirmed primary projection: %w", projectionErr)
		}
		if err := c.persistState(PrimaryProgress{DurableIndex: durableIndex, ConfirmedIndex: confirmed, OrderingHeadMAC: orderingHead, Projection: projection}); err != nil {
			c.poisoned = err
			c.available = false
			c.unavailable = err
			c.signalChanged()
			return c.confirmed, fmt.Errorf("persist confirmed member state: %w", err)
		}
		c.projection = projection
	}
	c.outbound.Acknowledge(ack.NodeID, ack.DurableIndex, ack.AppliedIndex)
	if ack.AppliedIndex > c.peerApplied[ack.NodeID] {
		c.peerApplied[ack.NodeID] = ack.AppliedIndex
	}
	noticeAcknowledged := false
	if c.pendingNoticeIndex > 0 && c.allPeersApplied(c.pendingNoticeIndex) {
		c.pendingNotice = nil
		c.pendingNoticeIndex = 0
		c.noticeUnavailable = false
		noticeAcknowledged = true
	}
	changed := advanced || noticeAcknowledged
	c.confirmed = confirmed
	for index := range c.pending {
		if index <= confirmed {
			delete(c.pending, index)
		}
	}
	if c.failedFrameIndex > 0 && confirmed >= c.failedFrameIndex {
		c.failedFrameIndex = 0
	}
	if advanced {
		if noticeErr := c.queueCommitNotice(confirmed); noticeErr != nil {
			c.noticeUnavailable = true
			c.unavailable = noticeErr
		} else {
			c.noticeUnavailable = false
		}
	}
	c.refreshAvailability()
	if changed {
		c.signalChanged()
	}
	return confirmed, nil
}

// projectionThrough advances only across newly confirmed records. Replaying the
// full ordering prefix for every ACK would make confirmation quadratic.
func (c *PrimaryCoordinator) projectionThrough(confirmed uint64) (ProjectionState, error) {
	projection := c.projection
	for index := c.confirmed + 1; index <= confirmed; index++ {
		record, err := c.journal.Record(index)
		if err != nil {
			return ProjectionState{}, err
		}
		if record.Kind != KindData || record.Store != StoreMetadata {
			continue
		}
		if projection.MetadataEpoch == 0 {
			if record.StoreSequenceFirst != 1 {
				return ProjectionState{}, fmt.Errorf("ordering metadata begins at sequence %d", record.StoreSequenceFirst)
			}
		} else if projection.MetadataEpoch != record.StoreGeneration || projection.MetadataSequence+1 != record.StoreSequenceFirst {
			return ProjectionState{}, errors.New("ordering metadata range does not continue its confirmed cursor")
		}
		projection.MetadataEpoch = record.StoreGeneration
		projection.MetadataSequence = record.StoreSequenceLast
	}
	projection.Index = confirmed
	return projection, nil
}

func (c *PrimaryCoordinator) MarkUnavailable(cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cause == nil {
		cause = ErrReplicationUnavailable
	}
	c.available = false
	c.manualUnavailable = cause
	c.unavailable = cause
	c.signalChanged()
}

// FreezeForStepdown atomically verifies the handoff prefix and prevents every
// later old-term ordering append or ACK advance. A native store hook already
// in flight may fail after its own fsync; startup recovery treats that as an
// unordered tail and removes it before the member can serve again.
func (c *PrimaryCoordinator) FreezeForStepdown(expectedIndex uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.poisoned != nil {
		return fmt.Errorf("primary coordinator requires restart after an uncertain durable transition: %w", c.poisoned)
	}
	head, _, _ := c.journal.Head()
	if head != expectedIndex || c.confirmed != expectedIndex {
		return fmt.Errorf("planned stepdown target index %d does not match Primary durable/confirmed prefix %d/%d", expectedIndex, head, c.confirmed)
	}
	c.manualUnavailable = errors.New("planned stepdown frozen")
	c.available = false
	c.unavailable = c.manualUnavailable
	c.signalChanged()
	return nil
}

func (c *PrimaryCoordinator) MarkAvailable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manualUnavailable = nil
	c.refreshAvailability()
	c.signalChanged()
}

// RetryPending re-enqueues every unconfirmed frame and the latest commit
// notice. It is the only operation, besides an ACK proving delivery, that can
// clear a queue failure; merely observing a reconnected socket is insufficient.
func (c *PrimaryCoordinator) RetryPending() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index := c.confirmed + 1; ; index++ {
		encoded, ok := c.pending[index]
		if !ok {
			break
		}
		if err := c.outbound.QueueFrame(index, append([]byte(nil), encoded...)); err != nil {
			c.noteFrameQueueFailure(index, err)
			c.signalChanged()
			return err
		}
	}
	c.failedFrameIndex = 0
	if len(c.pendingNotice) > 0 {
		if err := c.outbound.QueueCommitNotice(c.pendingNoticeIndex, append([]byte(nil), c.pendingNotice...)); err != nil {
			c.noticeUnavailable = true
			c.unavailable = err
			c.refreshAvailability()
			c.signalChanged()
			return err
		}
		c.noticeUnavailable = false
	}
	c.refreshAvailability()
	c.signalChanged()
	return nil
}

func (c *PrimaryCoordinator) Status() (confirmed uint64, available bool, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.confirmed, c.available, c.unavailable
}

// RequireLedgerConfirmed is the Roll gate: a generation may become a sealed
// structural object only after its final native frame is confirmed.
func (c *PrimaryCoordinator) RequireLedgerConfirmed(generation, lastSequence uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	baseline := c.journal.BaselineCursors()[StoreLedger-StoreLedger]
	if baseline.Generation == generation && baseline.Sequence == lastSequence {
		return nil
	}
	head, _, _ := c.journal.Head()
	for index := head; index > 0; index-- {
		record, err := c.journal.Record(index)
		if err != nil {
			return err
		}
		if record.Kind == KindData && record.Store == StoreLedger && record.StoreGeneration == generation && record.StoreSequenceLast == lastSequence {
			if index > c.confirmed {
				return fmt.Errorf("%w: ledger generation %d ends at unconfirmed index %d", ErrReplicationUnavailable, generation, index)
			}
			return nil
		}
	}
	return errors.New("ledger Roll cursor is absent from the authenticated replication order")
}

func (c *PrimaryCoordinator) PendingFrom(after uint64) []LocalCommit {
	c.mu.Lock()
	defer c.mu.Unlock()
	commits := make([]LocalCommit, 0, len(c.pending))
	for index := after + 1; index <= c.confirmed+uint64(len(c.pending)); index++ {
		encoded, ok := c.pending[index]
		if !ok {
			continue
		}
		commits = append(commits, LocalCommit{Index: index, Encoded: append([]byte(nil), encoded...), ReplicationAvailable: c.available})
	}
	return commits
}

func (c *PrimaryCoordinator) PendingCommitNotice() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.pendingNotice...)
}

func (c *PrimaryCoordinator) WaitConfirmed(ctx context.Context, index uint64) error {
	for {
		c.mu.Lock()
		if index <= c.confirmed {
			c.mu.Unlock()
			return nil
		}
		if !c.available {
			cause := c.unavailable
			c.mu.Unlock()
			return fmt.Errorf("%w: %v", ErrReplicationUnavailable, cause)
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (c *PrimaryCoordinator) signalChanged() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *PrimaryCoordinator) noteFrameQueueFailure(index uint64, cause error) {
	if index > c.failedFrameIndex {
		c.failedFrameIndex = index
	}
	c.available = false
	c.unavailable = cause
}

func (c *PrimaryCoordinator) allPeersApplied(index uint64) bool {
	for _, applied := range c.peerApplied {
		if applied < index {
			return false
		}
	}
	return true
}

func (c *PrimaryCoordinator) queueCommitNotice(confirmed uint64) error {
	notice, err := (CommitNotice{
		ClusterID: c.clusterID, Incarnation: c.incarnation, NodeID: c.nodeID,
		Term: c.term, ConfirmedIndex: confirmed,
	}).MarshalBinary()
	if err != nil {
		return err
	}
	c.pendingNotice = append([]byte(nil), notice...)
	c.pendingNoticeIndex = confirmed
	return c.outbound.QueueCommitNotice(confirmed, append([]byte(nil), notice...))
}

func (c *PrimaryCoordinator) refreshAvailability() {
	c.available = c.failedFrameIndex == 0 && !c.noticeUnavailable && c.manualUnavailable == nil
	if c.available {
		c.unavailable = nil
	}
}

func ReplicationRetryAfter() time.Duration { return time.Second }
