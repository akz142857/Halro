package replication

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const PhysicalPublicationRAMBytes int64 = 4*MaxMemberStateJSON + 8192
const physicalPublicationReceiptBytes int64 = 8192
const physicalPublicationOpaqueSlots = 4
const PhysicalPublicationDiskBytes int64 = 2*MaxMemberStateJSON + physicalPublicationReceiptBytes + physicalPublicationOpaqueSlots*(MaxMemberStateJSON+physicalPublicationReceiptBytes+4096)

// PhysicalPublicationOptions never authorizes a source, role, or selected root.
// The caller retains its actual data-directory exclusion and separately proves
// its seven-root permission before admitting a Provider request.
type PhysicalPublicationOptions struct {
	Records      int
	OrderingDisk resourcebudget.Permit
	BankDisk     resourcebudget.Permit
	Restart      bool
	phase        func(string) error
}

// PhysicalPublication retains actual bank descriptors and their complete fees.
// Only the originating journal can issue/consume its finite write reservations.
// Uncertain mutation/Close leaves this owner reachable from both parents.
type PhysicalPublication struct {
	publisher         *StatePublisher
	journal           *OrderingJournal
	manager           *resourcebudget.Manager
	bank              *os.File
	bankOwner         *ownedfile.Owner
	current           os.FileInfo
	end               int64
	reserved          int
	bankFee           resourcebudget.Permit
	orderingRAM       resourcebudget.Permit
	bankDisk          resourcebudget.Permit
	orderingFee       resourcebudget.Permit
	faultMu           sync.Mutex
	fault             error
	banksClosed       bool
	persistent        bool
	withdrawn         atomic.Bool
	receipt           physicalPublicationReceipt
	receiptFile       *os.File
	receiptOwner      *ownedfile.Owner
	phase             func(string) error
	renewalGeneration uint64
	currentControl    physicalRenewalWindow
	renewalDebt       resourcebudget.Permit
}

type physicalPublicationReceipt struct {
	Version                        int
	ClusterID, Incarnation, NodeID string
	State                          [2]resourcebudget.PhysicalIdentity
	Ordering                       resourcebudget.PhysicalIdentity
	OrderingEnd                    int64
	Receipt                        resourcebudget.PhysicalIdentity `json:",omitzero"`
	MAC                            []byte
}

// OrderingPhysicalReservation is not constructible from a record, context or
// persisted candidate. It escrows one real preallocated record before the
// source write. It is single-use; uncertain durability keeps it reserved.
type OrderingPhysicalReservation struct {
	owner          *PhysicalPublication
	term           uint64
	used, released bool
}

func physicalReceiptMAC(r physicalPublicationReceipt, key []byte) ([]byte, error) {
	r.MAC = nil
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("halro:ha:physical-publication:v1\x00"))
	h.Write(raw)
	return h.Sum(nil), nil
}

// PreparePhysicalPublication authenticates actual Member and Ordering parents.
// KEEP_SIZE leaves journal EOF and its MAC chain unchanged. Member writes use
// existing names via captured atomic exchange, never a fresh temporary inode.
// Restart rechecks the independent Member, complete recovered Ordering owner,
// authenticated receipt and exact birth identities; the receipt grants no cut.
func (p *StatePublisher) PreparePhysicalPublication(ctx context.Context, j *OrderingJournal, manager *resourcebudget.Manager, options PhysicalPublicationOptions) (owner *PhysicalPublication, resultErr error) {
	return p.preparePhysicalPublication(ctx, j, manager, options, nil)
}

func (p *StatePublisher) preparePhysicalPublication(ctx context.Context, j *OrderingJournal, manager *resourcebudget.Manager, options PhysicalPublicationOptions, received *ReplicaReceivedOrderingAuthority) (owner *PhysicalPublication, resultErr error) {
	if p == nil || j == nil || manager == nil || options.Records < 1 || options.Records > maxUnconfirmedEntries {
		return nil, resourcebudget.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if p.closed || p.authorityFault != nil || p.physical != nil || j.closed || j.closing || j.poisoned != nil || j.freezeCancel != nil || j.physical != nil {
		return nil, ErrMemberAuthority
	}
	if p.state.ClusterID != j.header.ClusterID || p.state.Incarnation != j.header.Incarnation || p.state.Term < j.lastTerm || p.state.PromisedTerm != p.state.Term || ((p.state.DurableIndex != j.lastIndex || p.state.OrderingHeadMAC != j.head) && (received == nil || received.validateLocked(p, j, manager) != nil)) {
		return nil, ErrMemberAuthority
	}
	if j.maxDiskBytes <= 0 {
		return nil, ErrOrderingJournalDiskLimit
	}
	o := &PhysicalPublication{publisher: p, journal: j, manager: manager, phase: options.phase}
	// Retain the actual original journal ceiling; never charge a second ceiling.
	var err error
	o.orderingFee, err = options.OrderingDisk.RetainCovering(manager, resourcebudget.Cost{Disk: j.maxDiskBytes})
	if err != nil {
		return nil, err
	}
	o.bankFee, err = manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: PhysicalPublicationRAMBytes - 8192})
	if err != nil {
		return nil, errors.Join(err, o.orderingFee.Release())
	}
	o.orderingRAM, err = manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 8192})
	if err != nil {
		return nil, errors.Join(err, o.bankFee.Release(), o.orderingFee.Release())
	}
	const bankBytes = PhysicalPublicationDiskBytes
	if options.BankDisk != (resourcebudget.Permit{}) {
		o.bankDisk, err = options.BankDisk.RetainCovering(manager, resourcebudget.Cost{Disk: bankBytes})
	} else {
		o.bankDisk, err = manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{Disk: bankBytes})
	}
	if err != nil {
		return nil, errors.Join(err, o.orderingRAM.Release(), o.bankFee.Release(), o.orderingFee.Release())
	}
	// Install ownership before the first allocation/create. Failures cannot drop
	// a partially allocated inode, original fees, or unknown creator debt.
	p.physical, j.physical = o, o
	defer func() {
		if resultErr != nil {
			if o.bank == nil && o.receipt.Version == 0 && !o.persistent && !errors.Is(resultErr, ownedfile.ErrRetirement) {
				cleanup := errors.Join(o.orderingRAM.Release(), o.bankFee.Release(), o.bankDisk.Release(), o.orderingFee.Release())
				if cleanup == nil {
					p.physical, j.physical = nil, nil
					owner = nil
					return
				}
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, cleanup)
			}
			o.fail(resultErr)
			owner = o
		}
	}()
	if err := p.readAuthoritySource(p.state); err != nil {
		return o, err
	}
	current, err := os.OpenFile(p.path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return o, err
	}
	defer func() {
		if err := ownedfile.CloseFile(current); err != nil {
			resultErr = errors.Join(resultErr, ownedfile.CloseError(current, err))
		}
	}()
	o.current, err = current.Stat()
	if err != nil {
		return o, err
	}
	stateID, err := resourcebudget.CapturePhysicalIdentity(current)
	if err != nil {
		return o, err
	}
	journalID, err := resourcebudget.CapturePhysicalIdentity(j.file)
	if err != nil {
		return o, err
	}
	bankPath := p.path + ".physical-bank"
	receiptPath := p.path + ".physical.json"
	if options.Restart {
		raw, readErr := readPhysicalReceipt(receiptPath)
		var prior physicalPublicationReceipt
		decodeErr := decodePhysicalPublicationReceipt(raw, &prior)
		mac, macErr := physicalReceiptMAC(prior, p.key[:])
		if readErr != nil || decodeErr != nil || macErr != nil || !hmac.Equal(mac, prior.MAC) {
			if received != nil {
				return o, errors.Join(ErrMemberAuthority, readErr, decodeErr, macErr)
			}
			if errors.Is(readErr, ownedfile.ErrRetirement) {
				return o, readErr
			}
			// No authenticated ownership is inferred for the old objects. Only the
			// independently authenticated Member/Ordering and absence of unexplained
			// Ordering extents permit preserving them in a fixed opaque namespace.
			if err := resourcebudget.CheckPhysicalTail(j.file, j.nextOffset, j.nextOffset); err != nil {
				return o, err
			}
			o.persistent = true
			if err := preserveOpaquePublicationFiles(ctx, p.path); err != nil {
				return o, err
			}
			options.Restart = false
		}
	}
	if options.Restart {
		raw, err := readPhysicalReceipt(receiptPath)
		if err != nil {
			return o, err
		}
		if err := decodePhysicalPublicationReceipt(raw, &o.receipt); err != nil {
			return o, err
		}
		mac, err := physicalReceiptMAC(o.receipt, p.key[:])
		if err != nil || !hmac.Equal(mac, o.receipt.MAC) || (o.receipt.Version != 1 && o.receipt.Version != 2 && o.receipt.Version != 3) || o.receipt.ClusterID != p.state.ClusterID || o.receipt.Incarnation != p.state.Incarnation || o.receipt.NodeID != p.state.NodeID || o.receipt.Ordering != journalID || o.receipt.OrderingEnd > j.maxDiskBytes {
			return o, errors.Join(ErrMemberAuthority, err)
		}
		if received != nil && (o.receipt.Receipt != received.receipt.Receipt || !hmac.Equal(o.receipt.MAC, received.receipt.MAC)) {
			return o, ownedfile.ErrReplaced
		}
		o.bank, err = os.OpenFile(bankPath, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return o, err
		}
		bankInfo, err := o.bank.Stat()
		if err != nil {
			return o, err
		}
		bankID, err := resourcebudget.CapturePhysicalIdentity(o.bank)
		if err != nil {
			return o, err
		}
		pair := o.receipt.State
		if !((stateID == pair[0] && bankID == pair[1]) || (stateID == pair[1] && bankID == pair[0])) {
			return o, ownedfile.ErrReplaced
		}
		parent, err := os.Lstat(filepath.Dir(p.path))
		if err != nil {
			return o, err
		}
		o.bankOwner, err = ownedfile.RestoreAuthenticated(bankPath, o.bank, bankInfo, parent)
		if err != nil {
			return o, err
		}
		o.end = o.receipt.OrderingEnd
		if err := o.openReceiveReceipt(receiptPath, received != nil); err != nil {
			return o, err
		}
		if err := o.restoreRenewalWindowLocked(ctx); err != nil {
			return o, err
		}
		if o.end < j.nextOffset || o.end > j.maxDiskBytes {
			return o, ErrOrderingJournalDiskLimit
		}
	} else {
		if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
			return o, errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := resourcebudget.CheckPhysicalTail(j.file, j.nextOffset, j.nextOffset); err != nil {
			return o, err
		}
		o.end = j.nextOffset + int64(options.Records)*orderingRecordBytes
		if o.end > j.maxDiskBytes {
			return o, ErrOrderingJournalDiskLimit
		}
		o.bank, o.bankOwner, err = ownedfile.Create(bankPath, os.O_RDWR, 0600)
		if err != nil {
			return o, err
		}
		o.persistent = true
		bankID, err := resourcebudget.CapturePhysicalIdentity(o.bank)
		if err != nil {
			return o, err
		}
		if o.phase != nil {
			if err := o.phase("bank-created"); err != nil {
				return o, err
			}
		}
		o.receipt = physicalPublicationReceipt{Version: 3, ClusterID: p.state.ClusterID, Incarnation: p.state.Incarnation, NodeID: p.state.NodeID, State: [2]resourcebudget.PhysicalIdentity{stateID, bankID}, Ordering: journalID, OrderingEnd: o.end}
		o.receiptFile, o.receiptOwner, err = ownedfile.Create(receiptPath, os.O_RDWR, 0600)
		if err != nil {
			return o, err
		}
		o.receipt.Receipt, err = resourcebudget.CapturePhysicalIdentity(o.receiptFile)
		if err != nil {
			return o, err
		}
		o.receipt.MAC, err = physicalReceiptMAC(o.receipt, p.key[:])
		if err != nil {
			return o, err
		}
		raw, err := encodePhysicalPublicationReceipt(o.receipt)
		if err != nil {
			return o, errors.Join(resourcebudget.ErrInvalid, err)
		}
		writeErr := writeAll(o.receiptFile, raw)
		syncErr := o.receiptFile.Sync()
		if err := errors.Join(writeErr, syncErr, durable.SyncDirectory(filepath.Dir(p.path))); err != nil {
			return o, err
		}
	}
	if o.phase != nil {
		if err := o.phase("receipt-synced"); err != nil {
			return o, err
		}
	}
	// A valid preparation receipt precedes allocation. Interrupted preparations
	// remain opaque until explicit restart reauthenticates these actual parents.
	if err := resourcebudget.AllocatePhysicalRange(current, 0, MaxMemberStateJSON); err != nil {
		return o, err
	}
	if err := resourcebudget.AllocatePhysicalRange(o.bank, 0, MaxMemberStateJSON); err != nil {
		return o, err
	}
	if o.end > j.nextOffset {
		if err := resourcebudget.AllocatePhysicalRange(j.file, j.nextOffset, o.end-j.nextOffset); err != nil {
			return o, err
		}
	}
	if o.phase != nil {
		if err := o.phase("ordering-allocated"); err != nil {
			return o, err
		}
	}
	if err := resourcebudget.CheckPhysicalRange(current, 0, MaxMemberStateJSON); err != nil {
		return o, err
	}
	if err := resourcebudget.CheckPhysicalRange(o.bank, 0, MaxMemberStateJSON); err != nil {
		return o, err
	}
	if err := resourcebudget.CheckPhysicalTail(j.file, j.nextOffset, o.end); err != nil {
		return o, err
	}
	if o.end > j.nextOffset {
		if err := resourcebudget.CheckPhysicalRange(j.file, j.nextOffset, o.end); err != nil {
			return o, err
		}
	}
	if err := ctx.Err(); err != nil {
		return o, err
	}
	return o, nil
}

func readPhysicalReceipt(path string) ([]byte, error) {
	raw, _, err := readPhysicalReceiptIdentity(path)
	return raw, err
}
func readPhysicalReceiptIdentity(path string) (raw []byte, identity resourcebudget.PhysicalIdentity, resultErr error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, identity, err
	}
	defer func() {
		if err := ownedfile.CloseFile(f); err != nil {
			resultErr = errors.Join(resultErr, ownedfile.CloseError(f, err))
		}
	}()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > physicalPublicationReceiptBytes {
		return nil, identity, errors.Join(ErrMemberAuthority, err)
	}
	identity, err = resourcebudget.CapturePhysicalIdentity(f)
	if err != nil {
		return nil, identity, err
	}
	raw, err = io.ReadAll(io.LimitReader(f, physicalPublicationReceiptBytes+1))
	if err != nil {
		return nil, identity, err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() {
		return nil, identity, errors.Join(ownedfile.ErrReplaced, err)
	}
	return raw, identity, nil
}

func (j *OrderingJournal) ReservePhysicalAppend(term uint64) (*OrderingPhysicalReservation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.physical == nil {
		return nil, nil
	}
	o := j.physical
	if j.closed || j.closing || j.poisoned != nil || o.failure() != nil || o.withdrawn.Load() || term < j.lastTerm || int64(o.reserved+1)*orderingRecordBytes > o.end-j.nextOffset {
		return nil, errors.Join(resourcebudget.ErrPhysicalAllocation, o.failure())
	}
	o.reserved++
	return &OrderingPhysicalReservation{owner: o, term: term}, nil
}
func (r *OrderingPhysicalReservation) Release() error {
	if r == nil {
		return nil
	}
	j := r.owner.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	if r.used || r.released {
		return nil
	}
	if j.poisoned != nil || r.owner.failure() != nil {
		return errors.Join(ownedfile.ErrRetirement, j.poisoned, r.owner.failure())
	}
	r.released = true
	r.owner.reserved--
	return nil
}
func (j *OrderingJournal) checkPhysicalAppendLocked(record OrderingRecord, bytes int64, r *OrderingPhysicalReservation) error {
	o := j.physical
	if o == nil {
		if r != nil {
			return resourcebudget.ErrInvalid
		}
		return nil
	}
	if o.failure() != nil || o.withdrawn.Load() {
		return errors.Join(ErrMemberAuthority, o.failure())
	}
	if bytes != orderingRecordBytes || j.nextOffset > o.end || bytes > o.end-j.nextOffset {
		return resourcebudget.ErrPhysicalAllocation
	}
	if r != nil {
		if r.owner != o || r.used || r.released || r.term != record.Term {
			return resourcebudget.ErrInvalid
		}
	} else if int64(o.reserved+1)*orderingRecordBytes > o.end-j.nextOffset {
		return resourcebudget.ErrExhausted
	}
	actual, err := resourcebudget.CapturePhysicalIdentity(j.file)
	if err != nil || actual != o.receipt.Ordering {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return resourcebudget.CheckPhysicalRange(j.file, j.nextOffset, j.nextOffset+bytes)
}

func (p *StatePublisher) writeStateLocked(next MemberState) error {
	if p.physical == nil {
		return WriteState(p.path, next, p.key[:])
	}
	o := p.physical
	if o.failure() != nil || o.banksClosed || o.bank == nil {
		return errors.Join(ErrMemberAuthority, o.failure())
	}
	if err := p.readAuthoritySource(p.state); err != nil {
		return o.fail(errors.Join(ErrMemberAuthority, err))
	}
	raw, err := MarshalState(next, p.key[:])
	if err != nil {
		return err
	}
	if err := resourcebudget.CheckPhysicalRange(o.bank, 0, MaxMemberStateJSON); err != nil {
		return err
	}
	// Fixed EOF padded JSON does not free the preallocated final blocks. The
	// exclusive publisher scratch charge covers encoding plus the bounded pad.
	padded := bytes.Repeat([]byte{' '}, MaxMemberStateJSON)
	copy(padded, raw)
	if n, err := o.bank.WriteAt(padded, 0); err != nil || n != len(padded) {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err, io.ErrShortWrite))
	}
	if err := o.bank.Sync(); err != nil {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err))
	}
	currentName, err := os.Lstat(p.path)
	if err != nil || !os.SameFile(currentName, o.current) {
		return o.fail(errors.Join(ownedfile.ErrRetirement, ownedfile.ErrReplaced, err))
	}
	oldOwner, err := o.bankOwner.ExchangeCaptured(p.path, o.current)
	if err != nil {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err))
	}
	if o.phase != nil {
		if err := o.phase("member-exchanged"); err != nil {
			return o.fail(errors.Join(ownedfile.ErrRetirement, err))
		}
	}
	// Close the descriptor only after the actual exchange; old current is now
	// the spare name. Unknown close forbids bank reuse and retains all fees.
	if err := ownedfile.CloseFile(o.bank); err != nil {
		return o.fail(ownedfile.CloseError(o.bank, err))
	}
	o.bank = nil
	o.bankOwner = oldOwner
	o.current, err = os.Lstat(p.path)
	if err != nil {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err))
	}
	o.bank, err = os.OpenFile(p.path+".physical-bank", os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err))
	}
	info, err := o.bank.Stat()
	named, nameErr := os.Lstat(p.path + ".physical-bank")
	if err != nil || nameErr != nil || !os.SameFile(info, named) {
		return o.fail(errors.Join(ownedfile.ErrRetirement, ownedfile.ErrReplaced, err, nameErr))
	}
	if err := durable.SyncDirectory(filepath.Dir(p.path)); err != nil {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err))
	}
	return nil
}
func (o *PhysicalPublication) closeBanksLocked() error {
	o.withdrawn.Store(true)
	if o.banksClosed {
		return o.failure()
	}
	if o.bank != nil {
		if err := ownedfile.CloseFile(o.bank); err != nil {
			return o.fail(errors.Join(o.failure(), ownedfile.CloseError(o.bank, err)))
		}
		o.bank = nil
	}
	if o.receiptFile != nil {
		if err := ownedfile.CloseFile(o.receiptFile); err != nil {
			return o.fail(ownedfile.CloseError(o.receiptFile, err))
		}
		o.receiptFile = nil
	}
	if o.failure() != nil {
		return errors.Join(ownedfile.ErrRetirement, o.failure())
	}
	if err := o.bankFee.Release(); err != nil {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err))
	}
	o.banksClosed = true
	return nil
}

func (o *PhysicalPublication) RemainingRecords() (int, error) {
	if o == nil {
		return 0, resourcebudget.ErrInvalid
	}
	j := o.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.closing || j.poisoned != nil || o.failure() != nil || o.withdrawn.Load() {
		return 0, errors.Join(resourcebudget.ErrPhysicalAllocation, o.failure(), j.poisoned)
	}
	return int((o.end-j.nextOffset)/orderingRecordBytes) - o.reserved, nil
}

// PhysicalCloseError preserves the historical void Close API while letting
// the Runtime report an actual physical-owner retirement debt.
func (p *StatePublisher) PhysicalCloseError() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.received != nil && p.received.fault != nil {
		return errors.Join(ownedfile.ErrRetirement, p.received.fault)
	}
	if p.physical == nil {
		return nil
	}
	if err := p.physical.failure(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	return nil
}

func (o *PhysicalPublication) String() string {
	return fmt.Sprintf("physical Member/Ordering owner %s", o.publisher.path)
}

func (o *PhysicalPublication) failure() error {
	o.faultMu.Lock()
	defer o.faultMu.Unlock()
	return o.fault
}
func (o *PhysicalPublication) fail(err error) error {
	o.faultMu.Lock()
	defer o.faultMu.Unlock()
	o.fault = errors.Join(o.fault, err)
	return o.fault
}

// Retain withdraws reuse after an uncertain native write. Close/cancellation
// does not authorize releasing its reserved range or any persistent fee.
func (r *OrderingPhysicalReservation) Retain(cause error) error {
	if r == nil {
		return cause
	}
	j := r.owner.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	if r.used || r.released {
		return resourcebudget.ErrInvalid
	}
	return r.owner.fail(errors.Join(ownedfile.ErrRetirement, cause))
}

func (j *OrderingJournal) requirePhysicalAdmissionBudget(configured bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !configured && j.physical != nil {
		return resourcebudget.ErrInvalid
	}
	return nil
}

// DiskReservations lends only the actual persistent pool owners for a same-
// Manager restart. Preparation reauthenticates parents/receipt before use.
func (o *PhysicalPublication) DiskReservations() (resourcebudget.Permit, resourcebudget.Permit) {
	if o == nil {
		return resourcebudget.Permit{}, resourcebudget.Permit{}
	}
	return o.bankDisk, o.orderingFee
}

// Called only after journal.Close has joined every reader/freeze and closed
// the original mutable descriptor. An unknown owner retains its original fee.
func (o *PhysicalPublication) closeOrderingLocked() error {
	if err := o.failure(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	if o.orderingRAM == (resourcebudget.Permit{}) {
		return nil
	}
	if err := o.orderingRAM.Release(); err != nil {
		return o.fail(errors.Join(ownedfile.ErrRetirement, err))
	}
	o.orderingRAM = resourcebudget.Permit{}
	return nil
}
