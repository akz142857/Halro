package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ownedfile"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

// This original controller owns one paid fixed control workspace and only
// copied scalar messages. It cannot construct a native, ordering or apply grant.
// Ordinary authentication, receiver receipts and source MACs remain mandatory.
type haPeerPhysicalController struct {
	owner     *haSelectedRuntimeFoundation
	mu        sync.Mutex
	entry     sync.RWMutex
	fee       resourcebudget.Permit
	vector    haPeerPhysicalVector
	spent     replication.PhysicalCreditCost
	grant     replication.PhysicalCreditCost
	active    [2]replication.PeerPhysicalFutureTicketObservation
	primary   bool
	local     [32]byte
	peers     [2]haPeerPhysicalPeer
	live      *haPeerPhysicalTicket
	bankIndex uint64

	closing bool
	failure error
}

func newHAReplicaPhysicalController(ctx context.Context, f *haSelectedRuntimeFoundation) (*haPeerPhysicalController, error) {
	if f == nil || f.replication == nil || f.replication.role != replication.RoleReplica || f.replication.applier == nil || f.replication.manager != nil || f.reopened == nil || f.metadataStore == nil {
		return nil, replication.ErrPhysicalCredit
	}
	fee, err := f.owners.Components.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 10})
	if err != nil {
		return nil, err
	}
	q := &haPeerPhysicalController{owner: f, fee: fee}
	err = q.withReplicaObservationMode(ctx, true, func(ctx context.Context, v haPeerPhysicalVector, original replication.PeerPhysicalFutureObservation) error {
		if original.WindowID != v.Window {
			// A fresh window is minted only from actual full common publication.
			if v.Seal.OrderingIndex != v.Member.DurableIndex {
				return replication.ErrPhysicalCredit
			}
			return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
				if cut.Seal != v.Seal || cut.Member.OrderingHeadMAC != v.Member.OrderingHeadMAC {
					return replication.ErrPhysicalCredit
				}
				return f.physicalPublication.RecordPeerFutureWindowFrozen(ctx, v.Window)
			})
		}
		return nil
	})
	if err == nil {
		err = f.replication.applier.InstallPhysicalReceiveAdmission(ctx, q.beforeReceive)
	}
	if err != nil {
		if cleanup := fee.Release(); cleanup != nil {
			q.failure = errors.Join(err, cleanup)
			return q, q.failure
		}
		return nil, err
	}
	return q, nil
}

func (q *haPeerPhysicalController) withReplicaObservation(ctx context.Context, visit func(context.Context, haPeerPhysicalVector, replication.PeerPhysicalFutureObservation) error) error {
	return q.withReplicaObservationMode(ctx, false, visit)
}

// Only construction before transport installation may renew a fully retired
// bank. Live receive/credit observations keep exact bank identity checks.
func (q *haPeerPhysicalController) withReplicaObservationMode(ctx context.Context, startup bool, visit func(context.Context, haPeerPhysicalVector, replication.PeerPhysicalFutureObservation) error) error {
	f := q.owner
	return f.replication.applier.WithPhysicalReceiveReadBarrier(ctx, func(ctx context.Context, p replication.ReplicaConfirmedPosition, ack replication.Acknowledgement) error {
		// Observation, grant and negative receipt pay from the paid catch-up
		// lanes, as the Primary's reserve does. At full pressure a new IO
		// admission is refused; the handler's error would end the session,
		// and the next session's window could not be sent either.
		return f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			return q.observeReplicaLocked(ctx, startup, p, ack, visit)
		})
	})
}

// observeReplicaLocked runs inside the receive-read barrier and the paid
// catch-up workspace, in that order, the order every Replica path takes them.
func (q *haPeerPhysicalController) observeReplicaLocked(ctx context.Context, startup bool, p replication.ReplicaConfirmedPosition, ack replication.Acknowledgement, visit func(context.Context, haPeerPhysicalVector, replication.PeerPhysicalFutureObservation) error) error {
	f := q.owner
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closing || q.failure != nil || f.ctx.Err() != nil {
		return errors.Join(replication.ErrPhysicalCredit, q.failure, f.ctx.Err())
	}
	v, err := f.capturePeerPhysicalVector(ctx)
	if err != nil {
		return err
	}
	m := v.Member
	if p.NodeID != m.NodeID || p.Term != m.Term || p.PromisedTerm != m.Term || p.Durable != m.DurableIndex || p.Confirmed != m.ConfirmedIndex || p.Applied != m.AppliedIndex || p.Projection != m.Projection || ack.Index != m.DurableIndex {
		return replication.ErrPhysicalCredit
	}
	var original replication.PeerPhysicalFutureObservation
	if err = f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
		if cut.Seal != v.Seal || cut.Member.OrderingHeadMAC != m.OrderingHeadMAC {
			return replication.ErrPhysicalCredit
		}
		var err error
		original, err = f.physicalPublication.ObservePeerPhysicalPromisesFrozen(ctx)
		return err
	}); err != nil {
		return err
	}
	spent, err := haPeerPhysicalSpent(v)
	if err != nil {
		return err
	}
	if original.WindowID != ([32]byte{}) && original.WindowID != v.Window {
		if !startup || f.replication.manager != nil || v.Seal.OrderingIndex != m.DurableIndex || !spent.IsZero() {
			return replication.ErrPhysicalCredit
		}
		for _, ticket := range original.Tickets {
			if ticket.Active {
				return replication.ErrPhysicalCredit
			}
		}
	}
	if original.WindowID != ([32]byte{}) && !original.Granted.Covers(spent) {
		return replication.ErrPhysicalCredit
	}
	q.vector, q.spent, q.grant, q.active = v, spent, original.Granted, original.Tickets
	if err = visit(ctx, v, original); err != nil {
		return err
	}
	return errors.Join(ctx.Err(), f.ctx.Err())
}

func haPeerPhysicalSpent(v haPeerPhysicalVector) (out replication.PhysicalCreditCost, err error) {
	current := [4]authenticatedstate.PhysicalNativeReference{v.Ledger.Native, {}, {}, v.Metadata.Native}
	baseline := [4]authenticatedstate.PhysicalNativeReference{v.Ledger.Window.Native, {}, {}, v.Metadata.Window.Native}
	for _, i := range []int{0, 3} {
		n, b := current[i], baseline[i]
		if n.Generation != b.Generation || n.Sequence < b.Sequence || n.Sequence-b.Sequence > 65535 || n.EOF < b.EOF {
			return out, replication.ErrPhysicalCredit
		}
		out.NativeRecords[i] = uint32(n.Sequence - b.Sequence)
		out.NativeBytes[i] = uint64(n.EOF - b.EOF)
		out.COWRecords[i] = out.NativeRecords[i]
	}
	native := []struct {
		slot                int
		beforeSeq, afterSeq uint64
		beforeEOF, afterEOF int64
	}{
		{1, v.Audit.Baseline.Sequence, v.Audit.Native.Head.Sequence, v.Audit.Baseline.EOF, v.Audit.Native.Head.EOF},
		{2, v.Governance.Native.Baseline.Sequence, v.Governance.Native.Native.Head.Sequence, v.Governance.Native.Baseline.EOF, v.Governance.Native.Native.Head.EOF},
	}
	for _, n := range native {
		if n.afterSeq < n.beforeSeq || n.afterSeq-n.beforeSeq > 65535 || n.afterEOF < n.beforeEOF {
			return out, replication.ErrPhysicalCredit
		}
		out.NativeRecords[n.slot] = uint32(n.afterSeq - n.beforeSeq)
		out.NativeBytes[n.slot] = uint64(n.afterEOF - n.beforeEOF)
	}
	out.COWRecords[2] = out.NativeRecords[2]
	out.UsageFrames = out.NativeRecords[0]
	if v.Ordering.Index < v.Seal.OrderingIndex || v.Ordering.Index-v.Seal.OrderingIndex > 65535 {
		return out, replication.ErrPhysicalCredit
	}
	out.OrderingRecords = uint32(v.Ordering.Index - v.Seal.OrderingIndex)
	return out, nil
}

func haPeerPhysicalFrameCost(frame replication.Frame) (cost replication.PhysicalCreditCost, err error) {
	cost.OrderingRecords = 1
	if frame.Kind == replication.KindLedgerRoll {
		cost.DirectoryRolls = 1
		return cost, nil
	}
	if frame.Kind != replication.KindData || frame.StoreSequenceLast < frame.StoreSequenceFirst || frame.StoreSequenceLast-frame.StoreSequenceFirst >= 65535 {
		return cost, replication.ErrPhysicalCredit
	}
	slot := int(frame.Store) - 1
	if slot < 0 || slot > 3 {
		return cost, replication.ErrPhysicalCredit
	}
	n := uint32(frame.StoreSequenceLast - frame.StoreSequenceFirst + 1)
	cost.NativeBytes[slot] = uint64(len(frame.Payload))
	cost.NativeRecords[slot] = n
	if slot != 1 {
		cost.COWRecords[slot] = n
	}
	if slot == 0 {
		cost.UsageFrames = n
	}
	return cost, nil
}

func (q *haPeerPhysicalController) beforeReceive(frame replication.Frame) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closing || q.failure != nil || q.owner.ctx.Err() != nil {
		return replication.ErrPhysicalCredit
	}
	active := false
	for _, t := range q.active {
		active = active || t.Active
	}
	if !active {
		return replication.ErrPhysicalCredit
	}
	cost, err := haPeerPhysicalFrameCost(frame)
	if err != nil {
		return err
	}
	next, err := q.spent.Add(cost)
	if err != nil || !q.grant.Covers(next) {
		return replication.ErrPhysicalCredit
	}
	// Unknown native/Ordering IO never refunds this debit or the persisted ticket.
	q.spent = next
	return nil
}

func (q *haPeerPhysicalController) replicaCredit(ctx context.Context, request *replication.PhysicalCreditRecord) (reply replication.PhysicalCreditRecord, resultErr error) {
	if request == nil || request.Operation == replication.PhysicalCreditReserve {
		if !q.entry.TryRLock() {
			return reply, replication.ErrPhysicalCredit
		}
		defer q.entry.RUnlock()
	}
	resultErr = q.withReplicaObservation(ctx, func(ctx context.Context, v haPeerPhysicalVector, original replication.PeerPhysicalFutureObservation) error {
		f := q.owner
		if request != nil {
			if request.WindowID != v.Window || request.BaselineDigest != f.owners.Baseline.Descriptor().AuthorityDigest || request.CommonIndex != v.Seal.OrderingIndex || request.CommonMAC != v.Seal.OrderingMAC {
				return replication.ErrPhysicalCredit
			}
			if err := f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
				if cut.Seal != v.Seal || cut.Member.OrderingHeadMAC != v.Member.OrderingHeadMAC {
					return replication.ErrPhysicalCredit
				}
				switch request.Operation {
				case replication.PhysicalCreditReserve:
					peerIndex := -1
					for i, p := range cut.Member.Peers {
						if p.Name == f.owners.Baseline.CurrentPrimary() {
							peerIndex = i
						}
					}
					if peerIndex < 0 || peerIndex > 1 {
						return replication.ErrPhysicalCredit
					}
					return f.physicalPublication.RecordPeerFuturePromiseFrozen(ctx, v.Window, request.TicketID, request.RequestDigest, request.Cost, v.Available, uint8(peerIndex))
				case replication.PhysicalCreditRelease:
					matched := false
					for _, t := range original.Tickets {
						if t.ID == request.TicketID && t.Window == request.WindowID && t.Request == request.RequestDigest {
							matched = true
						}
					}
					if !matched {
						return replication.ErrPhysicalCredit
					}
					return f.physicalPublication.RecordPeerFutureReleaseFrozen(ctx, v.Window, request.TicketID, request.Cost, q.spent)
				default:
					return replication.ErrPhysicalCredit
				}
			}); err != nil {
				return err
			}
			if err := f.withSelectedCut(ctx, func(ctx context.Context, _ statecut.HASelectedCut) error {
				var err error
				original, err = f.physicalPublication.ObservePeerPhysicalPromisesFrozen(ctx)
				return err
			}); err != nil {
				return err
			}
			q.grant, q.active = original.Granted, original.Tickets
		}
		held := replication.PhysicalCreditCost{}
		for _, t := range original.Tickets {
			if t.Active {
				var err error
				held, err = held.Add(t.Cost)
				if err != nil {
					return err
				}
			}
		}
		spare, err := v.Available.Subtract(held)
		if err != nil {
			spare = replication.PhysicalCreditCost{}
		}
		m := v.Member
		reply = replication.PhysicalCreditRecord{Operation: replication.PhysicalCreditWindow, Status: replication.Acknowledgement{ClusterID: m.ClusterID, Incarnation: m.Incarnation, NodeID: m.NodeID, Term: m.Term, Index: m.DurableIndex, DurableIndex: m.DurableIndex, AppliedIndex: m.AppliedIndex}, BaselineDigest: f.owners.Baseline.Descriptor().AuthorityDigest, CommonIndex: v.Seal.OrderingIndex, CommonMAC: v.Seal.OrderingMAC, WindowID: v.Window, TicketSerial: original.Serial, Available: spare}
		if request != nil {
			reply.TicketID, reply.TicketSerial, reply.RequestDigest, reply.Cost = request.TicketID, request.TicketSerial, request.RequestDigest, request.Cost
			reply.Operation = replication.PhysicalCreditReserved
			if request.Operation == replication.PhysicalCreditRelease {
				reply.Operation = replication.PhysicalCreditReleased
			}
		}
		return reply.Validate()
	})
	if resultErr != nil {
		reply = replication.PhysicalCreditRecord{}
	}
	return
}

// A known rejection is an actual negative receipt, never a timeout inference.
// Only a fresh complete original source/control observation may prove that the
// requested next ticket was not persisted. Unknown IO cannot enter this path.
func (q *haPeerPhysicalController) replicaRejection(ctx context.Context, request replication.PhysicalCreditRecord) (reply replication.PhysicalCreditRecord, resultErr error) {
	if !q.entry.TryRLock() {
		return reply, replication.ErrPhysicalCredit
	}
	defer q.entry.RUnlock()
	resultErr = q.withReplicaObservation(ctx, func(ctx context.Context, v haPeerPhysicalVector, original replication.PeerPhysicalFutureObservation) error {
		if request.Operation != replication.PhysicalCreditReserve || request.WindowID != v.Window || request.TicketSerial != original.Serial+1 || original.Serial == ^uint64(0) || request.CommonIndex != v.Seal.OrderingIndex || request.CommonMAC != v.Seal.OrderingMAC {
			return replication.ErrPhysicalCredit
		}
		for _, t := range original.Tickets {
			if t.ID == request.TicketID {
				return replication.ErrPhysicalCredit
			}
		}
		f := q.owner
		if err := f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			if cut.Seal != v.Seal || cut.Member.OrderingHeadMAC != v.Member.OrderingHeadMAC {
				return replication.ErrPhysicalCredit
			}
			peerIndex := -1
			for i, p := range cut.Member.Peers {
				if p.Name == f.owners.Baseline.CurrentPrimary() {
					peerIndex = i
				}
			}
			if peerIndex < 0 || peerIndex > 1 {
				return replication.ErrPhysicalCredit
			}
			return f.physicalPublication.RecordPeerFutureRejectionFrozen(ctx, v.Window, request.TicketID, request.RequestDigest, uint8(peerIndex))
		}); err != nil {
			return err
		}
		m := v.Member
		reply = request
		reply.Operation = replication.PhysicalCreditRejected
		reply.Status = replication.Acknowledgement{ClusterID: m.ClusterID, Incarnation: m.Incarnation, NodeID: m.NodeID, Term: m.Term, Index: m.DurableIndex, DurableIndex: m.DurableIndex, AppliedIndex: m.AppliedIndex}
		reply.Available = v.Available
		return reply.Validate()
	})
	if resultErr != nil {
		reply = replication.PhysicalCreditRecord{}
	}
	return
}

type haPeerPhysicalPeer struct {
	window   replication.PhysicalCreditRecord
	response chan replication.PhysicalCreditRecord
	waiting  bool
	expected replication.PhysicalCreditRecord
}

func newHAPrimaryPhysicalController(ctx context.Context, f *haSelectedRuntimeFoundation) (*haPeerPhysicalController, error) {
	if f == nil || f.replication == nil || f.replication.role != replication.RolePrimary || f.replication.manager != nil || f.reopened == nil || f.metadataStore == nil || f.physicalPublication == nil {
		return nil, replication.ErrPhysicalCredit
	}
	fee, err := f.owners.Components.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 10})
	if err != nil {
		return nil, err
	}
	q := &haPeerPhysicalController{owner: f, fee: fee, primary: true}
	err = f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			m := cut.Member
			if m.Role != replication.RolePrimary || m.Term != m.PromisedTerm || m.DurableIndex != m.ConfirmedIndex || m.AppliedIndex != m.DurableIndex || cut.Seal.OrderingIndex != m.DurableIndex || len(m.Peers) < 1 || len(m.Peers) > 2 {
				return replication.ErrPhysicalCredit
			}
			original, err := f.physicalPublication.ObservePhysicalOrderingWindowFrozen(ctx)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(struct {
				Seal     authenticatedstate.CandidateSeal
				Ordering replication.PhysicalOrderingWindowObservation
			}{cut.Seal, original})
			if err != nil {
				return err
			}
			q.local = sha256.Sum256(raw)
			q.bankIndex = m.DurableIndex
			existing, err := f.physicalPublication.ObservePeerPhysicalPromisesFrozen(ctx)
			if err != nil {
				return err
			}
			for _, t := range existing.Tickets {
				if t.Active {
					return replication.ErrPhysicalCredit
				}
			}
			if existing.WindowID != q.local {
				if err = f.physicalPublication.RecordPeerFutureWindowFrozen(ctx, q.local); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		if cleanup := fee.Release(); cleanup != nil {
			q.failure = errors.Join(err, cleanup)
			return q, q.failure
		}
		return nil, err
	}
	for i := range q.peers {
		q.peers[i].response = make(chan replication.PhysicalCreditRecord, 1)
	}
	return q, nil
}
func (q *haPeerPhysicalController) peerIndex(node string) (int, error) {
	m := q.owner.replication.publisher.Snapshot()
	for i, p := range m.Peers {
		if p.Name == node && i < 2 {
			return i, nil
		}
	}
	return -1, replication.ErrPhysicalCredit
}
func (q *haPeerPhysicalController) handle(ctx context.Context, peer replication.Hello, record replication.StreamRecord, send func([]byte) error) error {
	if record.Kind != replication.StreamRecordPhysicalCredit {
		return replication.ErrPhysicalCredit
	}
	message, err := replication.UnmarshalPhysicalCreditRecord(record.Encoded)
	if err != nil {
		return err
	}
	if err = message.ValidatePeer(peer, q.owner.replication.role); err != nil {
		return err
	}
	if message.BaselineDigest != q.owner.owners.Baseline.Descriptor().AuthorityDigest || q.owner.ctx.Err() != nil {
		return replication.ErrPhysicalCredit
	}
	if q.primary {
		index, err := q.peerIndex(peer.NodeID)
		if err != nil {
			return err
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.closing || q.failure != nil {
			return errors.Join(replication.ErrPhysicalCredit, q.failure)
		}
		p := &q.peers[index]
		if message.Operation == replication.PhysicalCreditWindow {
			if message.CommonIndex < q.bankIndex {
				return nil
			}
			p.window = message
			return nil // A Window is never passed to coordinator.Acknowledge.
		}
		if !p.waiting || message.WindowID != p.expected.WindowID || message.TicketID != p.expected.TicketID || message.RequestDigest != p.expected.RequestDigest || message.Cost != p.expected.Cost || message.CommonIndex != p.expected.CommonIndex || message.CommonMAC != p.expected.CommonMAC || message.TicketSerial != p.expected.TicketSerial {
			return replication.ErrPhysicalCredit
		}
		wanted := replication.PhysicalCreditReserved
		if p.expected.Operation == replication.PhysicalCreditRelease {
			wanted = replication.PhysicalCreditReleased
		}
		if message.Operation != wanted && (p.expected.Operation != replication.PhysicalCreditReserve || message.Operation != replication.PhysicalCreditRejected) {
			return replication.ErrPhysicalCredit
		}
		select {
		case p.response <- message:
			return nil
		default:
			return replication.ErrPhysicalCredit
		}
	}
	if send == nil || peer.NodeID != q.owner.owners.Baseline.CurrentPrimary() {
		return replication.ErrPhysicalCredit
	}
	reply, err := q.replicaCredit(ctx, &message)
	if err != nil && message.Operation == replication.PhysicalCreditReserve {
		// Return a negative receipt only when a fresh independent read proves that
		// the requested ticket was not persisted. Unknown ownership prevents it.
		negative, known := q.replicaRejection(ctx, message)
		if known == nil {
			reply, err = negative, nil
		}
	}
	if err != nil {
		return err
	}
	encoded, err := reply.MarshalBinary()
	if err != nil {
		return err
	}
	return send(encoded)
}
func (q *haPeerPhysicalController) sendReplicaWindow(ctx context.Context, send func([]byte) error) error {
	if q.primary || send == nil {
		return replication.ErrPhysicalCredit
	}
	reply, err := q.replicaCredit(ctx, nil)
	if err != nil {
		return err
	}
	encoded, err := reply.MarshalBinary()
	if err != nil {
		return err
	}
	return send(encoded)
}

type haPeerPhysicalTicketKey struct{}
type haPeerPhysicalTicket struct {
	self            *haPeerPhysicalTicket
	owner           *haPeerPhysicalController
	mu              sync.Mutex
	request         [32]byte
	cost, remaining replication.PhysicalCreditCost
	used            replication.PhysicalCreditCost
	messages        [2]replication.PhysicalCreditRecord
	peers           int
	active          bool
	exposed         bool
	startIndex      uint64
	startTerm       uint64
	startMAC        [32]byte
	startAllowed    [4]replication.StoreCursor
	startHeads      [4][32]byte
	localWritten    [2]bool
	remoteKnown     [2]bool
	remoteRejected  [2]bool
	remoteAttempted [2]bool
	completed       [2]bool
}

func (q *haPeerPhysicalController) beforePrimarySource(ctx context.Context, slot int, first, last uint64, size int) error {
	if !q.primary || ctx == nil || slot < 0 || slot > 3 || last < first || last-first >= 65535 || size <= 0 || q.owner.ctx.Err() != nil {
		return replication.ErrPhysicalCredit
	}
	ticket, _ := ctx.Value(haPeerPhysicalTicketKey{}).(*haPeerPhysicalTicket)
	if ticket == nil || ticket.self != ticket || ticket.owner != q {
		return replication.ErrPhysicalCredit
	}
	cost := replication.PhysicalCreditCost{OrderingRecords: 1}
	cost.NativeBytes[slot] = uint64(size)
	cost.NativeRecords[slot] = uint32(last - first + 1)
	if slot != 1 {
		cost.COWRecords[slot] = cost.NativeRecords[slot]
	}
	if slot == 0 {
		cost.UsageFrames = cost.NativeRecords[slot]
	}
	ticket.mu.Lock()
	defer ticket.mu.Unlock()
	if !ticket.active {
		return replication.ErrPhysicalCredit
	}
	remaining, err := ticket.remaining.Subtract(cost)
	if err != nil {
		return err
	}
	ticket.remaining = remaining
	return nil
}
func (q *haPeerPhysicalController) rpc(ctx context.Context, index int, request replication.PhysicalCreditRecord) (reply replication.PhysicalCreditRecord, resultErr error) {
	if index < 0 || index > 1 || !q.primary || q.owner.replication.manager == nil {
		return reply, replication.ErrPhysicalCredit
	}
	q.mu.Lock()
	p := &q.peers[index]
	if q.closing || q.failure != nil || p.waiting {
		q.mu.Unlock()
		return reply, replication.ErrPhysicalCredit
	}
	select {
	case <-p.response:
	default:
	}
	p.waiting, p.expected = true, request
	q.mu.Unlock()
	defer func() { q.mu.Lock(); p.waiting = false; p.expected = replication.PhysicalCreditRecord{}; q.mu.Unlock() }()
	encoded, err := request.MarshalBinary()
	if err != nil {
		return reply, err
	}
	m := q.owner.replication.publisher.Snapshot()
	if index >= len(m.Peers) {
		return reply, replication.ErrPhysicalCredit
	}
	if err = q.owner.replication.manager.Send(m.Peers[index].Name, encoded); err != nil {
		return reply, err
	}
	select {
	case reply = <-p.response:
		return reply, nil
	case <-ctx.Done():
		return reply, ctx.Err()
	case <-q.owner.ctx.Done():
		return reply, q.owner.ctx.Err()
	}
}
func (q *haPeerPhysicalController) reserve(ctx context.Context, requestDigest [32]byte, cost replication.PhysicalCreditCost) (context.Context, *haPeerPhysicalTicket, error) {
	if !q.primary || requestDigest == ([32]byte{}) || cost.OrderingRecords == 0 {
		return ctx, nil, replication.ErrPhysicalCredit
	}
	if !q.entry.TryRLock() {
		return ctx, nil, replication.ErrPhysicalCredit
	}
	defer q.entry.RUnlock()
	ticket := &haPeerPhysicalTicket{owner: q, request: requestDigest, cost: cost, remaining: cost, active: true}
	ticket.self = ticket
	err := q.owner.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return q.owner.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			m := cut.Member
			if m.Role != replication.RolePrimary || m.DurableIndex != m.ConfirmedIndex || m.AppliedIndex != m.DurableIndex || len(m.Peers) < 1 || len(m.Peers) > 2 {
				return replication.ErrPhysicalCredit
			}
			ticket.peers = len(m.Peers)
			q.mu.Lock()
			defer q.mu.Unlock()
			if q.closing || q.failure != nil || q.live != nil {
				return replication.ErrPhysicalCredit
			}
			q.live = ticket
			ticket.startIndex, ticket.startTerm, ticket.startMAC = m.DurableIndex, m.Term, m.OrderingHeadMAC
			ticket.startAllowed, ticket.startHeads = cut.Allowed, cut.Heads
			for i := 0; i < ticket.peers; i++ {
				p := &q.peers[i]
				w := p.window
				if p.waiting || w.Operation != replication.PhysicalCreditWindow || w.Status.NodeID != m.Peers[i].Name || w.Status.Term != m.Term || w.Status.ClusterID != m.ClusterID || w.Status.Incarnation != m.Incarnation || w.Status.AppliedIndex < w.CommonIndex || w.CommonIndex < q.bankIndex || w.CommonIndex > m.DurableIndex || w.TicketSerial == ^uint64(0) || !w.Available.Covers(cost) {
					return replication.ErrPhysicalCredit
				}
				rec, err := q.owner.replication.journal.Record(w.CommonIndex)
				if err != nil || rec.MAC != w.CommonMAC {
					return errors.Join(replication.ErrPhysicalCredit, err)
				}
				message := replication.PhysicalCreditRecord{Operation: replication.PhysicalCreditReserve, Status: replication.Acknowledgement{ClusterID: m.ClusterID, Incarnation: m.Incarnation, NodeID: m.NodeID, Term: m.Term, Index: m.DurableIndex, DurableIndex: m.DurableIndex, AppliedIndex: m.AppliedIndex}, BaselineDigest: w.BaselineDigest, CommonIndex: w.CommonIndex, CommonMAC: w.CommonMAC, WindowID: w.WindowID, TicketSerial: w.TicketSerial + 1, RequestDigest: requestDigest, Cost: cost}
				copy(message.TicketID[:8], w.WindowID[:8])
				binary.BigEndian.PutUint64(message.TicketID[8:], message.TicketSerial)
				// Keep the attempted original owner reachable even when Sync is unknown.
				ticket.messages[i] = message
				ticket.localWritten[i] = true
				// Persist all mirrors before any reserve is sent or request is accepted.
				if err = q.owner.physicalPublication.RecordPeerFuturePromiseForPeerFrozen(ctx, q.local, w.WindowID, message.TicketID, requestDigest, cost, w.Available, uint8(i)); err != nil {
					return err
				}
				ticket.messages[i] = message
				ticket.localWritten[i] = true
			}
			return nil
		})
	})
	if err != nil {
		ticket.active = false
		cleanup := q.releaseUnexposed(ctx, ticket)
		return ctx, ticket, errors.Join(err, cleanup)
	}
	for i := 0; i < ticket.peers; i++ {
		ticket.remoteAttempted[i] = true
		reply, err := q.rpc(ctx, i, ticket.messages[i])
		if err != nil {
			return ctx, ticket, err
		}
		if reply.Operation == replication.PhysicalCreditRejected {
			q.rememberReply(i, reply)
			ticket.remoteRejected[i] = true
			ticket.active = false
			return ctx, ticket, errors.Join(replication.ErrPhysicalCredit, q.releaseUnexposed(ctx, ticket))
		}
		ticket.remoteKnown[i] = true
		q.mu.Lock()
		w := reply
		w.Operation = replication.PhysicalCreditWindow
		w.TicketID = [16]byte{}
		w.RequestDigest = [32]byte{}
		w.Cost = replication.PhysicalCreditCost{}
		q.peers[i].window = w
		q.mu.Unlock()
	}
	ticket.exposed = true
	return context.WithValue(ctx, haPeerPhysicalTicketKey{}, ticket), ticket, nil
}

// An unexposed context can never have authorized a source write. Actual source
// heads and the full original Member/Ordering cut must still prove no fact was
// produced before a known negative receipt or an unsent mirror is retired.
// A failed or cancelled RPC never enters this cleanup path by inference.
func (q *haPeerPhysicalController) releaseUnexposed(ctx context.Context, t *haPeerPhysicalTicket) error {
	if t == nil || t.self != t || t.owner != q || t.exposed {
		return replication.ErrPhysicalCredit
	}
	if err := q.requireUnchangedTicketSource(ctx, t); err != nil {
		return err
	}
	for i := 0; i < t.peers; i++ {
		if !t.localWritten[i] || t.completed[i] {
			continue
		}
		if t.remoteAttempted[i] && !t.remoteKnown[i] && !t.remoteRejected[i] {
			return replication.ErrPhysicalCredit
		}
		if t.remoteKnown[i] {
			request := t.messages[i]
			request.Operation = replication.PhysicalCreditRelease
			request.Cost = replication.PhysicalCreditCost{}
			reply, err := q.rpc(ctx, i, request)
			if err != nil {
				return err
			}
			if reply.Operation != replication.PhysicalCreditReleased {
				return replication.ErrPhysicalCredit
			}
			q.rememberReply(i, reply)
		}
		// Unsent slots and independently proved negative slots contain no remote
		// promise. A previous successful slot is released above using its real ACK.
		if err := q.releaseLocalTicket(ctx, t, i, replication.PhysicalCreditCost{}); err != nil {
			return err
		}
		t.completed[i] = true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.live == t {
		q.live = nil
	}
	return nil
}
func (q *haPeerPhysicalController) requireUnchangedTicketSource(ctx context.Context, t *haPeerPhysicalTicket) error {
	return q.owner.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return q.owner.withSelectedCut(ctx, func(_ context.Context, cut statecut.HASelectedCut) error {
			m := cut.Member
			if m.Term != t.startTerm || m.DurableIndex != t.startIndex || m.ConfirmedIndex != t.startIndex || m.AppliedIndex != t.startIndex || m.OrderingHeadMAC != t.startMAC || cut.Allowed != t.startAllowed || cut.Heads != t.startHeads {
				return replication.ErrPhysicalCredit
			}
			return nil
		})
	})
}
func (q *haPeerPhysicalController) rememberReply(i int, reply replication.PhysicalCreditRecord) {
	q.mu.Lock()
	defer q.mu.Unlock()
	reply.Operation = replication.PhysicalCreditWindow
	reply.TicketID = [16]byte{}
	reply.RequestDigest = [32]byte{}
	reply.Cost = replication.PhysicalCreditCost{}
	q.peers[i].window = reply
}
func (q *haPeerPhysicalController) actualTicketUsage(ctx context.Context, t *haPeerPhysicalTicket) (used, total replication.PhysicalCreditCost, err error) {
	err = q.owner.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return q.owner.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			m := cut.Member
			if m.Term != t.startTerm || m.DurableIndex != m.ConfirmedIndex || m.AppliedIndex != m.DurableIndex || m.DurableIndex < t.startIndex || m.DurableIndex-q.bankIndex > 256 {
				return replication.ErrPhysicalCredit
			}
			record, err := q.owner.replication.journal.Record(t.startIndex)
			if err != nil || record.MAC != t.startMAC {
				return errors.Join(replication.ErrPhysicalCredit, err)
			}
			if m.DurableIndex == q.bankIndex {
				return nil
			}
			permit, err := q.owner.owners.Components.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 20})
			if err != nil {
				return err
			}
			reconstructErr := replication.ReconstructRangeEach(m.ClusterID, m.Incarnation, q.bankIndex+1, m.DurableIndex, q.owner.replication.journal, q.owner.replication.source, func(commit replication.LocalCommit) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				frame, err := replication.UnmarshalFrame(commit.Encoded)
				if err != nil {
					return err
				}
				cost, err := haPeerPhysicalFrameCost(frame)
				if err != nil {
					return err
				}
				total, err = total.Add(cost)
				if err != nil {
					return err
				}
				if frame.Index > t.startIndex {
					used, err = used.Add(cost)
				}
				return err
			})
			return errors.Join(reconstructErr, permit.Release())
		})
	})
	if err != nil {
		used, total = replication.PhysicalCreditCost{}, replication.PhysicalCreditCost{}
	}
	return
}
func (q *haPeerPhysicalController) releaseLocalTicket(ctx context.Context, t *haPeerPhysicalTicket, i int, total replication.PhysicalCreditCost) error {
	var mirrored replication.PhysicalCreditCost
	for n := 0; n < t.peers; n++ {
		var err error
		mirrored, err = mirrored.Add(total)
		if err != nil {
			return err
		}
	}
	return q.owner.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return q.owner.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			if cut.Member.Term != t.startTerm || cut.Member.DurableIndex != cut.Member.ConfirmedIndex || cut.Member.AppliedIndex != cut.Member.DurableIndex {
				return replication.ErrPhysicalCredit
			}
			original, err := q.owner.physicalPublication.ObservePeerPhysicalPromisesFrozen(ctx)
			if err != nil {
				return err
			}
			found := false
			for _, old := range original.Tickets {
				if old.ID == t.messages[i].TicketID {
					if old.Window != t.messages[i].WindowID || old.Request != t.request {
						return replication.ErrPhysicalCredit
					}
					found = true
				}
			}
			if !found {
				if !t.exposed && !t.remoteKnown[i] {
					return nil
				}
				return replication.ErrPhysicalCredit
			}
			return q.owner.physicalPublication.RecordPeerFutureReleaseFrozen(ctx, t.messages[i].WindowID, t.messages[i].TicketID, t.used, mirrored)
		})
	})
}

// finishTicket may run only after the synchronous original completion owner
// has joined. Revocation precedes the genuine all-peer application wait; an
// unknown wait leaves the persistent promises and the original ticket owner.
func (q *haPeerPhysicalController) finishTicket(ctx context.Context, t *haPeerPhysicalTicket) error {
	if t == nil || t.self != t || t.owner != q {
		return replication.ErrPhysicalCredit
	}
	t.mu.Lock()
	t.active = false
	t.mu.Unlock()
	if !t.exposed {
		return q.releaseUnexposed(ctx, t)
	}
	c := q.owner.reopened.Owners.Components
	if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		head, _, err := c.Ledger.ReadPosition(ctx)
		if err != nil {
			return err
		}
		target, err := q.owner.replication.coordinator.CaptureRecoveryTarget(ctx, replication.StoreCursor{Generation: head.Generation, Sequence: head.Sequence})
		if err != nil {
			return err
		}
		return q.owner.replication.coordinator.WaitRecoveryAppliedTarget(ctx, target)
	}); err != nil {
		return err
	}
	used, total, err := q.actualTicketUsage(ctx, t)
	if err != nil {
		return err
	}
	if !t.cost.Covers(used) {
		return replication.ErrPhysicalCredit
	}
	t.used = used
	for i := 0; i < t.peers; i++ {
		if t.completed[i] {
			continue
		}
		request := t.messages[i]
		request.Operation = replication.PhysicalCreditRelease
		request.Cost = used
		reply, err := q.rpc(ctx, i, request)
		if err != nil {
			return err
		}
		if reply.Operation != replication.PhysicalCreditReleased {
			return replication.ErrPhysicalCredit
		}
		q.rememberReply(i, reply)
		if err := q.releaseLocalTicket(ctx, t, i, total); err != nil {
			return err
		}
		t.completed[i] = true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.live == t {
		q.live = nil
	}
	return nil
}
func (q *haPeerPhysicalController) closeJoined(ctx context.Context) error {
	if err := q.lockEntryContext(ctx); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	defer q.entry.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closing = true
	if q.live != nil {
		return errors.Join(ownedfile.ErrRetirement, fmt.Errorf("original peer ticket remains owned: %w", replication.ErrPhysicalCredit))
	}
	for _, p := range q.peers {
		if p.waiting {
			return errors.Join(ownedfile.ErrRetirement, replication.ErrPhysicalCredit)
		}
	}
	if q.fee == (resourcebudget.Permit{}) {
		return q.failure
	}
	if err := q.fee.Release(); err != nil {
		q.failure = errors.Join(q.failure, err)
		return errors.Join(ownedfile.ErrRetirement, q.failure)
	}
	q.fee = resourcebudget.Permit{}
	return q.failure
}

// This gate adds exclusion, never a Member/native/role capability. Invoke it
// OUTSIDE Member/Ordering freezes. New reserves refuse before IO; known releases
// remain permitted so an already admitted request can drain its original debt.
// Replica maintenance additionally joins the actual receiver/applier mutexes.
func (q *haPeerPhysicalController) withAdmissionPaused(ctx context.Context, visit func(context.Context) error) error {
	if q == nil || ctx == nil || visit == nil {
		return replication.ErrPhysicalCredit
	}
	if err := q.lockEntryContext(ctx); err != nil {
		return err
	}
	defer q.entry.Unlock()
	q.mu.Lock()
	invalid := q.closing || q.failure != nil || q.live != nil
	q.mu.Unlock()
	if invalid {
		return replication.ErrPhysicalCredit
	}
	qualify := func(ctx context.Context) error {
		return q.owner.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			return q.owner.withSelectedCut(ctx, func(ctx context.Context, _ statecut.HASelectedCut) error {
				return statecut.RequireNoHAOrderingPeerPromises(ctx, q.owner.owners, q.owner.physicalPublication)
			})
		})
	}
	if err := qualify(ctx); err != nil {
		return err
	}
	if q.primary {
		return q.visitPhysicalPause(ctx, replication.ReplicaConfirmedPosition{}, replication.Acknowledgement{}, visit)
	}
	return q.owner.replication.applier.WithPhysicalReceiveReadBarrier(ctx, func(ctx context.Context, p replication.ReplicaConfirmedPosition, ack replication.Acknowledgement) error {
		if err := qualify(ctx); err != nil {
			return err
		}
		return q.visitPhysicalPause(ctx, p, ack, visit)
	})
}
func (q *haPeerPhysicalController) lockEntryContext(ctx context.Context) error {
	if ctx == nil {
		return replication.ErrPhysicalCredit
	}
	for !q.entry.TryLock() {
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-q.owner.ctx.Done():
			timer.Stop()
			return q.owner.ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		q.entry.Unlock()
		return err
	}
	return nil
}
