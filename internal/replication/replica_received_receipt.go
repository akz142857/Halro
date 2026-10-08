package replication

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const receiveReceiptHeaderBytes = 2048
const receiveReceiptSlotBytes = 3072
const receivedRecoveryRAMBytes int64 = 32 << 20

type receivedOrderingIntent struct {
	Before  [32]byte
	Primary string
	Record  []byte
	MAC     []byte
}

func decodePhysicalPublicationReceipt(raw []byte, r *physicalPublicationReceipt) error {
	if len(raw) == int(physicalPublicationReceiptBytes) {
		raw = raw[:receiveReceiptHeaderBytes]
	}
	if len(raw) >= physicalRenewStaticBytes && bytes.Equal(raw[:8], []byte("HLRPUB03")) {
		return decodePhysicalReceiptV3(raw[:physicalRenewStaticBytes], r)
	}
	// V3 keeps immutable source IO identity in the first 1024B. Its next
	// 1024B are two separately MACed window slots; accepted intent slots retain
	// the original immutable receipt MAC and never borrow renewal permission.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(r); err != nil {
		return err
	}
	if r.Version == 3 {
		return ErrMemberAuthority
	}
	return json.Unmarshal(raw, r)
}
func encodePhysicalPublicationReceipt(r physicalPublicationReceipt) ([]byte, error) {
	if r.Version == 3 {
		header, err := encodePhysicalReceiptV3(r)
		if err != nil {
			return nil, err
		}
		out := make([]byte, physicalPublicationReceiptBytes)
		copy(out, header)
		return out, nil
	}
	raw, err := json.Marshal(r)
	limit := receiveReceiptHeaderBytes
	if r.Version == 3 {
		limit = physicalRenewStaticBytes
	}
	if err != nil || len(raw) > limit {
		return nil, errors.Join(ErrMemberAuthority, err)
	}
	out := make([]byte, physicalPublicationReceiptBytes)
	copy(out, bytes.Repeat([]byte{' '}, receiveReceiptHeaderBytes))
	copy(out, raw)
	if r.Version == 3 {
		clear(out[physicalRenewStaticBytes:receiveReceiptHeaderBytes])
	}
	return out, nil
}
func receivedIntentMAC(i receivedOrderingIntent, r physicalPublicationReceipt, key []byte) ([]byte, error) {
	i.MAC = nil
	raw, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("halro:replica:accepted-ordering:v1\x00"))
	h.Write(r.MAC)
	h.Write(raw)
	return h.Sum(nil), nil
}
func memberReceiptDigest(m MemberState, key []byte) ([32]byte, error) {
	raw, err := MarshalState(m, key)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// The descriptor belongs to the original immutable receipt birth. Opening it
// neither authorizes a progress change nor trusts an accepted candidate slot.
func (o *PhysicalPublication) openReceiveReceipt(path string, recovering bool) (resultErr error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	o.receiptFile = f
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	o.receiptOwner, err = ownedfile.RestoreAuthenticated(path, f, info, parent)
	if err != nil {
		return err
	}
	id, err := resourcebudget.CapturePhysicalIdentity(f)
	if err != nil {
		return err
	}
	if o.receipt.Version == 1 {
		if recovering {
			return ErrMemberAuthority
		}
		// Only equal independently authenticated Member/Ordering may upgrade a
		// legacy IO receipt. The same actual descriptor is retained; no old intent
		// is synthesized and no absent progress receives permission.
		o.receipt.Version = 2
		o.receipt.Receipt = id
		o.receipt.MAC, err = physicalReceiptMAC(o.receipt, o.publisher.key[:])
		if err != nil {
			return err
		}
		raw, err := encodePhysicalPublicationReceipt(o.receipt)
		if err != nil {
			return err
		}
		if n, e := f.WriteAt(raw, 0); e != nil || n != len(raw) {
			return o.fail(errors.Join(ownedfile.ErrRetirement, e, io.ErrShortWrite))
		}
		if err = f.Sync(); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
	} else if id != o.receipt.Receipt || info.Size() != physicalPublicationReceiptBytes {
		return ownedfile.ErrReplaced
	}
	return errors.Join(o.receiptOwner.VerifyName(), o.receiptOwner.VerifyFile(f))
}

// persistReceivedIntent runs only after the original Receiver has validated
// the complete authenticated frame and acquired its real Ordering escrow, but
// before the first native write. Two fixed slots preserve the last successful
// accepted proof across a torn next write without changing the static header.
func (r *ReplicaReceiver) persistReceivedIntent(record OrderingRecord, reservation *OrderingPhysicalReservation) error {
	if reservation == nil {
		r.journal.mu.Lock()
		owner := r.journal.received
		r.journal.mu.Unlock()
		if owner == nil {
			return nil
		}
		return owner.persist(r, record)
	}
	o := reservation.owner
	p, j := o.publisher, o.journal
	p.mu.Lock()
	defer p.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if p.closed || p.authorityFault != nil || p.physical != o || j.physical != o || j.closed || j.closing || j.poisoned != nil || o.failure() != nil || o.withdrawn.Load() || reservation.used || reservation.released || reservation.term != r.term || (o.receipt.Version != 2 && o.receipt.Version != 3) || o.receiptFile == nil || o.receiptOwner == nil {
		return ErrMemberAuthority
	}
	m := p.state
	if m.Role != RoleReplica || m.ClusterID != r.clusterID || m.Incarnation != r.incarnation || m.NodeID != r.nodeID || m.Term != r.term || m.PromisedTerm != r.promisedTerm || m.DurableIndex != r.durableIndex || m.ConfirmedIndex != r.confirmedIndex || m.AppliedIndex != r.appliedIndex || m.Projection != r.projection || m.OrderingHeadMAC != j.head || m.DurableIndex != j.lastIndex || record.Index != m.DurableIndex+1 || r.primaryNodeID == "" || r.primaryNodeID == m.NodeID {
		return ErrMemberAuthority
	}
	if err := p.readAuthoritySource(m); err != nil {
		return err
	}
	if err := errors.Join(o.receiptOwner.VerifyName(), o.receiptOwner.VerifyFile(o.receiptFile)); err != nil {
		return err
	}
	record.PreviousMAC = j.head
	encoded, err := record.MarshalBinary(j.key[:], m.ClusterID, m.Incarnation)
	if err != nil {
		return err
	}
	before, err := memberReceiptDigest(m, p.key[:])
	if err != nil {
		return err
	}
	intent := receivedOrderingIntent{Before: before, Primary: r.primaryNodeID, Record: encoded}
	intent.MAC, err = receivedIntentMAC(intent, o.receipt, p.key[:])
	if err != nil {
		return err
	}
	raw, err := json.Marshal(intent)
	if err != nil || len(raw) > receiveReceiptSlotBytes-4 {
		return errors.Join(ErrMemberAuthority, err)
	}
	slot := make([]byte, receiveReceiptSlotBytes)
	binary.LittleEndian.PutUint32(slot, uint32(len(raw)))
	copy(slot[4:], raw)
	offset := int64(receiveReceiptHeaderBytes) + int64(record.Index%2)*receiveReceiptSlotBytes
	if n, e := o.receiptFile.WriteAt(slot, offset); e != nil || n != len(slot) {
		return o.fail(errors.Join(ownedfile.ErrRetirement, e, io.ErrShortWrite))
	}
	if err = o.receiptFile.Sync(); err != nil {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err))
	}
	if o.phase != nil {
		return o.phase("receive-intent-synced")
	}
	return nil
}

// ReplicaReceivedOrderingAuthority is private provenance for exactly one
// previously accepted, fully durable native/Ordering frame. It grants no root,
// ordinary read, Applied watermark, role change or provider admission.
type ReplicaReceivedOrderingAuthority struct {
	mu             sync.Mutex
	publisher      *StatePublisher
	journal        *OrderingJournal
	source         *NativeSource
	baseline       *MetadataBaselineAuthority
	manager        *resourcebudget.Manager
	before         MemberState
	record         OrderingRecord
	receipt        physicalPublicationReceipt
	primary        string
	encoded        []byte
	work           resourcebudget.Permit
	portable       bool
	portableHeader portableReceivedHeader
	used           atomic.Bool
}

func (a *ReplicaReceivedOrderingAuthority) validateLocked(p *StatePublisher, j *OrderingJournal, m *resourcebudget.Manager) error {
	if a == nil || a.used.Load() || a.publisher != p || a.journal != j || a.manager != m || p.closed || p.authorityFault != nil || j.closed || j.closing || j.poisoned != nil || !sameHAPeerLeadershipMember(a.before, p.state) || p.state.Role != RoleReplica || j.lastIndex != p.state.DurableIndex+1 || j.lastTerm != p.state.Term || j.head != a.record.MAC {
		return ErrMemberAuthority
	}
	id, err := captureReceivedBirth(j.file)
	if err != nil || id != a.receipt.Ordering {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return nil
}
func (p *StatePublisher) PreparePhysicalPublicationForReceived(ctx context.Context, j *OrderingJournal, m *resourcebudget.Manager, o PhysicalPublicationOptions, a *ReplicaReceivedOrderingAuthority) (*PhysicalPublication, error) {
	if a == nil || a.portable || !o.Restart {
		return nil, ErrMemberAuthority
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return p.preparePhysicalPublication(ctx, j, m, o, a)
}

func decodeReceivedIntent(raw []byte, index uint64, r physicalPublicationReceipt, key []byte) (receivedOrderingIntent, error) {
	var intent receivedOrderingIntent
	if (r.Version != 2 && r.Version != 3) || len(raw) != int(physicalPublicationReceiptBytes) {
		return intent, ErrMemberAuthority
	}
	start := receiveReceiptHeaderBytes + int(index%2)*receiveReceiptSlotBytes
	slot := raw[start : start+receiveReceiptSlotBytes]
	n := int(binary.LittleEndian.Uint32(slot))
	if n < 1 || n > len(slot)-4 {
		return intent, ErrMemberAuthority
	}
	decoder := json.NewDecoder(bytes.NewReader(slot[4 : 4+n]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return intent, err
	}
	mac, err := receivedIntentMAC(intent, r, key)
	if err != nil || !hmac.Equal(mac, intent.MAC) {
		return intent, errors.Join(ErrMemberAuthority, err)
	}
	return intent, nil
}

// QualifyReplicaReceivedOrdering reauthenticates the original Member MAC, both
// actual receipt/journal births, exact accepted Ordering bytes and all native
// history. The current primary is supplied by the independently signed
// Metadata baseline, never inferred from this candidate receipt.
func QualifyReplicaReceivedOrdering(ctx context.Context, p *StatePublisher, j *OrderingJournal, s *NativeSource, baseline *MetadataBaselineAuthority, m *resourcebudget.Manager) (*ReplicaReceivedOrderingAuthority, error) {
	return qualifyReplicaReceivedOrdering(ctx, p, j, s, baseline, m, false)
}
func QualifyPortableReplicaReceivedOrdering(ctx context.Context, p *StatePublisher, j *OrderingJournal, s *NativeSource, baseline *MetadataBaselineAuthority, m *resourcebudget.Manager) (*ReplicaReceivedOrderingAuthority, error) {
	return qualifyReplicaReceivedOrdering(ctx, p, j, s, baseline, m, true)
}
func qualifyReplicaReceivedOrdering(ctx context.Context, p *StatePublisher, j *OrderingJournal, s *NativeSource, baseline *MetadataBaselineAuthority, m *resourcebudget.Manager, portable bool) (a *ReplicaReceivedOrderingAuthority, resultErr error) {
	if ctx == nil || p == nil || j == nil || s == nil || baseline == nil || m == nil {
		return nil, ErrMemberAuthority
	}
	fee, err := m.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: receivedRecoveryRAMBytes})
	if err != nil {
		return nil, err
	}
	a = &ReplicaReceivedOrderingAuthority{publisher: p, journal: j, source: s, baseline: baseline, manager: m, work: fee, portable: portable}
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				resultErr = &ReplicaReceivedOrderingFailure{Owner: a, Cause: resultErr}
				return
			}
			resultErr = errors.Join(resultErr, a.Close())
			if !errors.Is(resultErr, ownedfile.ErrRetirement) {
				a = nil
			} else {
				resultErr = &ReplicaReceivedOrderingFailure{Owner: a, Cause: resultErr}
			}
		}
	}()
	p.mu.Lock()
	j.mu.Lock()
	err = func() error {
		if p.closed || p.authorityFault != nil || p.physical != nil || j.closed || j.closing || j.poisoned != nil || j.physical != nil || p.state.Role != RoleReplica || p.state.Term != p.state.PromisedTerm || j.lastIndex != p.state.DurableIndex+1 || j.lastTerm != p.state.Term {
			return ErrMemberAuthority
		}
		if err := p.readAuthoritySource(p.state); err != nil {
			return err
		}
		a.before = cloneMemberState(p.state)
		if err := baseline.ValidateMember(a.before); err != nil {
			return err
		}
		var raw []byte
		var receiptID resourcebudget.PhysicalIdentity
		var err error
		if portable {
			raw, receiptID, err = readPortableReceivedReceiptIdentity(portableReceivedPath(p))
			if err != nil {
				return err
			}
			decoder := json.NewDecoder(bytes.NewReader(raw[:receiveReceiptHeaderBytes]))
			decoder.DisallowUnknownFields()
			if err = decoder.Decode(&a.portableHeader); err != nil {
				return err
			}
			mac, e := portableReceivedMAC(a.portableHeader, p.key[:])
			if e != nil || !hmac.Equal(mac, a.portableHeader.MAC) || a.portableHeader.Version != 1 || a.portableHeader.Receipt != receiptID || a.portableHeader.ClusterID != p.state.ClusterID || a.portableHeader.Incarnation != p.state.Incarnation || a.portableHeader.NodeID != p.state.NodeID || a.portableHeader.Baseline != baseline.Descriptor().AuthorityDigest {
				return errors.Join(ErrMemberAuthority, e)
			}
			a.receipt = a.portableHeader.intentHeader()
		} else {
			raw, receiptID, err = readPhysicalReceiptIdentity(p.path + ".physical.json")
			if err != nil {
				return err
			}
			if err = decodePhysicalPublicationReceipt(raw, &a.receipt); err != nil {
				return err
			}
			mac, e := physicalReceiptMAC(a.receipt, p.key[:])
			if e != nil || !hmac.Equal(mac, a.receipt.MAC) || (a.receipt.Version != 2 && a.receipt.Version != 3) || a.receipt.Receipt != receiptID || a.receipt.ClusterID != p.state.ClusterID || a.receipt.Incarnation != p.state.Incarnation || a.receipt.NodeID != p.state.NodeID {
				return errors.Join(ErrMemberAuthority, e)
			}
		}
		// The full captured native/journal bytes remain the evidence. The persisted
		// header alone is never a progress authority.
		id, err := captureReceivedBirth(j.file)
		if err != nil || id != a.receipt.Ordering {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		intent, err := decodeReceivedIntent(raw, j.lastIndex, a.receipt, p.key[:])
		if err != nil {
			return err
		}
		before, err := memberReceiptDigest(p.state, p.key[:])
		if err != nil || before != intent.Before || intent.Primary != baseline.CurrentPrimary() {
			return errors.Join(ErrMemberAuthority, err)
		}
		record, err := UnmarshalOrderingRecord(intent.Record, j.key[:], p.state.ClusterID, p.state.Incarnation, p.state.OrderingHeadMAC)
		if err != nil {
			return err
		}
		actual, err := j.recordLocked(j.lastIndex)
		if err != nil {
			return err
		}
		actualRaw, err := actual.MarshalBinary(j.key[:], p.state.ClusterID, p.state.Incarnation)
		if err != nil || !bytes.Equal(actualRaw, intent.Record) || record.Index != j.lastIndex || record.Term != p.state.Term || record.MAC != j.head {
			return errors.Join(ErrMemberAuthority, err)
		}
		a.record = record
		a.primary = intent.Primary
		return nil
	}()
	j.mu.Unlock()
	p.mu.Unlock()
	if err != nil {
		return a, err
	}
	s.mu.RLock()
	valid := !s.closed && s.retirement.Load() == nil && s.options.ReaderBudget == m && s.options.MetadataBaseline != nil && s.options.MetadataBaseline.Authority == baseline && s.metadataBaselineActive
	s.mu.RUnlock()
	if !valid {
		return a, ErrMemberAuthority
	}
	if err = ReconstructRangeEach(a.before.ClusterID, a.before.Incarnation, 1, a.record.Index, j, s, func(commit LocalCommit) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if commit.Index == a.record.Index {
			a.encoded = commit.Encoded
		}
		return nil
	}); err != nil {
		return a, err
	}
	cursors, err := j.StoreCursors()
	if err != nil {
		return a, err
	}
	for store := StoreLedger; store <= StoreMetadata; store++ {
		cursor, err := s.Cursor(store)
		if err != nil || cursor != cursors[store-StoreLedger] {
			return a, errors.Join(ErrMemberAuthority, err)
		}
		if _, err = s.HeadAt(store, cursor); err != nil {
			return a, err
		}
	}
	p.mu.Lock()
	j.mu.Lock()
	err = a.validateLocked(p, j, m)
	j.mu.Unlock()
	p.mu.Unlock()
	return a, errors.Join(err, ctx.Err())
}

// Recover uses the original Receiver duplicate path and publisher. Applied and
// projection remain exactly the independently authenticated old tuple.
func (a *ReplicaReceivedOrderingAuthority) Recover(ctx context.Context, receiver *ReplicaReceiver) error {
	if ctx == nil {
		return ErrMemberAuthority
	}
	if a == nil || receiver == nil || ctx.Err() != nil {
		return errors.Join(ErrMemberAuthority, ctx.Err())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p, j := a.publisher, a.journal
	p.mu.Lock()
	j.mu.Lock()
	err := a.validateLocked(p, j, a.manager)
	if err == nil {
		if a.portable {
			o := p.received
			if o == nil || j.received != o || o.publisher != p || o.journal != j || o.manager != a.manager || o.header.Receipt != a.receipt.Receipt || !hmac.Equal(o.header.MAC, a.receipt.MAC) || o.closed || o.fault != nil {
				err = ErrMemberAuthority
			}
		} else {
			o := p.physical
			if o == nil || j.physical != o || o.publisher != p || o.journal != j || o.manager != a.manager || (o.receipt.Version != 2 && o.receipt.Version != 3) || o.receipt.Receipt != a.receipt.Receipt || !hmac.Equal(o.receipt.MAC, a.receipt.MAC) || o.banksClosed || o.withdrawn.Load() || o.failure() != nil {
				err = ErrMemberAuthority
			}
		}
	}
	j.mu.Unlock()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	receiver.mu.Lock()
	valid := receiver.journal == j && receiver.clusterID == a.before.ClusterID && receiver.incarnation == a.before.Incarnation && receiver.nodeID == a.before.NodeID && receiver.primaryNodeID == a.primary && receiver.term == a.before.Term && receiver.promisedTerm == a.before.PromisedTerm && receiver.confirmedIndex == a.before.ConfirmedIndex && receiver.appliedIndex == a.before.AppliedIndex && receiver.projection == a.before.Projection && receiver.persistState != nil
	receiver.mu.Unlock()
	if !valid {
		return ErrMemberAuthority
	}
	if _, err = receiver.Receive(a.encoded); err != nil {
		return err
	}
	state := p.Snapshot()
	if state.DurableIndex != a.record.Index || state.ConfirmedIndex != a.record.Index || state.OrderingHeadMAC != a.record.MAC || state.AppliedIndex != a.before.AppliedIndex || state.Projection != a.before.Projection {
		return ErrMemberAuthority
	}
	a.used.Store(true)
	return a.closeLocked()
}
func (a *ReplicaReceivedOrderingAuthority) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closeLocked()
}
func (a *ReplicaReceivedOrderingAuthority) closeLocked() error {
	a.used.Store(true)
	clear(a.encoded)
	a.encoded = nil
	if err := a.work.Release(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	a.work = resourcebudget.Permit{}
	return nil
}

type ReplicaReceivedOrderingFailure struct {
	Owner *ReplicaReceivedOrderingAuthority
	Cause error
}

func (e *ReplicaReceivedOrderingFailure) Error() string { return e.Cause.Error() }
func (e *ReplicaReceivedOrderingFailure) Unwrap() error { return e.Cause }

// Observations are accepted only while the caller holds the original actual
// Member and Ordering freezes. This explicit cold path never mints a normal
// selected-operation context, nor a confirmed/applied prefix.
func (a *ReplicaReceivedOrderingAuthority) ValidateObservedOwners(p *StatePublisher, j *OrderingJournal, s *NativeSource, b *MetadataBaselineAuthority, m *resourcebudget.Manager, member MemberState, head FrozenOrderingHead) error {
	if a == nil {
		return ErrMemberAuthority
	}
	if a.used.Load() || a.publisher != p || a.journal != j || a.source != s || a.manager != m || b == nil || b != a.baseline || b.ValidateMember(member) != nil || !sameHAPeerLeadershipMember(a.before, member) || head.Index != a.record.Index || head.Term != member.Term || head.MAC != a.record.MAC || head.Header.ClusterID != member.ClusterID || head.Header.Incarnation != member.Incarnation {
		return ErrMemberAuthority
	}
	return nil
}
