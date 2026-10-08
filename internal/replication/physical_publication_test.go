package replication

import (
	"bytes"
	"context"

	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func physicalPublicationBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 32 << 20, Disk: 32 << 20, Unconfirmed: 32 << 20}, MaxClaims: 128, MaxOwners: 16, MaxPressureAge: time.Hour}
	for _, class := range []resourcebudget.Class{resourcebudget.IO, resourcebudget.Background} {
		limits.Classes[class] = resourcebudget.Cost{RAM: 16 << 20, Disk: 16 << 20, Unconfirmed: 16 << 20}
	}
	limits.RecoveryReserve = resourcebudget.Cost{RAM: 8 << 20, Disk: 8 << 20, Unconfirmed: 8 << 20}
	limits.RecoveryClaims = 16
	limits.Classes[resourcebudget.Recovery] = limits.RecoveryReserve
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 128 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

func physicalPublicationFixture(t *testing.T) (*StatePublisher, *OrderingJournal, *resourcebudget.Manager, resourcebudget.Permit) {
	t.Helper()
	root := t.TempDir()
	if path := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT"); path != "" {
		var err error
		root, err = os.MkdirTemp(path, "ha-publication-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	key := bytes.Repeat([]byte{32}, 32)
	state := publisherTestState(RolePrimary)
	path := filepath.Join(root, "state.json")
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	p, err := NewStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	j, err := OpenOrderingJournalWithOptions(filepath.Join(root, "ordering.journal"), key, OrderingHeader{ClusterID: state.ClusterID, Incarnation: state.Incarnation}, 0, [32]byte{}, OrderingJournalOptions{MaxDiskBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ownedfile.ErrRetirement) {
			t.Error(err)
		}
	})
	m := physicalPublicationBudget(t)
	fee, err := m.TryAcquire(resourcebudget.IO, resourcebudget.Cost{Disk: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return p, j, m, fee
}
func requireActualPhysicalPublication(t *testing.T, p *StatePublisher, j *OrderingJournal, m *resourcebudget.Manager, fee resourcebudget.Permit, records int) *PhysicalPublication {
	t.Helper()
	o, err := p.PreparePhysicalPublication(context.Background(), j, m, PhysicalPublicationOptions{Records: records, OrderingDisk: fee})
	if err != nil {
		if errors.Is(err, resourcebudget.ErrPhysicalAllocation) && os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" {
			t.Skip("requires actual ext4 physical allocation", err)
		}
		t.Fatal(err)
	}
	return o
}

func TestPhysicalPublicationIndependentMemberRefusesBeforeMutation(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	before := m.Snapshot().Used
	p.state.DurableIndex = 1
	p.state.OrderingHeadMAC = [32]byte{1}
	if _, err := p.PreparePhysicalPublication(context.Background(), j, m, PhysicalPublicationOptions{Records: 2, OrderingDisk: fee}); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("changed actual authority accepted", err)
	}
	if m.Snapshot().Used != before {
		t.Fatal("failed source qualification changed fees", m.Snapshot())
	}
	if _, err := os.Lstat(p.path + ".physical-bank"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unqualified bank created", err)
	}
}
func TestPhysicalPublicationActualReservedRangeCannotBeStolen(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	o := requireActualPhysicalPublication(t, p, j, m, fee, 2)
	r, err := j.ReservePhysicalAppend(7)
	if err != nil {
		t.Fatal(err)
	}
	first, err := j.Append(testLeadershipRecord(1, 7, "anchor"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(testOrderingRecord(2, 7, "stolen")); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("ordinary append stole reserved bytes", err)
	}
	if err := m.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	second, err := j.AppendWithPhysicalReservation(testOrderingRecord(2, 7, "reserved"), r)
	if err != nil {
		t.Fatal(err)
	}
	if second.PreviousMAC != first.MAC {
		t.Fatal("physical escrow changed chain")
	}
	if _, err := j.AppendWithPhysicalReservation(testOrderingRecord(3, 7, "repeat"), r); err == nil {
		t.Fatal("repeated reservation accepted")
	}
	if remaining, err := o.RemainingRecords(); err != nil || remaining != 0 {
		t.Fatal(remaining, err)
	}
	if err := p.PublishPrimary(PrimaryProgress{DurableIndex: 2, ConfirmedIndex: 2, OrderingHeadMAC: second.MAC, Projection: ProjectionState{Index: 2}}); err != nil {
		t.Fatal(err)
	}
	s, err := ReadState(p.path, p.key[:])
	if err != nil || s.AppliedIndex != 2 {
		t.Fatal(s, err)
	}
}
func TestPhysicalPublicationActualMemberExchangeAndAuthenticatedRestart(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	original := requireActualPhysicalPublication(t, p, j, m, fee, 16)
	beforeQualification := m.Snapshot().Used
	if err := original.ValidateParents(context.Background(), p, j, physicalPublicationBudget(t)); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("foreign Manager borrowed original physical bank", err)
	}
	if err := original.ValidateParents(context.Background(), p, j, m); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Used != beforeQualification {
		t.Fatal("parent requalification allocated additional bank fees")
	}

	names := make(map[uint64]bool)
	for index := uint64(1); index <= 8; index++ {
		record := testOrderingRecord(index, 7, "fact")
		record.StoreSequenceFirst = index - 1
		record.StoreSequenceLast = index - 1
		if index == 1 {
			record = testLeadershipRecord(index, 7, "anchor")
		}
		persisted, err := j.Append(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := original.ValidateParents(context.Background(), p, j, m); err != nil {
			t.Fatal("actual installed bank/journal parent qualification", err)
		}
		for _, confirmed := range []uint64{index - 1, index} {
			if err := p.PublishPrimary(PrimaryProgress{DurableIndex: index, ConfirmedIndex: confirmed, OrderingHeadMAC: persisted.MAC, Projection: ProjectionState{Index: confirmed}}); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(p.path)
			if err != nil {
				t.Fatal(err)
			}
			id, err := resourcebudget.CapturePhysicalIdentity(f)
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
			names[id.Inode] = true
			if err := p.WithReadAuthority(context.Background(), m, func(_ context.Context, s MemberState, authorize func(func() error) error) error {
				return authorize(func() error {
					if s.ConfirmedIndex != confirmed {
						return errors.New("incorrect Member snapshot")
					}
					return nil
				})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(names) != 2 {
		t.Fatal("publisher allocated fresh inodes", names)
	}
	state := p.Snapshot()
	key := append([]byte(nil), p.key[:]...)
	p.Close()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewStatePublisher(p.path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	next, err := OpenExistingOrderingJournalWithOptions(j.filePathForPhysicalTest(), key, state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC, OrderingJournalOptions{MaxDiskBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	bankDisk, _ := original.DiskReservations()
	beforeDisk := m.Snapshot().Used.Disk
	if _, err := fresh.PreparePhysicalPublication(context.Background(), next, m, PhysicalPublicationOptions{Records: 8, OrderingDisk: fee, BankDisk: bankDisk, Restart: true}); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Used.Disk != beforeDisk {
		t.Fatal("same Manager restart duplicated physical Disk", beforeDisk, m.Snapshot())
	}
	if err := fresh.physical.ValidateParents(context.Background(), fresh, next, m); err != nil {
		t.Fatal("fresh independently restored actual bank parents", err)
	}
	if _, err := fresh.Promise(8); err != nil {
		t.Fatal(err)
	}
}

func (j *OrderingJournal) filePathForPhysicalTest() string {
	return filepath.Join(filepath.Dir(j.physical.publisher.path), "ordering.journal")
}

func TestPhysicalPublicationUncertainSourceRetainsReservationAndFees(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	o := requireActualPhysicalPublication(t, p, j, m, fee, 2)
	r, err := j.ReservePhysicalAppend(7)
	if err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot().Used
	cause := errors.New("native fsync outcome unknown")
	if err := r.Retain(cause); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if _, err := j.AppendWithPhysicalReservation(testLeadershipRecord(1, 7, "refused"), r); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	p.Close()
	if m.Snapshot().Used != before || o.reserved != 1 {
		t.Fatal("unknown source dropped original physical debt", m.Snapshot(), o.reserved)
	}
}

func TestPhysicalPublicationActualPrimaryAdmissionAndACK(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	o := requireActualPhysicalPublication(t, p, j, m, fee, 8)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = m
	limits.MaxUnconfirmedEntries = 32
	limits.UnconfirmedMetadataRAMBytes = 4 << 20
	limits.RecoveryReserveUnconfirmedBytes = 1 << 20
	c, err := newPrimaryCoordinator(p.state.ClusterID, p.state.Incarnation, p.state.NodeID, 7, 0, []string{"halro-1"}, j, &recordingOutbound{}, p.PublishPrimary, func(func(LocalCommit) error) error { return nil }, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	anchor, err := c.RecordDurable(Frame{Kind: KindLeadershipEstablished})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", anchor.Index)); err != nil {
		t.Fatal(err)
	}
	a, err := c.ReserveNativeSourceUnconfirmed(32)
	if err != nil {
		t.Fatal(err)
	}
	if a.(*primaryUnconfirmedAdmission).physical == nil {
		t.Fatal("source admission lacks original physical escrow")
	}
	if err := m.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	commit, err := c.RecordDurableWithAdmission(Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: bytes.Repeat([]byte{1}, 32)}, a)
	if err != nil {
		t.Fatal(err)
	}
	if !a.(*primaryUnconfirmedAdmission).physical.used {
		t.Fatal("AfterDurable failed to consume original token")
	}
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", commit.Index)); err != nil {
		t.Fatal(err)
	}
	s := p.Snapshot()
	if s.ConfirmedIndex != commit.Index || s.AppliedIndex != commit.Index || c.UnconfirmedAdmissionStatus().Entries != 0 {
		t.Fatal(s, c.UnconfirmedAdmissionStatus())
	}
	if remaining, err := o.RemainingRecords(); err != nil || remaining != 6 {
		t.Fatal(remaining, err)
	}
}

func TestPhysicalPublicationActualReplicaBeforeSinkAndDuplicates(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	p.state.Role = RoleReplica
	if err := WriteState(p.path, p.state, p.key[:]); err != nil {
		t.Fatal(err)
	}
	requireActualPhysicalPublication(t, p, j, m, fee, 1)
	sink := &recordingFrameSink{}
	r, err := NewReplicaReceiver(p.state.ClusterID, p.state.Incarnation, p.state.NodeID, "halro-1", 7, 7, 0, 0, j, sink, ReplicaReceiverOptions{PersistState: p.PublishReplica})
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{Kind: KindLeadershipEstablished, ClusterID: p.state.ClusterID, Incarnation: p.state.Incarnation, Index: 1, Term: 7}
	raw, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Receive(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Receive(raw); err != nil || len(sink.frames) != 1 {
		t.Fatal("duplicate consumed native/Ordering escrow", len(sink.frames), err)
	}
	frame.Kind = KindData
	frame.Store = StoreLedger
	frame.Index = 2
	frame.StoreGeneration = 1
	frame.StoreSequenceFirst = 1
	frame.StoreSequenceLast = 1
	frame.Payload = []byte{1}
	raw, err = frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Receive(raw); !errors.Is(err, resourcebudget.ErrPhysicalAllocation) || len(sink.frames) != 1 {
		t.Fatal("Ordering exhaustion was discovered after native sink write", len(sink.frames), err)
	}
}

func TestPhysicalPublicationActualFullFilesystemKeepsAcceptedOrderingAndMember(t *testing.T) {
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" {
		t.Skip("requires isolated <=512MiB ext4 full filesystem")
	}
	p, j, m, fee := physicalPublicationFixture(t)
	requireActualPhysicalPublication(t, p, j, m, fee, 4)
	r, err := j.ReservePhysicalAppend(7)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := os.OpenFile(filepath.Join(filepath.Dir(p.path), "foreign-fill"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	block := bytes.Repeat([]byte{43}, 64<<10)
	filled := int64(0)
	for {
		n, err := foreign.Write(block)
		filled += int64(n)
		if err != nil {
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatal(err)
			}
			break
		}
		if filled > 512<<20 {
			t.Fatal("fixture is not bounded full filesystem")
		}
	}
	if err := foreign.Sync(); err != nil && !errors.Is(err, syscall.ENOSPC) {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	record, err := j.AppendWithPhysicalReservation(testLeadershipRecord(1, 7, "accepted"), r)
	if err != nil {
		t.Fatal("original allocated Ordering range failed at ENOSPC", err)
	}
	if err := p.PublishPrimary(PrimaryProgress{DurableIndex: 1, ConfirmedIndex: 1, OrderingHeadMAC: record.MAC, Projection: ProjectionState{Index: 1}}); err != nil {
		t.Fatal("original allocated Member bank failed at ENOSPC", err)
	}
	actual, err := ReadState(p.path, p.key[:])
	if err != nil || actual.ConfirmedIndex != 1 {
		t.Fatal(actual, err)
	}
	t.Logf("actual full ext4 foreign_bytes=%d Ordering=%d Member=%d/%d/%d", filled, record.Index, actual.DurableIndex, actual.ConfirmedIndex, actual.AppliedIndex)
}

func TestPhysicalPublicationForeignOriginalFeeCannotFundPool(t *testing.T) {
	p, j, _, fee := physicalPublicationFixture(t)
	foreign := physicalPublicationBudget(t)
	before := foreign.Snapshot().Used
	if _, err := p.PreparePhysicalPublication(context.Background(), j, foreign, PhysicalPublicationOptions{Records: 1, OrderingDisk: fee}); !errors.Is(err, resourcebudget.ErrInvalid) {
		t.Fatal("foreign journal fee admitted", err)
	}
	if foreign.Snapshot().Used != before {
		t.Fatal("foreign permit changed fees", foreign.Snapshot())
	}
	if _, err := os.Lstat(p.path + ".physical-bank"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("foreign permit created bank", err)
	}
}

func TestPhysicalPublicationOpaqueSlotsBoundExplicitRestart(t *testing.T) {
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" {
		t.Skip("requires isolated ext4")
	}
	p, j, m, fee := physicalPublicationFixture(t)
	for i := 0; i < physicalPublicationOpaqueSlots; i++ {
		if err := os.Mkdir(fmt.Sprintf("%s.physical.unqualified-%d", p.path, i), 0700); err != nil {
			t.Fatal(err)
		}
	}
	foreign := []byte("opaque unqualified creator bytes")
	if err := os.WriteFile(p.path+".physical-bank", foreign, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PreparePhysicalPublication(context.Background(), j, m, PhysicalPublicationOptions{Records: 1, OrderingDisk: fee, Restart: true}); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("fifth opaque namespace accepted", err)
	}
	raw, err := os.ReadFile(p.path + ".physical-bank")
	if err != nil || !bytes.Equal(raw, foreign) {
		t.Fatal("unqualified original contents changed", err)
	}
	if _, err := os.Lstat(p.path + ".physical.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unqualified contents got receipt", err)
	}
}

func TestPhysicalPublicationUnknownBankCloseRetainsOriginalOwnerFee(t *testing.T) {
	p, j, m, fee := physicalPublicationFixture(t)
	o := requireActualPhysicalPublication(t, p, j, m, fee, 2)
	original := o.bank
	before := m.Snapshot().Used
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if !errors.Is(p.PhysicalCloseError(), ownedfile.ErrRetirement) || o.bank != original || m.Snapshot().Used != before {
		t.Fatal("unknown Close dropped bank/fee", p.PhysicalCloseError(), m.Snapshot())
	}
	p.Close()
	if m.Snapshot().Used != before {
		t.Fatal("second Close dropped debt", m.Snapshot())
	}
}
