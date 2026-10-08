package replication

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// These are local queue bounds, not calibrated production capacity promises.
// Shared budget ownership and unconfirmed native-disk admission remain separate.
const maxPrimaryWindowFrames = 64
const maxUnconfirmedEntries = replicationlimits.MaxPrimaryUnconfirmedEntries
const maxEncodedPrimaryFrame = replicationlimits.MaxEncodedFrameBytes
const maxEncodedCommitNotice = 4 + commitNoticeFixedBytes + 3*MaxIdentityBytes

var ErrUnconfirmedRecoveryOverLimit = errors.New("primary recovered unconfirmed suffix exceeds configured append limits")

func (c *PrimaryCoordinator) noteUnconfirmedAdmissionRefusalLocked() {
	if c.unconfirmedAdmissionRefusals < math.MaxUint64 {
		c.unconfirmedAdmissionRefusals++
	}
}

// This only rejects obviously undersized reconstruction envelopes. Native
// decoding/JSON amplification still requires measured deployment calibration.
const MinPrimaryCandidateRAMBytes int64 = replicationlimits.MinCandidateRAMBytes

type PrimaryWindowLimits struct {
	MaxFrames, MaxBytes             int
	CandidateBudget                 *resourcebudget.Manager
	CandidateRAMBytes               int64
	UnconfirmedBudget               *resourcebudget.Manager
	MaxUnconfirmedEntries           int
	UnconfirmedMetadataRAMBytes     int64
	RecoveryReserveUnconfirmedBytes int64
}

// UnconfirmedAdmission is acquired before a native source append and remains
// owned until its ordering index is included in a durably persisted confirmed
// prefix.
type UnconfirmedAdmission interface {
	Release() error
}

type primaryUnconfirmedAdmission struct {
	coordinator *PrimaryCoordinator
	physical    *OrderingPhysicalReservation
	permit      resourcebudget.Permit
	bytes       int64
	slot        bool
	moved       bool
	released    bool
	completion  bool
	window      *TerminalWindow
}

func (a *primaryUnconfirmedAdmission) Release() error {
	if a == nil || a.coordinator == nil {
		return nil
	}
	c := a.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	return a.releaseLocked()
}

func (a *primaryUnconfirmedAdmission) releaseLocked() error {
	if a.moved || a.released {
		return nil
	}
	c := a.coordinator
	if a.slot {
		c.reservedAdmissionSlots--
		a.slot = false
	}
	if err := a.physical.Release(); err != nil {
		return err
	}
	err := a.permit.Release()
	if err == nil {
		a.released = true
		a.permit = resourcebudget.Permit{}
		a.detachWindowLocked()
	}
	return err
}

type unconfirmedEntry struct {
	physical     *OrderingPhysicalReservation
	index        uint64
	bytes        int64
	chargedBytes int64
	recovery     bool
	completion   bool
}

func (c *PrimaryCoordinator) adoptUnconfirmedAdmissionLocked(index uint64, admission UnconfirmedAdmission) error {
	if admission == nil {
		return nil
	}
	a, ok := admission.(*primaryUnconfirmedAdmission)
	if !ok || a.coordinator != c || !a.slot || a.moved || a.released || a.bytes <= 0 || a.permit == (resourcebudget.Permit{}) {
		return errors.New("invalid or foreign Primary unconfirmed admission")
	}
	if len(c.unconfirmedEntries) >= c.windowLimits.MaxUnconfirmedEntries || c.reservedAdmissionSlots <= 0 {
		return resourcebudget.ErrExhausted
	}
	if len(c.unconfirmedEntries) > 0 && c.unconfirmedEntries[len(c.unconfirmedEntries)-1].index >= index {
		return errors.New("Primary unconfirmed admissions are not in ordering-index order")
	}
	permit, hasPermit := &c.liveUnconfirmedPermit, &c.hasLiveUnconfirmedPermit
	if a.completion {
		permit, hasPermit = &c.completionUnconfirmedPermit, &c.hasCompletionUnconfirmedPermit
	}
	if *hasPermit {
		merged, err := permit.Merge(a.permit)
		if err != nil {
			return fmt.Errorf("merge Primary unconfirmed owner: %w", err)
		}
		*permit = merged
	} else {
		*permit, *hasPermit = a.permit, true
	}
	a.permit = resourcebudget.Permit{}
	a.slot, a.moved = false, true
	c.reservedAdmissionSlots--
	c.unconfirmedEntries = append(c.unconfirmedEntries, unconfirmedEntry{index: index, bytes: a.bytes, chargedBytes: a.bytes, completion: a.completion, physical: a.physical})
	a.detachWindowLocked()
	return nil
}

func (c *PrimaryCoordinator) releaseUnconfirmedEntryLocked(entry unconfirmedEntry) error {
	permit := &c.liveUnconfirmedPermit
	hasPermit := &c.hasLiveUnconfirmedPermit
	if entry.recovery {
		permit = &c.recoveryUnconfirmedPermit
		hasPermit = &c.hasRecoveryUnconfirmedPermit
	}
	if entry.completion {
		permit, hasPermit = &c.completionUnconfirmedPermit, &c.hasCompletionUnconfirmedPermit
	}
	if entry.chargedBytes == 0 {
		return nil
	}
	if !*hasPermit {
		return errors.New("Primary unconfirmed aggregate owner is missing")
	}
	if err := permit.ReleasePart(resourcebudget.Cost{Unconfirmed: entry.chargedBytes}); err != nil {
		return err
	}
	cost, err := permit.Cost()
	if err != nil {
		return err
	}
	if cost == (resourcebudget.Cost{}) {
		if err := permit.Release(); err != nil {
			return err
		}
		*permit = resourcebudget.Permit{}
		*hasPermit = false
	}
	return nil
}

func DefaultPrimaryWindowLimits() PrimaryWindowLimits {
	return PrimaryWindowLimits{MaxFrames: 16, MaxBytes: 32 << 20}
}

func (l PrimaryWindowLimits) validate() error {
	if l.MaxFrames < 1 || l.MaxFrames > maxPrimaryWindowFrames || l.MaxBytes < maxEncodedPrimaryFrame || l.MaxBytes > 1<<30 {
		return errors.New("primary window requires 1-64 frames and bytes sufficient for a maximum wire frame")
	}
	if l.CandidateBudget != nil && l.CandidateRAMBytes < MinPrimaryCandidateRAMBytes || l.CandidateBudget == nil && l.CandidateRAMBytes != 0 {
		return errors.New("primary candidate RAM requires a shared manager and a calibrated envelope covering the reconstruction lower bound")
	}
	if l.UnconfirmedBudget != nil && (l.MaxUnconfirmedEntries < 1 || l.MaxUnconfirmedEntries > maxUnconfirmedEntries || l.UnconfirmedMetadataRAMBytes < replicationlimits.PrimaryUnconfirmedTableRAMBytes || l.RecoveryReserveUnconfirmedBytes <= 0) || l.UnconfirmedBudget == nil && (l.MaxUnconfirmedEntries != 0 || l.UnconfirmedMetadataRAMBytes != 0 || l.RecoveryReserveUnconfirmedBytes != 0) {
		return fmt.Errorf("primary unconfirmed admission requires a shared manager and 1-%d bounded entries", maxUnconfirmedEntries)
	}
	return nil
}

func (l PrimaryWindowLimits) acquireCandidate() (func(), error) {
	if l.CandidateBudget == nil {
		return nil, nil
	}
	permit, err := l.CandidateBudget.TryAcquireRecovery(resourcebudget.Cost{RAM: l.CandidateRAMBytes})
	if err != nil {
		return nil, fmt.Errorf("primary candidate RAM admission: %w", err)
	}
	return func() { _ = permit.Release() }, nil
}

// ReserveUnconfirmed is called by native stores before their durable append.
// A nil admission means the feature is unconfigured; callers must not interpret
// that as a configured zero-capacity budget.
func (c *PrimaryCoordinator) ReserveUnconfirmed(bytes int64) (UnconfirmedAdmission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.poisoned != nil || c.manualUnavailable != nil {
		return nil, ErrReplicationUnavailable
	}
	if c.windowLimits.UnconfirmedBudget == nil {
		if err := c.journal.requirePhysicalAdmissionBudget(false); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if c.recoveryOverLimit {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, resourcebudget.ErrExhausted
	}
	if bytes <= 0 {
		return nil, errors.New("unconfirmed admission requires positive encoded bytes")
	}
	if len(c.unconfirmedEntries)+c.reservedAdmissionSlots >= c.windowLimits.MaxUnconfirmedEntries {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, resourcebudget.ErrExhausted
	}
	permit, err := c.windowLimits.UnconfirmedBudget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{Unconfirmed: bytes})
	if err != nil {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, fmt.Errorf("primary unconfirmed byte admission: %w", err)
	}
	physical, err := c.journal.ReservePhysicalAppend(c.term)
	if err != nil {
		return nil, errors.Join(err, permit.Release())
	}
	c.reservedAdmissionSlots++
	return &primaryUnconfirmedAdmission{coordinator: c, physical: physical, permit: permit, bytes: bytes, slot: true}, nil
}

// ReserveNativeSourceUnconfirmed charges the exact encoded replication frame
// size that startup source recovery will charge for this native data batch.
func (c *PrimaryCoordinator) ReserveNativeSourceUnconfirmed(sourceBytes int) (UnconfirmedAdmission, error) {
	if sourceBytes <= 0 {
		return nil, errors.New("native unconfirmed admission requires encoded source bytes")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.poisoned != nil || c.manualUnavailable != nil {
		return nil, ErrReplicationUnavailable
	}
	if c.windowLimits.UnconfirmedBudget == nil {
		if err := c.journal.requirePhysicalAdmissionBudget(false); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if c.recoveryOverLimit {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, resourcebudget.ErrExhausted
	}
	if sourceBytes > MaxPayloadBytes {
		return nil, fmt.Errorf("native replication batch exceeds the %d-byte frame payload limit", MaxPayloadBytes)
	}
	return c.reserveReplicationFrameLocked(sourceBytes, 0)
}

// ReserveReplicationFrame admits an already-sized control/data frame before
// its underlying object or ordering fact is written.
func (c *PrimaryCoordinator) ReserveReplicationFrame(payloadBytes, metadataBytes int) (UnconfirmedAdmission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.poisoned != nil || c.manualUnavailable != nil {
		return nil, ErrReplicationUnavailable
	}
	if c.windowLimits.UnconfirmedBudget == nil {
		if err := c.journal.requirePhysicalAdmissionBudget(false); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if c.recoveryOverLimit {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, resourcebudget.ErrExhausted
	}
	if payloadBytes < 0 || payloadBytes > MaxPayloadBytes || metadataBytes < 0 || metadataBytes > MaxMetadataBytes {
		return nil, errors.New("replication admission frame dimensions exceed protocol limits")
	}
	return c.reserveReplicationFrameLocked(payloadBytes, metadataBytes)
}

// ReserveProviderObjectAdmissions reserves every encoded chunk before the
// encrypted object source file is published. The returned slice is bounded by
// MaxUnconfirmedEntries and is empty when this budget is unconfigured.
func (c *PrimaryCoordinator) ReserveProviderObjectAdmissions(name string, sealed []byte) ([]UnconfirmedAdmission, error) {
	digest := sha256.Sum256(sealed)
	total := uint64(len(sealed))
	admissions := make([]UnconfirmedAdmission, 0)
	for offset := uint64(0); offset < total || offset == 0; {
		remaining := total - offset
		chunkLength := remaining
		if chunkLength > MaxPayloadBytes {
			chunkLength = MaxPayloadBytes
		}
		metadata, err := (ProviderObjectMetadata{
			Name: name, Offset: offset, ChunkLength: chunkLength, TotalLength: total,
			Digest: digest, Final: offset+chunkLength == total,
		}).MarshalBinary()
		if err != nil {
			for _, admission := range admissions {
				_ = admission.Release()
			}
			return nil, err
		}
		admission, err := c.ReserveReplicationFrame(int(chunkLength), len(metadata))
		if err != nil {
			for _, reserved := range admissions {
				_ = reserved.Release()
			}
			return nil, err
		}
		if admission == nil {
			return nil, nil
		}
		admissions = append(admissions, admission)
		offset += chunkLength
		if total == 0 {
			break
		}
	}
	return admissions, nil
}

// RetainUnconfirmedAdmissions keeps a pre-reserved but not yet ordered object
// charged when source publication or a later chunk becomes uncertain.
func (c *PrimaryCoordinator) RetainUnconfirmedAdmissions(admissions []UnconfirmedAdmission) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, admission := range admissions {
		if admission != nil {
			if c.stopped {
				if owned, ok := admission.(*primaryUnconfirmedAdmission); ok {
					_ = owned.releaseLocked()
				}
				continue
			}
			c.orphanAdmissions = append(c.orphanAdmissions, admission)
		}
	}
}

func (c *PrimaryCoordinator) reserveReplicationFrameLocked(payloadBytes, metadataBytes int) (UnconfirmedAdmission, error) {
	if c.windowLimits.UnconfirmedBudget == nil {
		if err := c.journal.requirePhysicalAdmissionBudget(false); err != nil {
			return nil, err
		}
		return nil, nil
	}
	wireBytes := int64(4 + frameBodyFixedBytes + len(c.clusterID) + len(c.incarnation) + metadataBytes + payloadBytes)
	if len(c.unconfirmedEntries)+c.reservedAdmissionSlots >= c.windowLimits.MaxUnconfirmedEntries {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, resourcebudget.ErrExhausted
	}
	permit, err := c.windowLimits.UnconfirmedBudget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{Unconfirmed: wireBytes})
	if err != nil {
		c.noteUnconfirmedAdmissionRefusalLocked()
		return nil, fmt.Errorf("primary unconfirmed byte admission: %w", err)
	}
	physical, err := c.journal.ReservePhysicalAppend(c.term)
	if err != nil {
		return nil, errors.Join(err, permit.Release())
	}
	c.reservedAdmissionSlots++
	return &primaryUnconfirmedAdmission{coordinator: c, physical: physical, permit: permit, bytes: wireBytes, slot: true}, nil
}

// ValidateSession reserves space for a commit notice in the initial pre-Run
// batch. Without this reserve a full data window can prevent Run from ever
// starting. Later socket backpressure still fails the session normally.
func (l PrimaryWindowLimits) ValidateSession(options PeerSessionOptions) error {
	if err := l.validate(); err != nil {
		return err
	}
	session, err := options.normalized()
	if err != nil {
		return err
	}
	if l.MaxFrames+1 > session.QueueCapacity || int64(l.MaxBytes)+maxEncodedCommitNotice > session.MaxQueuedBytes {
		return errors.New("primary window must leave one commit-notice record and its bytes in the session queue")
	}
	return nil
}

type primaryPeerWindow struct {
	node                                  string
	ready                                 bool
	durable, queuedThrough, noticeThrough uint64
	bytes                                 int
	sizes                                 [maxPrimaryWindowFrames]int
	failure                               error
	send                                  func([]byte) error
	reader                                FrameSource
	releaseReader                         func()
	candidate                             LocalCommit
	candidateRevision                     *sourceRevision
	releaseCandidate                      func()
}

// Production NativeSource reserves at most two independent, read-only cursor
// sets. Other sources may be intrinsically seekable and use their own Read.
type sourceReaderFactory interface {
	acquireRangeReader() (FrameSource, func(), error)
}

func (peer *primaryPeerWindow) closeReader() {
	peer.clearCandidate()
	if peer.releaseReader != nil {
		peer.releaseReader()
	}
	peer.reader, peer.releaseReader = nil, nil
}

func (peer *primaryPeerWindow) clearCandidate() {
	peer.candidate, peer.candidateRevision = LocalCommit{}, nil
	release := peer.releaseCandidate
	peer.releaseCandidate = nil
	if release != nil {
		release()
	}
}

// NewPrimaryCoordinatorFromSource validates the complete frozen suffix without
// retaining payloads. Authenticated ordering/native ranges remain the retry
// authority. PreparePeer admits a bounded initial batch before socket Run; ACKs
// release each peer's byte/count window and drive its next batch, including
// confirmed history owed to a lagging second Replica.
func NewPrimaryCoordinatorFromSource(clusterID, incarnation, nodeID string, term, confirmed uint64, peers []string, journal *OrderingJournal, outbound OutboundQueue, persist PrimaryStateWriter, source DurableSource, limits PrimaryWindowLimits) (*PrimaryCoordinator, error) {
	if source == nil {
		return nil, errors.New("primary source is required")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	release, err := limits.acquireCandidate()
	if err != nil {
		return nil, err
	}
	if release != nil {
		defer release()
	}
	return newPrimaryCoordinator(clusterID, incarnation, nodeID, term, confirmed, peers, journal, outbound, persist,
		func(consume func(LocalCommit) error) error {
			if journal == nil {
				return errors.New("ordering journal is required")
			}
			head, _, _ := journal.Head()
			if confirmed == head {
				return nil
			}
			return ReconstructRangeEach(clusterID, incarnation, confirmed+1, head, journal, source, consume)
		}, source, limits)
}

func (c *PrimaryCoordinator) windowFor(node string) *primaryPeerWindow {
	for i := range c.peerWindows {
		if c.peerWindows[i].node == node && node != "" {
			return &c.peerWindows[i]
		}
	}
	return nil
}

// PreparePeer must run inside the newly registered session generation before
// its data loops start. Session replacement drains old admitted ACK handlers.
// Hello does not confirm anything: it seeds only this peer's send position.
// send must synchronously copy into this session's bounded, nonblocking queue.
func (c *PrimaryCoordinator) PreparePeer(node string, durable, applied uint64, send func([]byte) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.source == nil {
		return errors.New("peer windows require a source-backed primary")
	}
	if c.poisoned != nil || c.manualUnavailable != nil {
		return ErrReplicationUnavailable
	}
	peer := c.windowFor(node)
	head, _, _ := c.journal.Head()
	if peer == nil || durable > head || applied > durable || send == nil {
		return errors.New("peer preparation progress or identity is invalid")
	}
	if durable < head {
		record, err := c.journal.Record(durable + 1)
		if err != nil {
			return err
		}
		if record.Term != c.term {
			return fmt.Errorf("%w: peer needs a prior-term prefix before the current anchor", ErrMemberRequiresFullReseed)
		}
	}
	// Bind the concrete session: another peer's ACK must never route an old
	// window into a replacement connection whose Prepare has not run yet.
	peer.closeReader()
	*peer = primaryPeerWindow{node: node, ready: true, durable: durable, queuedThrough: durable, send: send, reader: c.source}
	if factory, ok := c.source.(sourceReaderFactory); ok {
		reader, release, err := factory.acquireRangeReader()
		if err != nil {
			peer.ready, peer.failure, peer.send = false, err, nil
			c.poisoned = fmt.Errorf("acquire ordered native reader: %w", err)
			c.closeSourceWindows()
			c.refreshSourceAvailability()
			c.signalChanged()
			return err
		}
		peer.reader, peer.releaseReader = reader, release
	}
	// A new session may advertise a smaller allowed native prefix. Do not
	// carry its old applied proof or queued lengths into this generation.
	c.peerApplied[node] = applied
	if err := c.fillPeerWindow(peer); err != nil {
		peer.ready = false
		peer.failure = err
		peer.send = nil
		peer.closeReader()
		if c.poisoned != nil {
			c.closeSourceWindows()
		}
		c.refreshSourceAvailability()
		c.signalChanged()
		return err
	}
	c.refreshSourceAvailability()
	c.signalChanged()
	return nil
}

func (c *PrimaryCoordinator) ForgetPeer(node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.source == nil {
		return
	}
	if peer := c.windowFor(node); peer != nil {
		peer.closeReader()
		*peer = primaryPeerWindow{node: node, failure: ErrPeerNotConnected}
	}
	c.refreshSourceAvailability()
	c.signalChanged()
}

func (c *PrimaryCoordinator) releasePeerWindow(peer *primaryPeerWindow, durable uint64) {
	for peer.durable < durable {
		peer.durable++
		slot := peer.durable % uint64(c.windowLimits.MaxFrames)
		peer.bytes -= peer.sizes[slot]
		peer.sizes[slot] = 0
	}
}

func (c *PrimaryCoordinator) readSourceCommit(index uint64) (LocalCommit, error) {
	return c.readSourceCommitFrom(c.source, index)
}

func (c *PrimaryCoordinator) readSourceCommitFrom(source FrameSource, index uint64) (LocalCommit, error) {
	var commit LocalCommit
	err := ReconstructRangeEach(c.clusterID, c.incarnation, index, index, c.journal, source, func(frame LocalCommit) error { commit = frame; return nil })
	return commit, err
}

func (c *PrimaryCoordinator) fillPeerWindow(peer *primaryPeerWindow) error {
	head, _, _ := c.journal.Head()
	for peer.queuedThrough < head && peer.queuedThrough-peer.durable < uint64(c.windowLimits.MaxFrames) {
		index := peer.queuedThrough + 1
		if peer.candidate.Index == 0 {
			release, err := c.windowLimits.acquireCandidate()
			if err != nil {
				return err // capacity failure does not poison durable facts
			}
			peer.releaseCandidate = release
			if guard, ok := peer.reader.(sourceRevisionGuard); ok {
				peer.candidateRevision, err = guard.sourceRevision()
				if err != nil {
					c.poisoned = err
					return err
				}
			}
			peer.candidate, err = c.readSourceCommitFrom(peer.reader, index)
			if err != nil {
				c.poisoned = fmt.Errorf("ordered native source cannot be reconstructed: %w", err)
				return err
			}
		}
		if guard, ok := peer.reader.(sourceRevisionGuard); ok {
			revision, err := guard.sourceRevision()
			if err != nil || revision != peer.candidateRevision {
				c.poisoned = errSourceRevisionChanged
				return errSourceRevisionChanged
			}
		}
		if len(peer.candidate.Encoded) > c.windowLimits.MaxBytes-peer.bytes {
			break
		}
		send := func() error { return peer.send(peer.candidate.Encoded) }
		var err error
		if guard, ok := peer.reader.(sourceRevisionGuard); ok {
			err = guard.deliverAtRevision(peer.candidateRevision, send)
		} else {
			err = send()
		}
		if err != nil {
			if errors.Is(err, errSourceRevisionChanged) {
				c.poisoned = err
			}
			return err
		}
		peer.sizes[index%uint64(c.windowLimits.MaxFrames)] = len(peer.candidate.Encoded)
		peer.bytes += len(peer.candidate.Encoded)
		peer.queuedThrough = index
		peer.clearCandidate() // queue now owns its independent synchronous copy
	}
	if len(c.pendingNotice) > 0 {
		through := min(c.confirmed, peer.queuedThrough)
		if through > peer.noticeThrough && through > c.peerApplied[peer.node] {
			// A current-term notice cannot precede that term's native anchor
			// in this peer's FIFO. Term adoption/reseed policy is unchanged.
			if peer.queuedThrough > 0 {
				record, err := c.journal.Record(peer.queuedThrough)
				if err != nil {
					return err
				}
				if record.Term != c.term {
					return nil
				}
			}
			notice, err := (CommitNotice{ClusterID: c.clusterID, Incarnation: c.incarnation, NodeID: c.nodeID, Term: c.term, ConfirmedIndex: through}).MarshalBinary()
			if err != nil {
				return err
			}
			if err := peer.send(notice); err != nil {
				return err
			}
			peer.noticeThrough = through
		}
	}
	return nil
}

// Stop releases session/payload references after the transport drain and
// prevents subsequent durable callbacks. The caller still owns source/journal.
func (c *PrimaryCoordinator) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recoveryRevision++
	c.stopped = true
	c.manualUnavailable = ErrReplicationUnavailable
	c.available = false
	c.unavailable = ErrReplicationUnavailable
	for i := range c.peerWindows {
		c.peerWindows[i].closeReader()
	}
	c.peerWindows = [2]primaryPeerWindow{}
	c.pending = nil
	c.pendingNotice = nil
	c.pendingNoticeIndex = 0
	c.signalChanged()
}

// ReleaseUnconfirmedForShutdown is called only after native sources and the
// ordering journal have joined and closed. A failed close keeps these owners
// charged until the process-wide budget is retired.
func (c *PrimaryCoordinator) ReleaseUnconfirmedForShutdown() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		return errors.New("Primary must stop before unconfirmed shutdown release")
	}
	var releaseErr error
	if c.hasCompletionUnconfirmedPermit {
		err := c.completionUnconfirmedPermit.Release()
		releaseErr = errors.Join(releaseErr, err)
		if err == nil {
			c.completionUnconfirmedPermit, c.hasCompletionUnconfirmedPermit = resourcebudget.Permit{}, false
		}
	}
	if c.hasLiveUnconfirmedPermit {
		err := c.liveUnconfirmedPermit.Release()
		releaseErr = errors.Join(releaseErr, err)
		if err == nil {
			c.liveUnconfirmedPermit = resourcebudget.Permit{}
			c.hasLiveUnconfirmedPermit = false
		}
	}
	if c.hasRecoveryUnconfirmedPermit {
		err := c.recoveryUnconfirmedPermit.Release()
		releaseErr = errors.Join(releaseErr, err)
		if err == nil {
			c.recoveryUnconfirmedPermit = resourcebudget.Permit{}
			c.hasRecoveryUnconfirmedPermit = false
		}
	}
	for _, admission := range c.orphanAdmissions {
		if admission == nil {
			continue
		}
		if owned, ok := admission.(*primaryUnconfirmedAdmission); ok {
			releaseErr = errors.Join(releaseErr, owned.releaseLocked())
		} else {
			releaseErr = errors.Join(releaseErr, errors.New("unknown Primary unconfirmed admission owner"))
		}
	}
	if releaseErr == nil {
		c.orphanAdmissions = nil
		c.unconfirmedEntries = nil
	}
	return releaseErr
}

func (c *PrimaryCoordinator) pumpSourceWindows() {
	for i := range c.peerWindows {
		peer := &c.peerWindows[i]
		if !peer.ready {
			continue
		}
		if err := c.fillPeerWindow(peer); err != nil {
			peer.ready = false
			peer.failure = err
			peer.send = nil
			peer.closeReader()
			if c.poisoned != nil {
				c.closeSourceWindows()
				break
			}
		}
	}
	c.refreshSourceAvailability()
}

func (c *PrimaryCoordinator) refreshSourceAvailability() {
	c.available = false
	var failure error = ErrPeerNotConnected
	for i := range c.peerWindows {
		peer := &c.peerWindows[i]
		if peer.failure != nil {
			failure = peer.failure
		}
		if peer.ready {
			c.available = true
		}
	}
	if c.manualUnavailable != nil {
		c.available = false
		failure = c.manualUnavailable
	}
	if c.poisoned != nil {
		c.available = false
		failure = c.poisoned
	}
	if c.recoveryOverLimit {
		c.available = false
		failure = ErrUnconfirmedRecoveryOverLimit
	}
	if c.available {
		c.unavailable = nil
	} else {
		c.unavailable = fmt.Errorf("%w: %w", ErrReplicationUnavailable, failure)
	}
}

// Called with c.mu held, after a global fail-closed transition.
func (c *PrimaryCoordinator) closeSourceWindows() {
	for i := range c.peerWindows {
		c.peerWindows[i].closeReader()
		c.peerWindows[i].ready, c.peerWindows[i].send = false, nil
	}
	c.refreshSourceAvailability()
	c.signalChanged()
}
