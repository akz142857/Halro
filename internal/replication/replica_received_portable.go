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
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const PortableReceivedDiskBytes int64 = 8192
const portableReceivedOpaqueSlots = 4

// Includes every retained opaque file. The fixed bank is still exactly 8192B.
const PortableReceivedBankDiskBytes int64 = (1 + portableReceivedOpaqueSlots) * PortableReceivedDiskBytes

type PortableReceivedPublicationOptions struct {
	Restart bool
	phase   func(string) error
}

const portableReceivedRAMBytes int64 = 4*MaxMemberStateJSON + 8192

type portableReceivedHeader struct {
	Version                        int
	ClusterID, Incarnation, NodeID string
	Baseline                       [32]byte
	Ordering, Receipt              resourcebudget.PhysicalIdentity
	MAC                            []byte
}

// This owner proves only that the originating authenticated Receiver recorded
// its exact intent before native IO. It supplies no physical allocation, root,
// role, term, readiness or Applied permission.
type PortableReceivedPublication struct {
	primary    string
	publisher  *StatePublisher
	journal    *OrderingJournal
	baseline   *MetadataBaselineAuthority
	manager    *resourcebudget.Manager
	header     portableReceivedHeader
	file       *os.File
	owner      *ownedfile.Owner
	work, disk resourcebudget.Permit
	fault      error
	closed     bool
}

func portableReceivedMAC(h portableReceivedHeader, key []byte) ([]byte, error) {
	h.MAC = nil
	raw, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:replica:received-descriptor:v1\x00"))
	mac.Write(raw)
	return mac.Sum(nil), nil
}
func (h portableReceivedHeader) intentHeader() physicalPublicationReceipt {
	return physicalPublicationReceipt{Version: 2, ClusterID: h.ClusterID, Incarnation: h.Incarnation, NodeID: h.NodeID, Ordering: h.Ordering, Receipt: h.Receipt, MAC: h.MAC}
}
func portableReceivedPath(p *StatePublisher) string { return p.path + ".received.json" }

// PreparePortableReceivedPublication is called by the selected startup owner
// after independent common/source qualification and before transport exposure.
// Existing immutable receipt births must match this actual journal and signed
// baseline. Missing or foreign contents are never upgraded into accepted proof.
func (p *StatePublisher) PreparePortableReceivedPublication(ctx context.Context, j *OrderingJournal, s *NativeSource, b *MetadataBaselineAuthority, m *resourcebudget.Manager, disk resourcebudget.Permit) (*PortableReceivedPublication, error) {
	return p.preparePortableReceivedPublication(ctx, j, s, b, m, disk, nil, PortableReceivedPublicationOptions{})
}

// WithOptions is an explicit stopped-owner retry. It can preserve an incomplete
// bank only while the real Member/Ordering/full native cut is equal; an orphan
// durable record always requires the independently authenticated accepted proof.
func (p *StatePublisher) PreparePortableReceivedPublicationWithOptions(ctx context.Context, j *OrderingJournal, s *NativeSource, b *MetadataBaselineAuthority, m *resourcebudget.Manager, disk resourcebudget.Permit, options PortableReceivedPublicationOptions) (*PortableReceivedPublication, error) {
	return p.preparePortableReceivedPublication(ctx, j, s, b, m, disk, nil, options)
}
func (p *StatePublisher) PreparePortableReceivedPublicationForReceived(ctx context.Context, j *OrderingJournal, s *NativeSource, b *MetadataBaselineAuthority, m *resourcebudget.Manager, disk resourcebudget.Permit, a *ReplicaReceivedOrderingAuthority) (*PortableReceivedPublication, error) {
	if a == nil || !a.portable {
		return nil, ErrMemberAuthority
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return p.preparePortableReceivedPublication(ctx, j, s, b, m, disk, a, PortableReceivedPublicationOptions{})
}
func (p *StatePublisher) preparePortableReceivedPublication(ctx context.Context, j *OrderingJournal, s *NativeSource, b *MetadataBaselineAuthority, m *resourcebudget.Manager, disk resourcebudget.Permit, a *ReplicaReceivedOrderingAuthority, options PortableReceivedPublicationOptions) (o *PortableReceivedPublication, resultErr error) {
	if ctx == nil || p == nil || j == nil || s == nil || b == nil || m == nil {
		return nil, ErrMemberAuthority
	}

	s.mu.RLock()
	valid := !s.closed && s.retirement.Load() == nil && s.options.ReaderBudget == m && s.options.MetadataBaseline != nil && s.options.MetadataBaseline.Authority == b && s.metadataBaselineActive
	s.mu.RUnlock()
	if !valid {
		return nil, ErrMemberAuthority
	}
	before := p.Snapshot()
	if err := b.ValidateMember(before); err != nil {
		return nil, err
	}
	cursors, err := j.StoreCursors()
	if err != nil {
		return nil, err
	}
	for store := StoreLedger; store <= StoreMetadata; store++ {
		cursor, e := s.Cursor(store)
		if e != nil || cursor != cursors[store-StoreLedger] {
			return nil, errors.Join(ErrMemberAuthority, e)
		}
		if _, e = s.HeadAt(store, cursor); e != nil {
			return nil, e
		}
	}
	heldDisk, err := disk.RetainCovering(m, resourcebudget.Cost{Disk: PortableReceivedBankDiskBytes})
	if err != nil {
		return nil, err
	}
	work, err := m.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: portableReceivedRAMBytes})
	if err != nil {
		return nil, errors.Join(err, heldDisk.Release())
	}
	o = &PortableReceivedPublication{publisher: p, journal: j, baseline: b, manager: m, work: work, disk: heldDisk, primary: b.CurrentPrimary()}
	p.mu.Lock()
	j.mu.Lock()
	defer j.mu.Unlock()
	defer p.mu.Unlock()
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				o.fault = errors.Join(o.fault, resultErr)
			}
			if e := o.closeLocked(); e != nil {
				p.authorityFault = errors.Join(p.authorityFault, e)
				p.received, j.received = o, o
				resultErr = &PortableReceivedFailure{Owner: o, Cause: errors.Join(resultErr, e)}
			} else {
				o = nil
			}
		}
	}()
	if p.closed || p.authorityFault != nil || p.physical != nil || p.received != nil || j.closed || j.closing || j.poisoned != nil || j.physical != nil || j.received != nil || p.state.Role != RoleReplica || p.state.Term != p.state.PromisedTerm || !sameHAPeerLeadershipMember(before, p.state) || ((p.state.DurableIndex != j.lastIndex || p.state.OrderingHeadMAC != j.head) && (a == nil || a.validateLocked(p, j, m) != nil)) {
		return o, ErrMemberAuthority
	}
	if err = ctx.Err(); err != nil {
		return o, err
	}
	if err = p.readAuthoritySource(p.state); err != nil {
		return o, err
	}
	orderingID, err := captureReceivedBirth(j.file)
	if err != nil {
		return o, err
	}
	path := portableReceivedPath(p)
	if err = checkPortableReceivedInventory(path); err != nil {
		return o, err
	}
	if options.Restart {
		if a != nil || p.state.DurableIndex != p.state.ConfirmedIndex || p.state.AppliedIndex != p.state.DurableIndex || p.state.DurableIndex != j.lastIndex || p.state.OrderingHeadMAC != j.head {
			return o, ErrMemberAuthority
		}
		if err = o.preserveUnqualified(ctx, path, orderingID); err != nil {
			return o, err
		}
	}
	var file *os.File
	var owner *ownedfile.Owner
	if a != nil {
		err = os.ErrExist
	} else {
		file, owner, err = ownedfile.Create(path, os.O_RDWR, 0600)
	}
	if errors.Is(err, os.ErrExist) {
		file, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return o, err
		}
		o.file = file
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() != PortableReceivedDiskBytes {
			return o, errors.Join(ErrMemberAuthority, e)
		}
		raw := make([]byte, receiveReceiptHeaderBytes)
		if _, e = file.ReadAt(raw, 0); e != nil {
			return o, e
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if e = decoder.Decode(&o.header); e != nil {
			return o, e
		}
		receiptID, e := captureReceivedBirth(file)
		if e != nil {
			return o, e
		}
		mac, e := portableReceivedMAC(o.header, p.key[:])
		if e != nil || !hmac.Equal(mac, o.header.MAC) || o.header.Version != 1 || o.header.Receipt != receiptID || o.header.Ordering != orderingID || o.header.ClusterID != before.ClusterID || o.header.Incarnation != before.Incarnation || o.header.NodeID != before.NodeID || o.header.Baseline != b.Descriptor().AuthorityDigest {
			return o, errors.Join(ErrMemberAuthority, e)
		}
		parent, e := os.Lstat(filepath.Dir(path))
		if e != nil {
			return o, e
		}
		o.owner, e = ownedfile.RestoreAuthenticated(path, file, info, parent)
		if e != nil {
			return o, e
		}
	} else {
		if err != nil {
			return o, err
		}
		o.file, o.owner = file, owner
		if options.phase != nil {
			if e := options.phase("received-created"); e != nil {
				return o, e
			}
		}
		receiptID, e := captureReceivedBirth(file)
		if e != nil {
			return o, e
		}
		o.header = portableReceivedHeader{Version: 1, ClusterID: before.ClusterID, Incarnation: before.Incarnation, NodeID: before.NodeID, Baseline: b.Descriptor().AuthorityDigest, Ordering: orderingID, Receipt: receiptID}
		o.header.MAC, e = portableReceivedMAC(o.header, p.key[:])
		if e != nil {
			return o, e
		}
		header, e := json.Marshal(o.header)
		if e != nil || len(header) > receiveReceiptHeaderBytes {
			return o, errors.Join(ErrMemberAuthority, e)
		}
		raw := make([]byte, PortableReceivedDiskBytes)
		copy(raw, bytes.Repeat([]byte{' '}, receiveReceiptHeaderBytes))
		copy(raw, header)
		if n, e := file.WriteAt(raw, 0); e != nil || n != len(raw) {
			o.fault = errors.Join(ownedfile.ErrRetirement, e, io.ErrShortWrite)
			return o, o.fault
		}
		if options.phase != nil {
			if e := options.phase("received-header-written"); e != nil {
				return o, e
			}
		}
		if e = file.Sync(); e != nil {
			o.fault = errors.Join(ownedfile.ErrRetirement, e)
			return o, o.fault
		}
		if e = durable.SyncDirectory(filepath.Dir(path)); e != nil {
			o.fault = errors.Join(ownedfile.ErrRetirement, e)
			return o, o.fault
		}
	}
	if options.phase != nil {
		if e := options.phase("received-synced"); e != nil {
			return o, e
		}
	}
	if a != nil && (o.header.Receipt != a.receipt.Receipt || !hmac.Equal(o.header.MAC, a.receipt.MAC)) {
		return o, ErrMemberAuthority
	}
	if err = errors.Join(o.owner.VerifyFile(o.file), ctx.Err()); err != nil {
		return o, err
	}
	p.received, j.received = o, o
	return o, nil
}

func (o *PortableReceivedPublication) persist(r *ReplicaReceiver, record OrderingRecord) error {
	p, j := o.publisher, o.journal
	p.mu.Lock()
	defer p.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if p.closed || p.authorityFault != nil || p.received != o || j.received != o || o.closed || o.fault != nil || j.closed || j.closing || j.poisoned != nil || p.physical != nil || j.physical != nil || o.file == nil || o.owner == nil {
		return ErrMemberAuthority
	}
	m := p.state
	if m.Role != RoleReplica || m.ClusterID != r.clusterID || m.Incarnation != r.incarnation || m.NodeID != r.nodeID || m.Term != r.term || m.PromisedTerm != r.promisedTerm || m.DurableIndex != r.durableIndex || m.ConfirmedIndex != r.confirmedIndex || m.AppliedIndex != r.appliedIndex || m.Projection != r.projection || m.DurableIndex != j.lastIndex || m.OrderingHeadMAC != j.head || record.Index != m.DurableIndex+1 || r.primaryNodeID != o.primary {
		return ErrMemberAuthority
	}
	if err := p.readAuthoritySource(m); err != nil {
		return err
	}
	if err := o.owner.VerifyFile(o.file); err != nil {
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
	intent.MAC, err = receivedIntentMAC(intent, o.header.intentHeader(), p.key[:])
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
	if n, e := o.file.WriteAt(slot, offset); e != nil || n != len(slot) {
		o.fault = errors.Join(ownedfile.ErrRetirement, e, io.ErrShortWrite)
		return o.fault
	}
	if e := o.file.Sync(); e != nil {
		o.fault = errors.Join(ownedfile.ErrRetirement, e)
		return o.fault
	}
	return nil
}
func (o *PortableReceivedPublication) closeLocked() error {
	if o == nil {
		return nil
	}
	o.closed = true
	if o.file != nil {
		if err := ownedfile.CloseFile(o.file); err != nil {
			o.fault = errors.Join(o.fault, ownedfile.CloseError(o.file, err))
			return o.fault
		}
		o.file = nil
	}
	if o.fault != nil {
		return errors.Join(ownedfile.ErrRetirement, o.fault)
	}
	if err := o.work.Release(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	o.work = resourcebudget.Permit{}
	if err := o.disk.Release(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	o.disk = resourcebudget.Permit{}
	return nil
}

type PortableReceivedFailure struct {
	Owner *PortableReceivedPublication
	Cause error
}

func (e *PortableReceivedFailure) Error() string { return e.Cause.Error() }
func (e *PortableReceivedFailure) Unwrap() error { return e.Cause }

func readPortableReceivedReceiptIdentity(path string) (raw []byte, id resourcebudget.PhysicalIdentity, resultErr error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, id, err
	}
	defer func() {
		if e := ownedfile.CloseFile(f); e != nil {
			resultErr = errors.Join(resultErr, ownedfile.CloseError(f, e))
		}
	}()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != PortableReceivedDiskBytes {
		return nil, id, errors.Join(ErrMemberAuthority, err)
	}
	id, err = captureReceivedBirth(f)
	if err != nil {
		return nil, id, err
	}
	raw = make([]byte, PortableReceivedDiskBytes)
	if _, err = io.ReadFull(f, raw); err != nil {
		return nil, id, err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() {
		return nil, id, errors.Join(ownedfile.ErrReplaced, err)
	}
	return raw, id, nil
}

var ErrNoReceivedPublication = errors.New("original received ordering publication is absent")

func (p *StatePublisher) ValidatePortableReceivedPublication(ctx context.Context, j *OrderingJournal, m *resourcebudget.Manager) error {
	if ctx == nil || p == nil || j == nil || m == nil {
		return ErrMemberAuthority
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	o := p.received
	if o == nil {
		return ErrNoReceivedPublication
	}
	if p.closed || p.authorityFault != nil || j.closed || j.closing || j.poisoned != nil || j.received != o || o.publisher != p || o.journal != j || o.manager != m || o.closed || o.fault != nil || o.file == nil || o.owner == nil || p.state.Role != RoleReplica || p.state.Term != p.state.PromisedTerm || p.state.DurableIndex != j.lastIndex || p.state.OrderingHeadMAC != j.head {
		return ErrMemberAuthority
	}
	if err := p.readAuthoritySource(p.state); err != nil {
		return err
	}
	id, err := captureReceivedBirth(j.file)
	if err != nil || id != o.header.Ordering {
		return errors.Join(ErrMemberAuthority, err)
	}
	return errors.Join(o.owner.VerifyFile(o.file), ctx.Err())
}
