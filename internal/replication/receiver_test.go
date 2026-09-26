package replication

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

type recordingFrameSink struct {
	frames []Frame
	err    error
}

type failNthOrderingSync struct {
	file  *os.File
	nth   int
	calls int
}

func (d *failNthOrderingSync) Write(value []byte) (int, error) { return d.file.Write(value) }
func (d *failNthOrderingSync) Sync() error {
	d.calls++
	if d.calls == d.nth {
		return errors.New("injected ordering sync failure")
	}
	return d.file.Sync()
}

func TestReplicaReceiverRefusesFrameThatConfirmsItselfBeforePersisting(t *testing.T) {
	sink := &recordingFrameSink{}
	receiver, journal := newTestReceiver(t, sink)
	defer journal.Close()
	encoded := encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)
	body := encoded[4:]
	binary.BigEndian.PutUint64(body[32:40], 1)
	digest := frameDigest(body)
	copy(body[frameDigestOffset:frameDigestOffset+len(digest)], digest[:])
	if _, err := receiver.Receive(encoded); err == nil || !strings.Contains(err.Error(), "confirmed index") {
		t.Fatalf("self-confirm error=%v", err)
	}
	if len(sink.frames) != 0 {
		t.Fatalf("persisted %d frames before refusing self-confirmation", len(sink.frames))
	}
}

func (s *recordingFrameSink) Persist(frame Frame) error {
	if s.err != nil {
		return s.err
	}
	s.frames = append(s.frames, frame)
	return nil
}

func newTestReceiver(t *testing.T, sink DurableFrameSink) (*ReplicaReceiver, *OrderingJournal) {
	t.Helper()
	journal, err := OpenOrderingJournal(
		filepath.Join(t.TempDir(), "ordering.journal"),
		[]byte("0123456789abcdef0123456789abcdef"),
		OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{},
	)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 0, 0, journal, sink)
	if err != nil {
		journal.Close()
		t.Fatal(err)
	}
	return receiver, journal
}

func encodeTestReplicaFrame(t *testing.T, index uint64, kind Kind) []byte {
	t.Helper()
	frame := testConfirmationFrame(index, kind)
	if kind == KindData {
		frame.StoreSequenceFirst = index - 1
		frame.StoreSequenceLast = index - 1
	}
	encoded, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestReplicaReceiverPersistsBeforeAcknowledgingAndDeduplicates(t *testing.T) {
	sink := &recordingFrameSink{}
	receiver, journal := newTestReceiver(t, sink)
	defer journal.Close()
	anchor := encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)
	ack, err := receiver.Receive(anchor)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Index != 1 || len(sink.frames) != 1 {
		t.Fatalf("ack=%#v persisted=%d", ack, len(sink.frames))
	}
	if index, _, _ := journal.Head(); index != 1 {
		t.Fatalf("journal head=%d, want 1", index)
	}
	if _, err := receiver.Receive(anchor); err != nil {
		t.Fatal(err)
	}
	if len(sink.frames) != 1 {
		t.Fatalf("retransmission persisted %d frames, want 1", len(sink.frames))
	}
	data := encodeTestReplicaFrame(t, 2, KindData)
	ack, err = receiver.Receive(data)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Index != 2 || ack.AppliedIndex != 0 || len(sink.frames) != 2 {
		t.Fatalf("ack=%#v persisted=%d", ack, len(sink.frames))
	}
	if err := receiver.AdvanceApplied(2); err == nil || !strings.Contains(err.Error(), "confirmed") {
		t.Fatalf("unconfirmed apply error=%v", err)
	}
	if err := receiver.Confirm(CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if err := receiver.AdvanceApplied(2); err != nil {
		t.Fatal(err)
	}
	ack, err = receiver.Receive(data)
	if err != nil || ack.AppliedIndex != 2 {
		t.Fatalf("post-apply ack=%#v err=%v", ack, err)
	}
}

func TestReplicaReceiverFailsClosedBeforeOrderingOnSinkError(t *testing.T) {
	sink := &recordingFrameSink{err: errors.New("disk full")}
	receiver, journal := newTestReceiver(t, sink)
	defer journal.Close()
	if _, err := receiver.Receive(encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("sink error=%v", err)
	}
	if index, _, _ := journal.Head(); index != 0 {
		t.Fatalf("ordering advanced to %d after store failure", index)
	}
	if _, err := receiver.Receive(encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)); err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("poison error=%v", err)
	}
}

func TestReplicaReceiverRefusesGapWrongTermAndIndexFork(t *testing.T) {
	sink := &recordingFrameSink{}
	receiver, journal := newTestReceiver(t, sink)
	defer journal.Close()
	if _, err := receiver.Receive(encodeTestReplicaFrame(t, 2, KindLeadershipEstablished)); err == nil || !strings.Contains(err.Error(), "does not continue") {
		t.Fatalf("gap error=%v", err)
	}
	if _, err := receiver.Receive(encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	wrongTerm := testConfirmationFrame(2, KindData)
	wrongTerm.Term = 6
	encodedWrongTerm, err := wrongTerm.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(encodedWrongTerm); err == nil || !strings.Contains(err.Error(), "below") {
		t.Fatalf("old-term error=%v", err)
	}
	accepted := testConfirmationFrame(2, KindData)
	accepted.StoreSequenceFirst, accepted.StoreSequenceLast = 1, 1
	acceptedBytes, err := accepted.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(acceptedBytes); err != nil {
		t.Fatal(err)
	}
	fork := accepted
	fork.Payload = []byte("different frame")
	encodedFork, err := fork.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(encodedFork); err == nil || !strings.Contains(err.Error(), "different frame bytes") {
		t.Fatalf("fork error=%v", err)
	}
}

func TestReplicaReceiverRequiresContiguousPerStoreSequence(t *testing.T) {
	sink := &recordingFrameSink{}
	receiver, journal := newTestReceiver(t, sink)
	defer journal.Close()
	if _, err := receiver.Receive(encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	frame := testConfirmationFrame(2, KindData)
	frame.StoreSequenceFirst, frame.StoreSequenceLast = 4, 4
	encoded, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(encoded); err == nil || !strings.Contains(err.Error(), "seeded cursor") {
		t.Fatalf("store gap error=%v", err)
	}
}

func TestReplicaReceiverRequiresReseedWhenMetadataEpochChanges(t *testing.T) {
	sink := &recordingFrameSink{}
	receiver, journal := newTestReceiver(t, sink)
	defer journal.Close()
	if _, err := receiver.Receive(encodeTestReplicaFrame(t, 1, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	first := testConfirmationFrame(2, KindData)
	first.Store = StoreMetadata
	first.StoreGeneration = 4
	first.StoreSequenceFirst, first.StoreSequenceLast = 1, 1
	encoded, err := first.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(encoded); err != nil {
		t.Fatal(err)
	}
	nextEpoch := testConfirmationFrame(3, KindData)
	nextEpoch.Store = StoreMetadata
	nextEpoch.StoreGeneration = 5
	nextEpoch.StoreSequenceFirst, nextEpoch.StoreSequenceLast = 1, 1
	encoded, err = nextEpoch.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(encoded); !errors.Is(err, ErrMemberRequiresFullReseed) {
		t.Fatalf("epoch-change error=%v", err)
	}
	if len(sink.frames) != 2 {
		t.Fatalf("persisted %d frames, want anchor plus first epoch frame", len(sink.frames))
	}
}

func TestReplicaReceiverRefusesOrderingJournalFromHigherTermBeforePersisting(t *testing.T) {
	directory := t.TempDir()
	j, err := OpenOrderingJournal(
		filepath.Join(directory, "ordering.journal"),
		[]byte("0123456789abcdef0123456789abcdef"),
		OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	frame := testConfirmationFrame(1, KindLeadershipEstablished)
	frame.Term = 8
	_, digest, err := frame.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(OrderingRecord{Kind: KindLeadershipEstablished, Index: 1, Term: 8, FrameDigest: digest}); err != nil {
		t.Fatal(err)
	}
	sink := &recordingFrameSink{}
	if _, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 0, 0, j, sink); err == nil || !strings.Contains(err.Error(), "behind") {
		t.Fatalf("constructor error=%v", err)
	}
	if len(sink.frames) != 0 {
		t.Fatal("constructor persisted a frame")
	}
}

func TestReplicaRollRetransmissionCompletesNativeBeforeOrderingCrash(t *testing.T) {
	directory := t.TempDir()
	chainKey := []byte("abcdef0123456789abcdef0123456789")
	orderingKey := []byte("0123456789abcdef0123456789abcdef")
	primaryPath := filepath.Join(directory, "primary", "ledger.wal")
	replicaPath := filepath.Join(directory, "replica", "ledger.wal")
	primary, err := ledger.OpenWithOptions(primaryPath, ledger.NewStatus(), ledger.Options{MaxBatch: 1, ChainKey: chainKey})
	if err != nil {
		t.Fatal(err)
	}
	reservation := int64(100)
	event := ledger.Event{
		EventID: "evt_roll_crash", Kind: ledger.EventReservationCreated, RequestID: "req_roll_crash",
		AttemptID: "att_roll_crash", ProjectID: "prj_1", PeriodID: "prj_1:2026-09-26:tz1",
		OccurredAt: time.Now().UTC(), ReservationMicrosUSD: &reservation, PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
	}
	if _, err := primary.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(replicaPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replicaPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	baselineHead, err := ledger.ReplicationHeadAt(primaryPath, chainKey, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	rolled, err := primary.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}

	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	header.StoreCursors[StoreLedger-StoreLedger] = StoreCursor{Generation: 1, Sequence: 1}
	header.StoreHeads[StoreLedger-StoreLedger] = baselineHead
	orderingPath := filepath.Join(directory, "replica", "cluster", "ordering.journal")
	journal, err := OpenOrderingJournalWithOptions(orderingPath, orderingKey, header, 0, [32]byte{}, OrderingJournalOptions{
		WrapDurability: func(file *os.File) OrderingDurability {
			return &failNthOrderingSync{file: file, nth: 3}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	replica, err := ledger.OpenWithOptions(replicaPath, ledger.NewStatus(), ledger.Options{ChainKey: chainKey, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 0, 0, journal, &NativeSink{ledger: replica})
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := (Frame{Kind: KindLeadershipEstablished, Index: 1, Term: 7, ClusterID: "production-a", Incarnation: "inc_01"}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(anchor); err != nil {
		t.Fatal(err)
	}
	metadata, err := ledgerRollMetadata(rolled.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	metadataBytes, err := metadata.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	rollFrame := Frame{Kind: KindLedgerRoll, Index: 2, Term: 7, ClusterID: "production-a", Incarnation: "inc_01", Metadata: metadataBytes}
	rollBytes, rollDigest, err := rollFrame.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(rollBytes); err == nil || !strings.Contains(err.Error(), "injected ordering sync failure") {
		t.Fatalf("first Roll error=%v", err)
	}
	if replica.Generation() != 2 {
		t.Fatalf("native Roll did not publish before injected ordering failure: generation=%d", replica.Generation())
	}
	_, _, anchorMAC := journal.Head()
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	headerBytes, err := header.MarshalBinary(orderingKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(orderingPath, int64(len(headerBytes)+orderingRecordBytes)); err != nil {
		t.Fatal(err)
	}

	reopenedJournal, err := OpenOrderingJournal(orderingPath, orderingKey, header, 1, anchorMAC)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedJournal.Close()
	reopenedReplica, err := ledger.OpenWithOptions(replicaPath, ledger.NewStatus(), ledger.Options{ChainKey: chainKey, Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedReplica.Close()
	reopenedReceiver, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, 0, 0, reopenedJournal, &NativeSink{ledger: reopenedReplica})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := reopenedReceiver.Receive(rollBytes)
	if err != nil {
		t.Fatal(err)
	}
	if ack.DurableIndex != 2 || reopenedReplica.Generation() != 2 {
		t.Fatalf("retransmitted Roll ack=%#v generation=%d", ack, reopenedReplica.Generation())
	}
	record, err := reopenedJournal.Record(2)
	if err != nil {
		t.Fatal(err)
	}
	if record.FrameDigest != rollDigest {
		t.Fatalf("recovered Roll digest=%x, want original %x", record.FrameDigest, rollDigest)
	}
}
