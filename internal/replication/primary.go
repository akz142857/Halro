package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
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
	mu                             sync.Mutex
	clusterID                      string
	incarnation                    string
	nodeID                         string
	term                           uint64
	journal                        *OrderingJournal
	confirmations                  *ConfirmationTracker
	outbound                       OutboundQueue
	persistState                   PrimaryStateWriter
	confirmed                      uint64
	projection                     ProjectionState
	available                      bool
	unavailable                    error
	changed                        chan struct{}
	pending                        map[uint64][]byte
	pendingNotice                  []byte
	pendingNoticeIndex             uint64
	failedFrameIndex               uint64
	peerApplied                    map[string]uint64
	noticeUnavailable              bool
	manualUnavailable              error
	poisoned                       error
	stopped                        bool
	recoveryRevision               uint64
	source                         DurableSource
	windowLimits                   PrimaryWindowLimits
	peerWindows                    [2]primaryPeerWindow
	unconfirmedEntries             []unconfirmedEntry
	recoveryUnconfirmedBytes       int64
	recoveryChargedBytes           int64
	recoveryOverLimit              bool
	unconfirmedAdmissionRefusals   uint64
	lastUnconfirmedProgress        time.Time
	reservedAdmissionSlots         int
	liveUnconfirmedPermit          resourcebudget.Permit
	hasLiveUnconfirmedPermit       bool
	completionUnconfirmedPermit    resourcebudget.Permit
	hasCompletionUnconfirmedPermit bool
	recoveryUnconfirmedPermit      resourcebudget.Permit
	hasRecoveryUnconfirmedPermit   bool
	orphanAdmissions               []UnconfirmedAdmission
}

// NewPrimaryCoordinator accepts the exact encoded frames for any authenticated
// ordering suffix above confirmed. Callers reconstruct those bytes from the
// four stores during startup; omitting a non-empty suffix fails closed instead
// of either losing it or pretending it was confirmed.
func NewPrimaryCoordinator(clusterID, incarnation, nodeID string, term, confirmed uint64, peers []string, journal *OrderingJournal, outbound OutboundQueue, persistState PrimaryStateWriter, recovered ...LocalCommit) (*PrimaryCoordinator, error) {
	return NewPrimaryCoordinatorStreaming(clusterID, incarnation, nodeID, term, confirmed, peers, journal, outbound, persistState,
		func(consume func(LocalCommit) error) error {
			for _, commit := range recovered {
				if err := consume(commit); err != nil {
					return err
				}
			}
			return nil
		})
}

// RecoveredCommitStream synchronously emits an exact authenticated suffix in
// index order. A successful consumer return releases its borrow of the bytes.
// The producer cannot retain the callback or invoke it concurrently.
// Reconciliation must finish before construction; the ordering head stays fixed.
type RecoveredCommitStream func(consume func(LocalCommit) error) error

// NewPrimaryCoordinatorStreaming validates a complete recovered suffix before
// member-state publication or network enqueue. It avoids a second full input
// slice; coordinator payload retention is a separate remaining migration.
func NewPrimaryCoordinatorStreaming(clusterID, incarnation, nodeID string, term, confirmed uint64, peers []string, journal *OrderingJournal, outbound OutboundQueue, persistState PrimaryStateWriter, recover RecoveredCommitStream) (*PrimaryCoordinator, error) {
	return newPrimaryCoordinator(clusterID, incarnation, nodeID, term, confirmed, peers, journal, outbound, persistState, recover, nil, DefaultPrimaryWindowLimits())
}

func newPrimaryCoordinator(clusterID, incarnation, nodeID string, term, confirmed uint64, peers []string, journal *OrderingJournal, outbound OutboundQueue, persistState PrimaryStateWriter, recover RecoveredCommitStream, source DurableSource, limits PrimaryWindowLimits) (*PrimaryCoordinator, error) {
	if journal == nil || outbound == nil || persistState == nil {
		return nil, errors.New("primary coordinator requires an ordering journal, outbound queue and state writer")
	}
	if recover == nil {
		return nil, errors.New("primary coordinator requires a recovered suffix stream")
	}
	if len(nodeID) == 0 || len(nodeID) > MaxIdentityBytes {
		return nil, errors.New("primary coordinator node identity is invalid")
	}
	index, journalTerm, initialHead := journal.Head()
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
		source: source, windowLimits: limits,
	}
	if source != nil {
		coordinator.pending = nil
	}
	for i, peer := range peers {
		coordinator.peerApplied[peer] = 0
		coordinator.peerWindows[i].node = peer
	}
	recoveryAdmissionTransferred := false
	defer func() {
		if recoveryAdmissionTransferred {
			return
		}
		if coordinator.hasRecoveryUnconfirmedPermit {
			_ = coordinator.recoveryUnconfirmedPermit.Release()
			coordinator.hasRecoveryUnconfirmedPermit = false
		}
	}()
	var recoveredUnconfirmed []unconfirmedEntry
	var recoveredUnconfirmedBytes int64
	if limits.UnconfirmedBudget != nil {
		if index-confirmed > uint64(maxUnconfirmedEntries) {
			return nil, fmt.Errorf("recovered unconfirmed suffix has %d entries, exceeding fixed bounded recovery table %d", index-confirmed, maxUnconfirmedEntries)
		}
		permit, acquireErr := limits.UnconfirmedBudget.TryAcquireRecovery(resourcebudget.Cost{RAM: limits.UnconfirmedMetadataRAMBytes})
		if acquireErr != nil {
			return nil, fmt.Errorf("admit bounded Primary unconfirmed release table: %w", acquireErr)
		}
		coordinator.recoveryUnconfirmedPermit = permit
		coordinator.hasRecoveryUnconfirmedPermit = true
		recoveredUnconfirmed = make([]unconfirmedEntry, 0, maxUnconfirmedEntries)
	}
	count := uint64(0)
	var consumerFailure error
	err = recover(func(commit LocalCommit) (consumeErr error) {
		if consumerFailure != nil {
			return consumerFailure
		}
		defer func() {
			if consumeErr != nil {
				consumerFailure = consumeErr
			}
		}()
		if count >= index-confirmed {
			return errors.New("recovered suffix exceeds the ordering head")
		}
		expectedIndex := confirmed + count + 1
		if commit.Index != expectedIndex {
			return fmt.Errorf("recovered frame index %d does not continue %d", commit.Index, expectedIndex-1)
		}
		frame, digest, decodeErr := UnmarshalFrameAndDigest(commit.Encoded)
		if decodeErr != nil {
			return fmt.Errorf("decode recovered frame %d: %w", commit.Index, decodeErr)
		}
		record, recordErr := journal.Record(expectedIndex)
		if recordErr != nil {
			return recordErr
		}
		if frame.Index != expectedIndex || frame.ClusterID != clusterID || frame.Incarnation != incarnation || frame.Term != term ||
			record.Term != frame.Term || record.ConfirmedIndex != frame.ConfirmedIndex || !bytes.Equal(record.Metadata, frame.Metadata) ||
			record.FrameDigest != digest || record.Kind != frame.Kind || record.Store != frame.Store ||
			record.StoreGeneration != frame.StoreGeneration || record.StoreSequenceFirst != frame.StoreSequenceFirst ||
			record.StoreSequenceLast != frame.StoreSequenceLast {
			return fmt.Errorf("recovered frame %d does not match its authenticated ordering record", expectedIndex)
		}
		if limits.UnconfirmedBudget != nil {
			frameBytes := int64(len(commit.Encoded))
			if frameBytes <= 0 || frameBytes > int64(^uint64(0)>>1)-recoveredUnconfirmedBytes {
				return errors.New("recovered unconfirmed byte count overflows")
			}
			recoveredUnconfirmedBytes += frameBytes
			recoveredUnconfirmed = append(recoveredUnconfirmed, unconfirmedEntry{index: expectedIndex, bytes: frameBytes, recovery: true})
		}
		if registerErr := tracker.Register(frame); registerErr != nil {
			return fmt.Errorf("register recovered frame %d: %w", expectedIndex, registerErr)
		}
		if source == nil {
			coordinator.pending[expectedIndex] = append([]byte(nil), commit.Encoded...)
		}
		count++
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("recover primary suffix: %w", err)
	}
	if consumerFailure != nil {
		return nil, fmt.Errorf("recover primary suffix consumer: %w", consumerFailure)
	}
	if count != index-confirmed {
		return nil, fmt.Errorf("primary coordinator needs %d recovered suffix frames, got %d", index-confirmed, count)
	}
	currentIndex, currentTerm, orderingHead := journal.Head()
	if currentIndex != index || currentTerm != journalTerm || orderingHead != initialHead {
		return nil, errors.New("ordering head changed during primary suffix recovery")
	}
	if limits.UnconfirmedBudget != nil {
		if recoveredUnconfirmedBytes > 0 {
			chargedBytes := min(recoveredUnconfirmedBytes, limits.RecoveryReserveUnconfirmedBytes)
			permit, acquireErr := limits.UnconfirmedBudget.TryAcquireRecovery(resourcebudget.Cost{Unconfirmed: chargedBytes})
			if acquireErr != nil {
				return nil, fmt.Errorf("admit bounded Primary recovered suffix: %w", acquireErr)
			}
			merged, mergeErr := coordinator.recoveryUnconfirmedPermit.Merge(permit)
			if mergeErr != nil {
				_ = permit.Release()
				return nil, fmt.Errorf("merge bounded Primary recovery owner: %w", mergeErr)
			}
			coordinator.recoveryUnconfirmedPermit = merged
			coordinator.recoveryChargedBytes = chargedBytes
			for i := range recoveredUnconfirmed {
				recoveredUnconfirmed[i].chargedBytes = min(recoveredUnconfirmed[i].bytes, chargedBytes)
				chargedBytes -= recoveredUnconfirmed[i].chargedBytes
			}
		}
		coordinator.unconfirmedEntries = recoveredUnconfirmed
		coordinator.recoveryUnconfirmedBytes = recoveredUnconfirmedBytes
		coordinator.recoveryOverLimit = len(recoveredUnconfirmed) > limits.MaxUnconfirmedEntries || recoveredUnconfirmedBytes > limits.RecoveryReserveUnconfirmedBytes
	}
	if err := persistState(PrimaryProgress{DurableIndex: index, ConfirmedIndex: confirmed, OrderingHeadMAC: orderingHead, Projection: projection}); err != nil {
		return nil, fmt.Errorf("persist recovered primary member state: %w", err)
	}
	if limits.UnconfirmedBudget != nil {
		coordinator.lastUnconfirmedProgress = time.Now()
	}
	if source != nil {
		coordinator.available = false
		coordinator.unavailable = ErrPeerNotConnected
		if confirmed > 0 && established {
			if err := coordinator.setCommitNotice(confirmed); err != nil {
				return nil, err
			}
		}
		recoveryAdmissionTransferred = true
		return coordinator, nil
	}
	// Authenticate the complete suffix before any network-visible enqueue. A
	// corrupt later record must not leak a verified prefix to peers.
	for offset := uint64(0); offset < count; offset++ {
		commitIndex := confirmed + offset + 1
		if queueErr := outbound.QueueFrame(commitIndex, append([]byte(nil), coordinator.pending[commitIndex]...)); queueErr != nil {
			coordinator.noteFrameQueueFailure(commitIndex, queueErr)
		}
	}
	if confirmed > 0 && established {
		if err := coordinator.queueCommitNotice(confirmed); err != nil {
			coordinator.noticeUnavailable = true
			coordinator.unavailable = err
			coordinator.available = false
		}
	}
	recoveryAdmissionTransferred = true
	return coordinator, nil
}

func (c *PrimaryCoordinator) RecordDurable(frame Frame) (LocalCommit, error) {
	return c.RecordDurableWithAdmission(frame, nil)
}

// RecordDurableWithAdmission adopts a pre-append admission before any ordering
// transition can become uncertain. It is retained through ordering, member
// state, transport and ACK failures, and released only after confirmed state
// has been durably published.
func (c *PrimaryCoordinator) RecordDurableWithAdmission(frame Frame, admission UnconfirmedAdmission) (LocalCommit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	admissionOwned := false
	defer func() {
		if admission != nil && !admissionOwned {
			if c.stopped {
				if owned, ok := admission.(*primaryUnconfirmedAdmission); ok {
					_ = owned.releaseLocked()
					return
				}
			}
			c.orphanAdmissions = append(c.orphanAdmissions, admission)
		}
	}()
	defer func() {
		if c.source != nil && c.poisoned != nil {
			c.closeSourceWindows()
		}
	}()
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
	if admission != nil {
		if err := c.adoptUnconfirmedAdmissionLocked(frame.Index, admission); err != nil {
			return LocalCommit{}, err
		}
		admissionOwned = true
	}
	if frame.Kind == KindMetadataBaseline {
		owner, ok := c.source.(metadataBaselineSource)
		if !ok || frame.ConfirmedIndex != index {
			return LocalCommit{}, errors.New("Metadata baseline requires a fully confirmed real source owner")
		}
		if err := owner.ValidateMetadataBaselineOrdering(c.journal, OrderingRecord{Kind: frame.Kind, Index: frame.Index, Term: frame.Term, ConfirmedIndex: frame.ConfirmedIndex, Metadata: frame.Metadata}); err != nil {
			return LocalCommit{}, err
		}
	}
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
	var physical *OrderingPhysicalReservation
	if owned, ok := admission.(*primaryUnconfirmedAdmission); ok {
		physical = owned.physical
	}
	persistedRecord, err := c.journal.AppendWithPhysicalReservation(record, physical)
	if err != nil {
		// A failed ordering fsync leaves an uncertain durable transition.
		// Propagate the journal's terminal state and wake prefix waiters;
		// merely returning the append error strands an already waiting recovery.
		if cause := c.journal.appendUncertainty(); cause != nil {
			c.poisoned = cause
			c.available = false
			c.unavailable = cause
		}
		c.signalChanged()
		return LocalCommit{}, err
	}
	if frame.Kind == KindMetadataBaseline {
		if err := c.source.(metadataBaselineSource).ActivateMetadataBaseline(c.journal, frame.Index); err != nil {
			c.poisoned = err
			c.signalChanged()
			return LocalCommit{}, fmt.Errorf("activate Primary Metadata ordering baseline: %w", err)
		}
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
	if c.source != nil {
		c.pumpSourceWindows()
		c.signalChanged()
		return LocalCommit{Index: frame.Index, Encoded: encoded, ReplicationAvailable: c.available}, nil
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
	defer func() {
		if c.source != nil && c.poisoned != nil {
			c.closeSourceWindows()
		}
	}()
	if c.poisoned != nil {
		return c.confirmed, fmt.Errorf("primary coordinator requires restart after an uncertain durable transition: %w", c.poisoned)
	}
	if c.manualUnavailable != nil {
		return c.confirmed, fmt.Errorf("%w: %v", ErrReplicationUnavailable, c.manualUnavailable)
	}
	if c.source != nil {
		peer := c.windowFor(ack.NodeID)
		if peer == nil || !peer.ready || ack.DurableIndex > peer.queuedThrough {
			return c.confirmed, errors.New("acknowledgement exceeds its active peer send window")
		}
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
		c.lastUnconfirmedProgress = time.Now()
	}
	c.outbound.Acknowledge(ack.NodeID, ack.DurableIndex, ack.AppliedIndex)
	appliedAdvanced := ack.AppliedIndex > c.peerApplied[ack.NodeID]
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
	changed := advanced || appliedAdvanced || noticeAcknowledged
	c.confirmed = confirmed
	if advanced {
		for len(c.unconfirmedEntries) > 0 && c.unconfirmedEntries[0].index <= confirmed {
			entry := c.unconfirmedEntries[0]
			if err := c.releaseUnconfirmedEntryLocked(entry); err != nil {
				c.poisoned = fmt.Errorf("release confirmed unconfirmed admission at index %d: %w", entry.index, err)
				c.available = false
				c.unavailable = c.poisoned
				c.signalChanged()
				return confirmed, c.poisoned
			}
			if entry.recovery {
				c.recoveryUnconfirmedBytes -= entry.bytes
				c.recoveryChargedBytes -= entry.chargedBytes
			}
			c.unconfirmedEntries = c.unconfirmedEntries[1:]
		}
		if err := c.refreshRecoveredAdmissionLocked(); err != nil {
			c.poisoned = fmt.Errorf("refresh recovered unconfirmed admission: %w", err)
			c.available = false
			c.unavailable = c.poisoned
			c.signalChanged()
			return confirmed, c.poisoned
		}
	}
	if c.source != nil {
		peer := c.windowFor(ack.NodeID)
		c.releasePeerWindow(peer, ack.DurableIndex)
		if advanced {
			if err := c.setCommitNotice(confirmed); err != nil {
				c.poisoned = err
				return confirmed, err
			}
			if c.allPeersApplied(confirmed) {
				c.pendingNotice = nil
				c.pendingNoticeIndex = 0
			}
		}
		c.pumpSourceWindows()
		c.signalChanged()
		if c.poisoned != nil {
			return confirmed, c.poisoned
		}
		if !peer.ready {
			return confirmed, peer.failure
		}
		return confirmed, nil
	}
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

// refreshRecoveredAdmissionLocked transitions an over-limit recovered suffix
// back to normal append admission only after its remaining bytes and entries
// fit configuration and the remaining suffix is fully charged again.
func (c *PrimaryCoordinator) refreshRecoveredAdmissionLocked() error {
	if !c.recoveryOverLimit || len(c.unconfirmedEntries) > c.windowLimits.MaxUnconfirmedEntries || c.recoveryUnconfirmedBytes > c.windowLimits.RecoveryReserveUnconfirmedBytes {
		return nil
	}
	missingCharge := c.recoveryUnconfirmedBytes - c.recoveryChargedBytes
	if missingCharge > 0 {
		permit, err := c.windowLimits.UnconfirmedBudget.TryAcquireRecovery(resourcebudget.Cost{Unconfirmed: missingCharge})
		if err != nil {
			return nil // Keep retransmission available; appends remain closed until a later ACK can retry.
		}
		merged, err := c.recoveryUnconfirmedPermit.Merge(permit)
		if err != nil {
			_ = permit.Release()
			return fmt.Errorf("merge promoted Primary recovery owner: %w", err)
		}
		c.recoveryUnconfirmedPermit = merged
		c.recoveryChargedBytes += missingCharge
		for i := range c.unconfirmedEntries {
			entry := &c.unconfirmedEntries[i]
			if entry.recovery {
				entry.chargedBytes = entry.bytes
			}
		}
	}
	c.recoveryOverLimit = false
	return nil
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
		if record.Kind == KindMetadataBaseline {
			boundary, err := DecodeMetadataBaselineMetadata(record.Metadata)
			if err != nil || projection.MetadataEpoch != boundary.PreviousEpoch || projection.MetadataSequence != boundary.PreviousSequence {
				return ProjectionState{}, errors.Join(errors.New("confirmed Metadata baseline does not continue its permitted prefix"), err)
			}
			projection.MetadataEpoch, projection.MetadataSequence = boundary.Epoch, 0
			continue
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
	c.recoveryRevision++
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
	c.recoveryRevision++
	c.manualUnavailable = errors.New("planned stepdown frozen")
	c.available = false
	c.unavailable = c.manualUnavailable
	c.signalChanged()
	return nil
}

func (c *PrimaryCoordinator) MarkAvailable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.recoveryRevision++
	c.manualUnavailable = nil
	c.refreshAvailability()
	c.signalChanged()
}

// RetryPending fills only available source-backed window space. Reconnection
// requires PreparePeer for the new concrete session; seeing a socket is not
// enough. Legacy coordinators re-enqueue their retained suffix and notice.
func (c *PrimaryCoordinator) RetryPending() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.source != nil {
		if c.poisoned != nil || c.manualUnavailable != nil {
			return ErrReplicationUnavailable
		}
		// Reconnection calls PreparePeer, which resets that session's cursor.
		// Retrying never duplicates the complete outstanding suffix in RAM.
		c.pumpSourceWindows()
		c.signalChanged()
		if !c.available {
			return c.unavailable
		}
		return nil
	}
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

type UnconfirmedAdmissionStatus struct {
	Entries           int
	RecoveryBytes     int64
	RecoveryCharged   int64
	OverLimit         bool
	AdmissionRefusals uint64
	LastProgress      time.Time
}

func (c *PrimaryCoordinator) UnconfirmedAdmissionStatus() UnconfirmedAdmissionStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return UnconfirmedAdmissionStatus{
		Entries:           len(c.unconfirmedEntries) + c.reservedAdmissionSlots,
		RecoveryBytes:     c.recoveryUnconfirmedBytes,
		RecoveryCharged:   c.recoveryChargedBytes,
		OverLimit:         c.recoveryOverLimit,
		AdmissionRefusals: c.unconfirmedAdmissionRefusals,
		LastProgress:      c.lastUnconfirmedProgress,
	}
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

// PendingFrom returns only one bounded page of unconfirmed frames. Read errors
// cannot masquerade as an empty suffix; continue from the last returned index.
func (c *PrimaryCoordinator) PendingFrom(after uint64) ([]LocalCommit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, ErrReplicationUnavailable
	}
	head, _, _ := c.journal.Head()
	after = max(after, c.confirmed)
	if after >= head {
		return nil, nil
	}
	var commits []LocalCommit
	used := 0
	for index := after + 1; index <= head && len(commits) < c.windowLimits.MaxFrames; index++ {
		var commit LocalCommit
		if c.source != nil {
			var err error
			commit, err = c.readSourceCommit(index)
			if err != nil {
				c.poisoned = fmt.Errorf("ordered native source cannot be reconstructed: %w", err)
				c.closeSourceWindows()
				c.refreshSourceAvailability()
				c.signalChanged()
				return nil, err
			}
		} else {
			encoded, ok := c.pending[index]
			if !ok {
				return nil, errors.New("pending frame is missing")
			}
			commit = LocalCommit{Index: index, Encoded: encoded}
		}
		if len(commit.Encoded) > c.windowLimits.MaxBytes-used {
			break
		}
		used += len(commit.Encoded)
		if c.source == nil {
			commit.Encoded = append([]byte(nil), commit.Encoded...)
		}
		commit.ReplicationAvailable = c.available
		commits = append(commits, commit)
		if index == head {
			break
		}
	}
	return commits, nil
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
	if err := c.setCommitNotice(confirmed); err != nil {
		return err
	}
	return c.outbound.QueueCommitNotice(confirmed, append([]byte(nil), c.pendingNotice...))
}

func (c *PrimaryCoordinator) setCommitNotice(confirmed uint64) error {
	notice, err := (CommitNotice{
		ClusterID: c.clusterID, Incarnation: c.incarnation, NodeID: c.nodeID,
		Term: c.term, ConfirmedIndex: confirmed,
	}).MarshalBinary()
	if err != nil {
		return err
	}
	c.pendingNotice = append([]byte(nil), notice...)
	c.pendingNoticeIndex = confirmed
	return nil
}

func (c *PrimaryCoordinator) refreshAvailability() {
	if c.source != nil {
		c.refreshSourceAvailability()
		return
	}
	c.available = c.failedFrameIndex == 0 && !c.noticeUnavailable && c.manualUnavailable == nil && !c.recoveryOverLimit
	if c.available {
		c.unavailable = nil
	} else if c.recoveryOverLimit {
		c.unavailable = ErrUnconfirmedRecoveryOverLimit
	}
}

func ReplicationRetryAfter() time.Duration { return time.Second }
