package replication

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type recordingOutbound struct {
	indexes       []uint64
	frames        [][]byte
	noticeIndexes []uint64
	notices       [][]byte
	acks          []Acknowledgement
	err           error
	failIndexes   map[uint64]error
}

func (o *recordingOutbound) QueueFrame(index uint64, encoded []byte) error {
	if err := o.failIndexes[index]; err != nil {
		return err
	}
	if o.err != nil {
		return o.err
	}
	o.indexes = append(o.indexes, index)
	o.frames = append(o.frames, encoded)
	return nil
}

func (o *recordingOutbound) QueueCommitNotice(index uint64, encoded []byte) error {
	if o.err != nil {
		return o.err
	}
	o.noticeIndexes = append(o.noticeIndexes, index)
	o.notices = append(o.notices, encoded)
	return nil
}

func (o *recordingOutbound) Acknowledge(nodeID string, durableIndex, appliedIndex uint64) {
	o.acks = append(o.acks, Acknowledgement{NodeID: nodeID, DurableIndex: durableIndex, AppliedIndex: appliedIndex})
}

func newTestPrimary(t *testing.T, outbound OutboundQueue) (*PrimaryCoordinator, *OrderingJournal) {
	t.Helper()
	journal, err := OpenOrderingJournal(
		filepath.Join(t.TempDir(), "ordering.journal"),
		[]byte("0123456789abcdef0123456789abcdef"),
		OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{},
	)
	if err != nil {
		t.Fatal(err)
	}
	primary, err := NewPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1", "halro-2"}, journal, outbound)
	if err != nil {
		journal.Close()
		t.Fatal(err)
	}
	return primary, journal
}

func TestPrimaryCoordinatorAllocatesOrdersQueuesAndConfirms(t *testing.T) {
	outbound := &recordingOutbound{}
	primary, journal := newTestPrimary(t, outbound)
	defer journal.Close()
	commit, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone})
	if err != nil {
		t.Fatal(err)
	}
	if commit.Index != 1 || !commit.ReplicationAvailable || len(outbound.frames) != 1 {
		t.Fatalf("commit=%#v queued=%d", commit, len(outbound.frames))
	}
	frame, err := UnmarshalFrame(outbound.frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if frame.Index != 1 || frame.Term != 7 || frame.ConfirmedIndex != 0 {
		t.Fatalf("queued frame=%#v", frame)
	}
	waiting := make(chan error, 1)
	go func() { waiting <- primary.WaitConfirmed(context.Background(), 1) }()
	select {
	case err := <-waiting:
		t.Fatalf("wait returned before ACK: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	ack := testAcknowledgement("halro-1", 1)
	if confirmed, err := primary.Acknowledge(ack); err != nil || confirmed != 1 {
		t.Fatalf("confirmed=%d err=%v", confirmed, err)
	}
	if err := <-waiting; err != nil {
		t.Fatal(err)
	}
	if len(outbound.frames) != 1 || len(outbound.notices) != 1 {
		t.Fatalf("queued frames=%d notices=%d, want one of each", len(outbound.frames), len(outbound.notices))
	}
	notice, err := UnmarshalCommitNotice(outbound.notices[0])
	if err != nil || notice.ConfirmedIndex != 1 || notice.NodeID != "halro-0" {
		t.Fatalf("notice=%#v err=%v", notice, err)
	}
	applied := ack
	applied.AppliedIndex = 1
	if _, err := primary.Acknowledge(applied); err != nil {
		t.Fatal(err)
	}
	if pending := primary.PendingCommitNotice(); len(pending) == 0 {
		t.Fatal("one peer incorrectly cleared a notice still owed to the other peer")
	}
	if err := primary.RetryPending(); err != nil {
		t.Fatal(err)
	}
	if len(outbound.notices) != 2 {
		t.Fatalf("notice was not retained for a disconnected peer: queued=%d", len(outbound.notices))
	}
	secondApplied := testAcknowledgement("halro-2", 1)
	secondApplied.AppliedIndex = 1
	if _, err := primary.Acknowledge(secondApplied); err != nil {
		t.Fatal(err)
	}
	if pending := primary.PendingCommitNotice(); len(pending) != 0 {
		t.Fatalf("commit notice remained pending after every peer applied: %x", pending)
	}
}

func TestPrimaryCoordinatorKeepsLocalCommitWhenReplicationQueueFails(t *testing.T) {
	outbound := &recordingOutbound{err: errors.New("peer disconnected")}
	primary, journal := newTestPrimary(t, outbound)
	defer journal.Close()
	commit, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone})
	if err != nil {
		t.Fatal(err)
	}
	if commit.Index != 1 || commit.ReplicationAvailable {
		t.Fatalf("commit=%#v", commit)
	}
	if index, _, _ := journal.Head(); index != 1 {
		t.Fatalf("durable ordering head=%d, want 1", index)
	}
	pending := primary.PendingFrom(0)
	if len(pending) != 1 || pending[0].Index != 1 {
		t.Fatalf("pending=%#v", pending)
	}
	if err := primary.WaitConfirmed(context.Background(), 1); !errors.Is(err, ErrReplicationUnavailable) {
		t.Fatalf("wait error=%v", err)
	}
	if primaryRetry := ReplicationRetryAfter(); primaryRetry != time.Second {
		t.Fatalf("retry-after=%s", primaryRetry)
	}
}

func TestPrimaryCoordinatorWaitHonorsContext(t *testing.T) {
	primary, journal := newTestPrimary(t, &recordingOutbound{})
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := primary.WaitConfirmed(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v", err)
	}
}

func TestPrimaryCoordinatorDoesNotRecoverAvailabilityFromAStaleACK(t *testing.T) {
	outbound := &recordingOutbound{}
	primary, journal := newTestPrimary(t, outbound)
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	ack := testAcknowledgement("halro-1", 1)
	if _, err := primary.Acknowledge(ack); err != nil {
		t.Fatal(err)
	}
	outbound.err = errors.New("queue disconnected")
	if _, err := primary.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("frame"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Acknowledge(ack); err != nil {
		t.Fatal(err)
	}
	if _, available, _ := primary.Status(); available {
		t.Fatal("a stale ACK incorrectly restored replication availability")
	}
	outbound.err = nil
	if err := primary.RetryPending(); err != nil {
		t.Fatal(err)
	}
	if _, available, _ := primary.Status(); !available {
		t.Fatal("successful retry did not restore replication availability")
	}
}

func TestPrimaryCoordinatorDoesNotRecoverAvailabilityFromAnEarlierAdvancingACK(t *testing.T) {
	outbound := &recordingOutbound{}
	primary, journal := newTestPrimary(t, outbound)
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	outbound.err = errors.New("queue disconnected")
	if _, err := primary.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger, StoreGeneration: 1,
		StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("frame"),
	}); err != nil {
		t.Fatal(err)
	}
	outbound.err = nil
	if confirmed, err := primary.Acknowledge(testAcknowledgement("halro-1", 1)); err != nil || confirmed != 1 {
		t.Fatalf("confirmed=%d err=%v", confirmed, err)
	}
	if _, available, _ := primary.Status(); available {
		t.Fatal("ACK for index 1 incorrectly healed the failed queue at index 2")
	}
}

func TestPrimaryCoordinatorTracksTheHighestOfMultipleQueueFailures(t *testing.T) {
	disconnected := errors.New("queue disconnected")
	outbound := &recordingOutbound{failIndexes: map[uint64]error{2: disconnected, 4: disconnected}}
	primary, journal := newTestPrimary(t, outbound)
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	for index := uint64(2); index <= 4; index++ {
		if _, err := primary.RecordDurable(Frame{
			Kind: KindData, Store: StoreLedger, StoreGeneration: 1,
			StoreSequenceFirst: index - 1, StoreSequenceLast: index - 1, Payload: []byte{byte(index)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if confirmed, err := primary.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil || confirmed != 2 {
		t.Fatalf("confirmed=%d err=%v", confirmed, err)
	}
	if _, available, _ := primary.Status(); available {
		t.Fatal("ACK for the first failed enqueue hid the later failure")
	}
}

func TestPrimaryCoordinatorResumesEstablishedTermWithoutSecondAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	journal, err := OpenOrderingJournal(path, key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	frame := testConfirmationFrame(1, KindLeadershipEstablished)
	_, digest, err := frame.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := journal.Append(OrderingRecord{Kind: KindLeadershipEstablished, Index: 1, Term: 7, FrameDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := NewPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1"}, journal, &recordingOutbound{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("frame"),
	}); err != nil {
		t.Fatal(err)
	}
	if anchor.MAC == ([32]byte{}) {
		t.Fatal("anchor MAC was not populated")
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrimaryCoordinatorRecoversAndRetransmitsUnconfirmedSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	journal, err := OpenOrderingJournal(path, key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := NewPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1", "halro-2"}, journal, &recordingOutbound{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	payload := []byte("frame")
	if _, err := primary.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger, StoreGeneration: 1,
		StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := primary.Acknowledge(testAcknowledgement("halro-1", 1)); err != nil || confirmed != 1 {
		t.Fatalf("confirmed=%d err=%v", confirmed, err)
	}
	first, err := journal.Record(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenOrderingJournal(path, key, header, 1, first.MAC)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	record, err := reopened.Record(2)
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{
		Kind: record.Kind, Store: record.Store, Index: record.Index, Term: record.Term,
		ConfirmedIndex: record.ConfirmedIndex, StoreGeneration: record.StoreGeneration,
		StoreSequenceFirst: record.StoreSequenceFirst, StoreSequenceLast: record.StoreSequenceLast,
		ClusterID: "production-a", Incarnation: "inc_01", Metadata: record.Metadata, Payload: payload,
	}
	encoded, digest, err := frame.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != record.FrameDigest || record.ConfirmedIndex != 0 {
		t.Fatalf("reconstructed digest=%x record=%x confirmed=%d", digest, record.FrameDigest, record.ConfirmedIndex)
	}
	recoveredOutbound := &recordingOutbound{}
	recovered, err := NewPrimaryCoordinator(
		"production-a", "inc_01", "halro-0", 7, 1, []string{"halro-1", "halro-2"},
		reopened, recoveredOutbound, LocalCommit{Index: 2, Encoded: encoded},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveredOutbound.frames) != 1 {
		t.Fatalf("retransmitted frames=%d, want 1", len(recoveredOutbound.frames))
	}
	if confirmed, err := recovered.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil || confirmed != 2 {
		t.Fatalf("confirmed=%d err=%v", confirmed, err)
	}
}

func TestPrimaryCoordinatorAuthenticatesEntireRecoveredSuffixBeforeQueueing(t *testing.T) {
	outbound := &recordingOutbound{}
	primary, journal := newTestPrimary(t, outbound)
	defer journal.Close()
	first, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone})
	if err != nil {
		t.Fatal(err)
	}
	second, err := primary.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger, StoreGeneration: 1,
		StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("frame"),
	})
	if err != nil {
		t.Fatal(err)
	}
	second.Encoded[len(second.Encoded)-1] ^= 1
	recoveredOutbound := &recordingOutbound{}
	if _, err := NewPrimaryCoordinator(
		"production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1", "halro-2"},
		journal, recoveredOutbound, first, second,
	); err == nil {
		t.Fatal("corrupt second recovered frame was accepted")
	}
	if len(recoveredOutbound.frames) != 0 || len(recoveredOutbound.notices) != 0 {
		t.Fatalf("outbound side effects before full validation: frames=%d notices=%d", len(recoveredOutbound.frames), len(recoveredOutbound.notices))
	}
}
