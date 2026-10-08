package replication

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type candidateCountSource struct {
	DurableSource
	reads      map[uint64]int
	beforeRead func()
}

type orderingAdmissionFailure struct{ file *os.File }

func (d *orderingAdmissionFailure) Write(value []byte) (int, error) { return d.file.Write(value) }
func (d *orderingAdmissionFailure) Sync() error                     { return errors.New("injected ordering sync failure") }

func (s *candidateCountSource) Read(store Store, generation, first, last uint64) ([]byte, error) {
	if s.beforeRead != nil {
		s.beforeRead()
	}
	s.reads[first]++
	return s.DurableSource.Read(store, generation, first, last)
}
func candidateManager(t *testing.T, slots int) *resourcebudget.Manager {
	t.Helper()
	reserve := int64(slots) * (MinPrimaryCandidateRAMBytes + resourcebudget.ClaimRAMBytes)
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: reserve + 64<<20}, RecoveryReserve: resourcebudget.Cost{RAM: reserve}, MaxClaims: slots + 4, RecoveryClaims: slots, MaxOwners: 4, MaxPressureAge: time.Minute}
	limits.Classes[resourcebudget.Recovery].RAM = int64(slots) * MinPrimaryCandidateRAMBytes
	limits.Classes[resourcebudget.Control].RAM = int64(limits.MaxClaims) * resourcebudget.ClaimRAMBytes
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	return m // Unknown deliberately preserves recovery admission.
}
func candidateLimits(m *resourcebudget.Manager) PrimaryWindowLimits {
	return PrimaryWindowLimits{MaxFrames: 16, MaxBytes: maxEncodedPrimaryFrame, CandidateBudget: m, CandidateRAMBytes: MinPrimaryCandidateRAMBytes}
}

func unconfirmedManager(t *testing.T, ioBytes, recoveryBytes int64, recoveryClaims int) *resourcebudget.Manager {
	t.Helper()
	claims := recoveryClaims + 8
	limits := resourcebudget.Limits{
		Total: resourcebudget.Cost{RAM: 32 << 20, Unconfirmed: ioBytes + recoveryBytes},
		// The fixed release table overlaps the temporary suffix claim before
		// Merge. Recovery's reserve includes each claim's control RAM as well.
		RecoveryReserve: resourcebudget.Cost{RAM: replicationlimits.PrimaryUnconfirmedTableRAMBytes + int64(recoveryClaims)*resourcebudget.ClaimRAMBytes, Unconfirmed: recoveryBytes},
		MaxClaims:       claims, RecoveryClaims: recoveryClaims, MaxOwners: 2, MaxPressureAge: time.Minute,
	}
	limits.Classes[resourcebudget.IO].Unconfirmed = ioBytes
	limits.Classes[resourcebudget.Recovery].RAM = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.Classes[resourcebudget.Recovery].Unconfirmed = recoveryBytes
	limits.Classes[resourcebudget.Control].RAM = int64(claims) * resourcebudget.ClaimRAMBytes
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPrimaryRecoveredUnconfirmedSuffixIsChargedAndACKReleasesOnlyConfirmedPrefix(t *testing.T) {
	journal, source := windowHistory(t, 4, []byte("source"))
	manager := unconfirmedManager(t, 2<<20, 2<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = manager
	limits.MaxUnconfirmedEntries = 32
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 2 << 20
	c, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1", "halro-2"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { return nil }, source, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Stop(); _ = c.ReleaseUnconfirmedForShutdown() }()
	before := manager.Snapshot()
	if before.RecoveryClaims != 1 || before.Used.Unconfirmed == 0 || len(c.unconfirmedEntries) != 3 || !c.hasRecoveryUnconfirmedPermit {
		t.Fatalf("recovered suffix was not charged before construction completed: %+v", before)
	}
	if err := c.PreparePeer("halro-1", 1, 0, (&windowSender{}).send); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := c.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil || confirmed != 2 {
		t.Fatal(confirmed, err)
	}
	after := manager.Snapshot()
	if after.RecoveryClaims != 1 || after.Used.Unconfirmed >= before.Used.Unconfirmed || len(c.unconfirmedEntries) != 2 {
		t.Fatalf("ACK did not release exactly the newly confirmed admission: before=%+v after=%+v", before, after)
	}
}

func TestPrimaryOverLimitRecoveredSuffixRetransmitsWithoutOpeningAppendAdmission(t *testing.T) {
	journal, source := windowHistory(t, 4, []byte("source"))
	lastFrame := testConfirmationFrame(4, KindData)
	lastFrame.StoreSequenceFirst, lastFrame.StoreSequenceLast = 3, 3
	lastFrame.Payload = []byte("source")
	lastEncoded, err := lastFrame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	recoveryCap := int64(len(lastEncoded))
	manager := unconfirmedManager(t, 1<<20, recoveryCap, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = manager
	limits.MaxUnconfirmedEntries = 1
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = recoveryCap
	limits.MaxFrames = 1
	limits.MaxBytes = maxEncodedPrimaryFrame
	c, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { return nil }, source, limits)
	if err != nil {
		t.Fatal("over-limit existing suffix must remain recoverable", err)
	}
	defer func() { c.Stop(); _ = c.ReleaseUnconfirmedForShutdown() }()
	if !c.recoveryOverLimit || len(c.unconfirmedEntries) != 3 || manager.Snapshot().Used.Unconfirmed != recoveryCap {
		t.Fatalf("over-limit suffix was not represented by bounded recovery state: entries=%d coordinator=%+v budget=%+v", len(c.unconfirmedEntries), c, manager.Snapshot())
	}
	if _, err := c.ReserveUnconfirmed(1); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("new append admitted while a recovered suffix exceeds its configured limit", err)
	}
	if got := c.UnconfirmedAdmissionStatus().AdmissionRefusals; got != 1 {
		t.Fatal("over-limit append refusal was not observable", got)
	}
	sender := &windowSender{}
	if err := c.PreparePeer("halro-1", 1, 0, sender.send); err != nil {
		t.Fatal("bounded retransmission could not start for the preserved suffix", err)
	}
	if len(sender.frames) != 1 || c.windowFor("halro-1").queuedThrough != 2 || c.pending != nil {
		t.Fatalf("recovery did not stay within its one-frame peer window: frames=%v peer=%+v pending=%v", sender.frames, c.windowFor("halro-1"), c.pending)
	}
	if _, available, cause := c.Status(); available || !errors.Is(cause, ErrUnconfirmedRecoveryOverLimit) {
		t.Fatalf("over-limit recovery reported ready after transport attached: available=%t cause=%v", available, cause)
	}
	if confirmed, err := c.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil || confirmed != 2 {
		t.Fatal("first recovered prefix did not advance", confirmed, err)
	}
	if len(sender.frames) != 2 || c.windowFor("halro-1").queuedThrough != 3 {
		t.Fatalf("ACK did not advance the bounded recovery window by one frame: frames=%v peer=%+v", sender.frames, c.windowFor("halro-1"))
	}
	if confirmed, err := c.Acknowledge(testAcknowledgement("halro-1", 3)); err != nil || confirmed != 3 {
		t.Fatal("recovered suffix did not advance into configured bounds", confirmed, err)
	}
	if len(sender.frames) != 3 || c.windowFor("halro-1").queuedThrough != 4 {
		t.Fatalf("second ACK did not advance the bounded recovery window by one frame: frames=%v peer=%+v", sender.frames, c.windowFor("halro-1"))
	}
	if c.recoveryOverLimit || len(c.unconfirmedEntries) != 1 || manager.Snapshot().Used.Unconfirmed != recoveryCap {
		t.Fatalf("recovery did not recharge the remaining suffix before reopening: coordinator=%+v budget=%+v", c, manager.Snapshot())
	}
	if _, err := c.ReserveUnconfirmed(1); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("append count limit did not remain active after over-limit debt cleared", err)
	}
	if confirmed, err := c.Acknowledge(testAcknowledgement("halro-1", 4)); err != nil || confirmed != 4 {
		t.Fatal("final recovered frame did not confirm", confirmed, err)
	}
	if c.recoveryOverLimit || len(c.unconfirmedEntries) != 0 || manager.Snapshot().Used.Unconfirmed != 0 {
		t.Fatalf("recovery did not reopen admission after clearing the over-limit suffix: coordinator=%+v budget=%+v", c, manager.Snapshot())
	}
	if _, available, cause := c.Status(); !available || cause != nil {
		t.Fatalf("coordinator did not regain availability after recovery: available=%t cause=%v", available, cause)
	}
	admission, err := c.ReserveUnconfirmed(128)
	if err != nil || admission == nil {
		t.Fatal("normal append admission did not resume after recovery", err)
	}
	if err := admission.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestPrimaryAppendAdmissionIsHeldUntilConfirmedStatePublication(t *testing.T) {
	journal, _ := windowHistory(t, 1, nil)
	manager := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = manager
	limits.MaxUnconfirmedEntries = 2
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 1 << 20
	stream := func(func(LocalCommit) error) error { return nil }
	c, err := newPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { return nil }, stream, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Stop(); _ = c.ReleaseUnconfirmedForShutdown() }()
	admission, err := c.ReserveUnconfirmed(4096)
	if err != nil || admission == nil {
		t.Fatal(admission, err)
	}
	if _, err := c.ReserveUnconfirmed(1<<20 - 4096 + 1); err == nil {
		t.Fatal("IO admission exceeded configured unconfirmed capacity")
	}
	second, err := c.ReserveUnconfirmed(1)
	if err != nil {
		t.Fatal("second bounded entry was rejected below byte capacity", err)
	}
	if _, err := c.ReserveUnconfirmed(1); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("fixed release-entry table allowed an unbounded third admission", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	commit, err := c.RecordDurableWithAdmission(Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("durable")}, admission)
	if err != nil || commit.Index != 2 {
		t.Fatal(commit, err)
	}
	if manager.Snapshot().Used.Unconfirmed != 4096 {
		t.Fatal("admission was not retained by the coordinator", manager.Snapshot())
	}
	if confirmed, err := c.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil || confirmed != 2 {
		t.Fatal(confirmed, err)
	}
	if got := manager.Snapshot().Used.Unconfirmed; got != 0 {
		t.Fatal("confirmed prefix did not release its admission", got)
	}
	if confirmed, err := c.Acknowledge(testAcknowledgement("halro-1", 2)); confirmed != 2 || err != nil || manager.Snapshot().Used.Unconfirmed != 0 {
		t.Fatalf("replayed cumulative ACK refunded an admission twice: confirmed=%d err=%v budget=%+v", confirmed, err, manager.Snapshot())
	}
}

func TestPrimaryOrderingSyncFailureRetainsAdmittedSourceBytes(t *testing.T) {
	journal, _ := windowHistory(t, 1, nil)
	manager := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = manager
	limits.MaxUnconfirmedEntries = 4
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 1 << 20
	coordinator, err := newPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { return nil }, func(func(LocalCommit) error) error { return nil }, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		coordinator.Stop()
		_ = journal.Close()
		_ = coordinator.ReleaseUnconfirmedForShutdown()
	})
	journal.durability = &orderingAdmissionFailure{file: journal.file}
	admission, err := coordinator.ReserveUnconfirmed(4096)
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("durable")}
	if _, err := coordinator.RecordDurableWithAdmission(frame, admission); err == nil {
		t.Fatal("injected ordering journal sync failure was hidden")
	}
	if coordinator.poisoned == nil || manager.Snapshot().Used.Unconfirmed != 4096 || len(coordinator.unconfirmedEntries) != 1 {
		t.Fatalf("uncertain ordering append refunded its source admission: poisoned=%v entries=%v budget=%+v", coordinator.poisoned, coordinator.unconfirmedEntries, manager.Snapshot())
	}
}

func TestPrimaryConfirmedStateFailureRetainsAdmittedSourceBytes(t *testing.T) {
	journal, _ := windowHistory(t, 1, nil)
	manager := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = manager
	limits.MaxUnconfirmedEntries = 4
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 1 << 20
	persistErr := errors.New("injected confirmed member-state failure")
	failConfirmedPersist := false
	coordinator, err := newPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1"}, journal, &recordingOutbound{}, func(progress PrimaryProgress) error {
		if failConfirmedPersist && progress.ConfirmedIndex > 1 {
			return persistErr
		}
		return nil
	}, func(func(LocalCommit) error) error { return nil }, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		coordinator.Stop()
		_ = journal.Close()
		_ = coordinator.ReleaseUnconfirmedForShutdown()
	})
	admission, err := coordinator.ReserveUnconfirmed(4096)
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("durable")}
	if _, err := coordinator.RecordDurableWithAdmission(frame, admission); err != nil {
		t.Fatal(err)
	}
	failConfirmedPersist = true
	if confirmed, err := coordinator.Acknowledge(testAcknowledgement("halro-1", 2)); confirmed != 1 || !errors.Is(err, persistErr) {
		t.Fatalf("confirmed=%d, error=%v", confirmed, err)
	}
	if coordinator.poisoned == nil || manager.Snapshot().Used.Unconfirmed != 4096 || len(coordinator.unconfirmedEntries) != 1 {
		t.Fatalf("failed confirmed-state publication refunded source admission: poisoned=%v entries=%v budget=%+v", coordinator.poisoned, coordinator.unconfirmedEntries, manager.Snapshot())
	}
}

func TestPrimaryConfirmedStateFailureReconstructsAdmissionAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	journal, err := OpenOrderingJournal(path, key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	anchor := testConfirmationFrame(1, KindLeadershipEstablished)
	_, anchorDigest, err := anchor.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	anchorRecord, err := journal.Append(OrderingRecord{Kind: anchor.Kind, Index: 1, Term: anchor.Term, FrameDigest: anchorDigest})
	if err != nil {
		t.Fatal(err)
	}
	firstManager := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = firstManager
	limits.MaxUnconfirmedEntries = 4
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 1 << 20
	persistErr := errors.New("injected confirmed member-state failure")
	failConfirmedPersist := false
	first, err := newPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1"}, journal, &recordingOutbound{}, func(progress PrimaryProgress) error {
		if failConfirmedPersist && progress.ConfirmedIndex > 1 {
			return persistErr
		}
		return nil
	}, func(func(LocalCommit) error) error { return nil }, nil, limits)
	if err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
	admission, err := first.ReserveUnconfirmed(4096)
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("durable")}
	commit, err := first.RecordDurableWithAdmission(frame, admission)
	if err != nil {
		t.Fatal(err)
	}
	failConfirmedPersist = true
	if _, err := first.Acknowledge(testAcknowledgement("halro-1", 2)); !errors.Is(err, persistErr) {
		t.Fatalf("confirmed-state failure was hidden: %v", err)
	}
	if firstManager.Snapshot().Used.Unconfirmed != 4096 {
		t.Fatal("failed confirmed-state publication released the old-tenure admission", firstManager.Snapshot())
	}
	first.Stop()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.ReleaseUnconfirmedForShutdown(); err != nil {
		t.Fatal(err)
	}
	if firstManager.Snapshot().Used.Unconfirmed != 0 {
		t.Fatal("joined old tenure retained its in-process admission", firstManager.Snapshot())
	}

	journal, err = OpenOrderingJournal(path, key, header, 1, anchorRecord.MAC)
	if err != nil {
		t.Fatal(err)
	}
	secondManager := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits.UnconfirmedBudget = secondManager
	second, err := newPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { return nil }, func(consume func(LocalCommit) error) error { return consume(commit) }, nil, limits)
	if err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		second.Stop()
		_ = journal.Close()
		_ = second.ReleaseUnconfirmedForShutdown()
	})
	if len(second.unconfirmedEntries) != 1 || second.unconfirmedEntries[0].bytes == 0 || secondManager.Snapshot().Used.Unconfirmed != second.unconfirmedEntries[0].bytes {
		t.Fatalf("restart did not reconstruct and charge the unconfirmed suffix before use: entries=%v budget=%+v", second.unconfirmedEntries, secondManager.Snapshot())
	}
	if confirmed, err := second.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil || confirmed != 2 {
		t.Fatalf("recovered suffix could not complete after restart: confirmed=%d err=%v", confirmed, err)
	}
	if secondManager.Snapshot().Used.Unconfirmed != 0 {
		t.Fatal("recovered confirmation did not retire its Recovery admission", secondManager.Snapshot())
	}
}

func TestPrimaryConcurrentPeerACKsShareAdmissionAndConfirmOnce(t *testing.T) {
	journal, source := windowHistory(t, 2, []byte("shared"))
	manager := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = manager
	limits.MaxUnconfirmedEntries = 4
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 1 << 20
	var stateWrites atomic.Int32
	coordinator, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1", "halro-2"}, journal, &recordingOutbound{}, func(PrimaryProgress) error {
		stateWrites.Add(1)
		return nil
	}, source, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		coordinator.Stop()
		_ = coordinator.ReleaseUnconfirmedForShutdown()
	})
	if err := coordinator.PreparePeer("halro-1", 1, 1, (&windowSender{}).send); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.PreparePeer("halro-2", 1, 1, (&windowSender{}).send); err != nil {
		t.Fatal(err)
	}
	if coordinator.windowFor("halro-1").queuedThrough != 2 || coordinator.windowFor("halro-2").queuedThrough != 2 {
		t.Fatal("both peers must independently receive the recovered frame before ACK")
	}
	charged := manager.Snapshot().Used.Unconfirmed
	if charged == 0 || len(coordinator.unconfirmedEntries) != 1 {
		t.Fatalf("recovered suffix was not charged to the shared owner: bytes=%d entries=%v", charged, coordinator.unconfirmedEntries)
	}
	stateWritesBeforeACK := stateWrites.Load()

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, peer := range []string{"halro-1", "halro-2"} {
		peer := peer
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			confirmed, err := coordinator.Acknowledge(testAcknowledgement(peer, 2))
			if err == nil && confirmed != 2 {
				err = errors.New("peer ACK did not observe the confirmed prefix")
			}
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if writes := stateWrites.Load() - stateWritesBeforeACK; writes != 1 {
		t.Fatalf("concurrent cumulative ACKs published confirmed state %d times, want once", writes)
	}
	if used := manager.Snapshot().Used.Unconfirmed; used != 0 || len(coordinator.unconfirmedEntries) != 0 {
		t.Fatalf("concurrent peers refunded the shared admission incorrectly: used=%d entries=%v", used, coordinator.unconfirmedEntries)
	}
}

func TestProviderObjectAdmissionsPrecedeSourceAndTransferPerOrderedChunk(t *testing.T) {
	journal, source := windowHistory(t, 1, nil)
	manager := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = manager
	limits.MaxUnconfirmedEntries = 8
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 1 << 20
	c, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1", "halro-2"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { return nil }, source, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Stop(); _ = c.ReleaseUnconfirmedForShutdown() }()
	sealed := []byte("sealed-object")
	admissions, err := c.ReserveProviderObjectAdmissions("object.content", sealed)
	if err != nil || len(admissions) != 1 {
		t.Fatal(len(admissions), err)
	}
	if got := manager.Snapshot().Used.Unconfirmed; got == 0 {
		t.Fatal("object source publication could begin without an unconfirmed reservation")
	}
	index, err := c.RecordProviderObjectWithAdmissions("object.content", sealed, admissions)
	if err != nil || index != 2 || len(c.unconfirmedEntries) != 1 {
		t.Fatal(index, len(c.unconfirmedEntries), err)
	}
}

func assertCandidateCharges(t *testing.T, c *PrimaryCoordinator, m *resourcebudget.Manager, count int) {
	t.Helper()
	stats := m.Snapshot()
	if stats.RecoveryClaims != count || stats.Classes[resourcebudget.Recovery].RAM != int64(count)*MinPrimaryCandidateRAMBytes || stats.Used.RAM != int64(count)*(MinPrimaryCandidateRAMBytes+resourcebudget.ClaimRAMBytes) {
		t.Fatalf("candidate fees %+v, want %d", stats, count)
	}
	held := 0
	for i := range c.peerWindows {
		p := &c.peerWindows[i]
		if p.candidate.Index != 0 {
			held++
		} else if p.releaseCandidate != nil {
			t.Fatal("empty candidate retained envelope")
		}
	}
	if held != count {
		t.Fatal("payload ownership differs from claims", held, count)
	}
}
func TestPrimaryCandidateDuplicateACKAndRetryDoNotRereadByteBlockedFrame(t *testing.T) {
	journal, base := windowHistory(t, 5, bytes.Repeat([]byte{1}, 8<<20))
	source := &candidateCountSource{DurableSource: base, reads: map[uint64]int{}}
	m := candidateManager(t, 3)
	c := windowPrimary(t, journal, source, 0, candidateLimits(m), nil)
	assertCandidateCharges(t, c, m, 0)
	clear(source.reads)
	source.beforeRead = func() {
		if m.Snapshot().RecoveryClaims == 0 {
			t.Fatal("source read before RAM admission")
		}
	}
	sender := &windowSender{}
	send := func(b []byte) error {
		if m.Snapshot().RecoveryClaims == 0 {
			t.Fatal("candidate released before synchronous queue copy")
		}
		return sender.send(b)
	}
	if err := c.PreparePeer("halro-1", 0, 0, send); err != nil {
		t.Fatal(err)
	}
	if c.windowFor("halro-1").candidate.Index != 4 || source.reads[3] != 1 || len(sender.frames) != 3 {
		t.Fatal("fixture did not byte-block candidate 4", source.reads, sender.frames)
	}
	assertCandidateCharges(t, c, m, 1)
	for i := 0; i < 100; i++ {
		if _, err := c.Acknowledge(testAcknowledgement("halro-1", 1)); err != nil {
			t.Fatal(err)
		}
		if err := c.RetryPending(); err != nil {
			t.Fatal(err)
		}
	}
	if source.reads[3] != 1 {
		t.Fatal("byte-blocked frame reread", source.reads)
	}
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", 4)); err == nil {
		t.Fatal("unsent candidate credited as durable")
	}
	for i := uint64(2); i <= 5; i++ {
		ack := testAcknowledgement("halro-1", i)
		ack.AppliedIndex = i
		if _, err := c.Acknowledge(ack); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(1); i <= 4; i++ {
		if source.reads[i] != 1 {
			t.Fatal("frame skipped or rebuilt", source.reads)
		}
	}
	assertCandidateCharges(t, c, m, 0)
	t.Log("100 duplicate ACK + 100 retries: blocked index 4 source reads=1; complete suffix source reads=1 per data frame")
}
func TestPrimaryCandidateFeesFollowReconnectForgetQueueFailureAndStop(t *testing.T) {
	journal, source := windowHistory(t, 5, bytes.Repeat([]byte{1}, 8<<20))
	m := candidateManager(t, 3)
	c := windowPrimary(t, journal, source, 0, candidateLimits(m), nil)
	first, second := &windowSender{}, &windowSender{}
	if err := c.PreparePeer("halro-1", 0, 0, first.send); err != nil {
		t.Fatal(err)
	}
	if err := c.PreparePeer("halro-2", 0, 0, second.send); err != nil {
		t.Fatal(err)
	}
	assertCandidateCharges(t, c, m, 2)
	if err := c.PreparePeer("halro-1", 0, 0, first.send); err != nil {
		t.Fatal(err)
	}
	assertCandidateCharges(t, c, m, 2)
	first.err = ErrPeerSessionClosed
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", 2)); err == nil {
		t.Fatal("queue failure hidden")
	}
	assertCandidateCharges(t, c, m, 1)
	if _, available, _ := c.Status(); !available || c.poisoned != nil {
		t.Fatal("failed session destroyed healthy quorum")
	}
	c.ForgetPeer("halro-2")
	assertCandidateCharges(t, c, m, 0)
	if err := c.PreparePeer("halro-2", 0, 0, second.send); err != nil {
		t.Fatal(err)
	}
	assertCandidateCharges(t, c, m, 1)
	c.Stop()
	c.Stop()
	assertCandidateCharges(t, c, m, 0)
}
func TestPrimaryCandidateBudgetDenialPrecedesSourceAndPreservesDurableFacts(t *testing.T) {
	journal, base := windowHistory(t, 5, bytes.Repeat([]byte{1}, 8<<20))
	source := &candidateCountSource{DurableSource: base, reads: map[uint64]int{}}
	m := candidateManager(t, 1)
	c := windowPrimary(t, journal, source, 0, candidateLimits(m), nil)
	first, second := &windowSender{}, &windowSender{}
	if err := c.PreparePeer("halro-1", 0, 0, first.send); err != nil {
		t.Fatal(err)
	}
	clear(source.reads)
	if err := c.PreparePeer("halro-2", 0, 0, second.send); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal(err)
	}
	if len(source.reads) != 0 || len(second.frames) != 0 || c.poisoned != nil {
		t.Fatal("capacity failure performed source work or poisoned facts")
	}
	if _, available, _ := c.Status(); !available {
		t.Fatal("healthy quorum lost")
	}
	c.ForgetPeer("halro-1")
	if err := c.PreparePeer("halro-2", 0, 0, second.send); err != nil {
		t.Fatal("reconnect did not recover capacity", err)
	}
	assertCandidateCharges(t, c, m, 1)
	c.Stop()
	assertCandidateCharges(t, c, m, 0)
	blocker, err := m.TryAcquireRecovery(resourcebudget.Cost{RAM: MinPrimaryCandidateRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	clear(source.reads)
	published := false
	_, err = NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { published = true; return nil }, source, candidateLimits(m))
	if !errors.Is(err, resourcebudget.ErrExhausted) || len(source.reads) != 0 || published {
		t.Fatal("constructor read/published before admission", err, source.reads, published)
	}
	if err := blocker.Release(); err != nil {
		t.Fatal(err)
	}
}
func TestPrimaryCandidateGlobalFailureReturnsAllPeerFees(t *testing.T) {
	for _, stage := range []string{"source", "persist", "durable-persist", "register", "pagination"} {
		t.Run(stage, func(t *testing.T) {
			journal, source := windowHistory(t, 6, bytes.Repeat([]byte{1}, 8<<20))
			m := candidateManager(t, 3)
			failed := false
			c := windowPrimary(t, journal, source, 0, candidateLimits(m), func(PrimaryProgress) error {
				if failed && (stage == "persist" || stage == "durable-persist") {
					return errors.New("state fsync failed")
				}
				return nil
			})
			for _, node := range []string{"halro-1", "halro-2"} {
				sender := &windowSender{}
				if err := c.PreparePeer(node, 0, 0, sender.send); err != nil {
					t.Fatal(err)
				}
			}
			assertCandidateCharges(t, c, m, 2)
			failed = true
			// Candidate 4 remains immutable; corrupt the next source read (index 5).
			if stage == "source" {
				source.payloads[sourceKey(StoreLedger, 1, 4, 4)] = []byte("changed")
			}
			changed := c.changed
			if stage == "register" {
				// Controlled invariant failure after the ordering append.
				c.confirmations.lastRegistered--
				if _, err := c.RecordSchemaBoundary(1, 2); err == nil {
					t.Fatal("register failure hidden")
				}
			} else if stage == "durable-persist" {
				if _, err := c.RecordSchemaBoundary(1, 2); err == nil {
					t.Fatal("durable watermark failure hidden")
				}
			} else if stage == "pagination" {
				source.payloads[sourceKey(StoreLedger, 1, 1, 1)] = []byte("changed")
				if _, err := c.PendingFrom(0); err == nil {
					t.Fatal("pagination source corruption hidden")
				}
			} else if _, err := c.Acknowledge(testAcknowledgement("halro-1", 2)); err == nil {
				t.Fatal("global failure hidden")
			}

			if c.poisoned == nil {
				t.Fatal("uncertain transition did not poison")
			}
			if _, available, cause := c.Status(); available || cause == nil {
				t.Fatal("global poison retained confirmable status", available, cause)
			}
			select {
			case <-changed:
			default:
				t.Fatal("global poison did not wake confirmation waiters")
			}
			assertCandidateCharges(t, c, m, 0)
		})
	}
}
func TestPrimaryCandidateNativeMutationRevokesEvenStillByteBlockedFrame(t *testing.T) {
	for _, stage := range []string{"mutation", "close", "pagination-corruption"} {
		t.Run(stage, func(t *testing.T) {

			native, _, _, _ := nativeRangeFixture(t)
			native.options.ProviderObjectDir = filepath.Join(t.TempDir(), "objects")
			object := bytes.Repeat([]byte{0x41}, 32<<20)
			if err := PersistProviderObjectSource(native.options.ProviderObjectDir, "file_candidate.content", object); err != nil {
				t.Fatal(err)
			}
			writer, journal := newTestPrimary(t, &recordingOutbound{})
			t.Cleanup(func() { _ = journal.Close() })
			if _, err := writer.RecordDurable(Frame{Kind: KindLeadershipEstablished}); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(object)
			for offset := 0; offset < len(object); offset += 8 << 20 {
				meta, err := (ProviderObjectMetadata{Name: "file_candidate.content", Offset: uint64(offset), ChunkLength: 8 << 20, TotalLength: uint64(len(object)), Digest: digest, Final: offset+8<<20 == len(object)}).MarshalBinary()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.RecordDurable(Frame{Kind: KindProviderObject, Metadata: meta, Payload: object[offset : offset+8<<20]}); err != nil {
					t.Fatal(err)
				}
			}
			object = nil
			m := candidateManager(t, 3)
			c := windowPrimary(t, journal, native, 0, candidateLimits(m), nil)
			sender := &windowSender{}
			if err := c.PreparePeer("halro-1", 0, 0, sender.send); err != nil {
				t.Fatal(err)
			}
			if c.windowFor("halro-1").candidate.Index != 4 || len(sender.frames) != 3 {
				t.Fatal("real native object did not block")
			}
			if err := c.PreparePeer("halro-2", 0, 0, (&windowSender{}).send); err != nil {
				t.Fatal(err)
			}
			assertCandidateCharges(t, c, m, 2)
			if nativeReaderCount(native) != 2 {
				t.Fatal("two cached candidates did not own two readers")
			}
			switch stage {
			case "mutation":
				if err := native.Truncate(StoreAudit, StoreCursor{Generation: 1, Sequence: 1}); err != nil {
					t.Fatal(err)
				}
				if err := c.RetryPending(); err == nil || c.poisoned == nil {
					t.Fatal("stale candidate retained after mutation", err)
				}
			case "close":
				native.Close()
				if err := c.RetryPending(); err == nil || c.poisoned == nil {
					t.Fatal("cached candidate crossed source close", err)
				}
			default:
				file, err := os.OpenFile(filepath.Join(native.options.ProviderObjectDir, "file_candidate.content"), os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt([]byte{0x42}, 0)
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
				if _, err := c.PendingFrom(0); err == nil || c.poisoned == nil {
					t.Fatal("corrupt source pagination retained cached candidates", err)
				}
			}

			if len(sender.frames) != 3 {
				t.Fatal("old candidate was sent")
			}
			assertCandidateCharges(t, c, m, 0)
			if nativeReaderCount(native) != 0 {
				t.Fatal("global source poison retained NativeSource leases")
			}

		})
	}
}
