package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func recoveryTargetPrimary(t *testing.T) (*PrimaryCoordinator, *OrderingJournal) {
	t.Helper()
	c, j := newTestPrimary(t, &recordingOutbound{})
	t.Cleanup(func() {
		c.Stop()
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, frame := range []Frame{
		{Kind: KindLeadershipEstablished, Store: StoreNone},
		{Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("ledger")},
		{Kind: KindData, Store: StoreAudit, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("audit")},
		{Kind: KindData, Store: StoreGovernance, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("governance")},
		{Kind: KindData, Store: StoreMetadata, StoreGeneration: 4, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("metadata")},
	} {
		if _, err := c.RecordDurable(frame); err != nil {
			t.Fatal(err)
		}
	}
	return c, j
}

func TestRecoveryTargetWaitsForCompleteFrozenOrderingCut(t *testing.T) {
	c, _ := recoveryTargetPrimary(t)
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{Generation: 1, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	evidence := target.Evidence()
	if evidence.Index != 5 || evidence.Term != 7 || evidence.MAC == ([32]byte{}) || evidence.Stores != ([4]StoreCursor{{1, 1}, {1, 1}, {1, 1}, {4, 1}}) {
		t.Fatal(evidence)
	}
	// Editing evidence cannot shrink the actual wait to the Ledger's index 2.
	evidence.Index = 2
	evidence.Stores[3] = StoreCursor{}
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryTarget(ctx, target) }()
	select {
	case err := <-done:
		t.Fatal("Ledger-only ACK confirmed complete recovery", err)
	case <-time.After(10 * time.Millisecond):
	}
	if _, err := c.RecordDurable(Frame{Kind: KindData, Store: StoreAudit, StoreGeneration: 1, StoreSequenceFirst: 2, StoreSequenceLast: 2, Payload: []byte("later audit")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", 5)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	later, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{Generation: 1, Sequence: 1})
	if err != nil || later.Evidence().Index != 6 {
		t.Fatal(later.Evidence(), err)
	}
	if err := c.WaitRecoveryTarget(context.Background(), RecoveryTarget{}); err == nil {
		t.Fatal("zero target confirmed")
	}
}

func TestRecoveryTargetRejectsMismatchForeignAndRevokedLifecycle(t *testing.T) {
	c, j := recoveryTargetPrimary(t)
	ctx := context.Background()
	for _, position := range []StoreCursor{{}, {1, 2}, {2, 1}} {
		if _, err := c.CaptureRecoveryTarget(ctx, position); err == nil {
			t.Fatal("mismatched Ledger cut accepted", position)
		}
	}
	target, err := c.CaptureRecoveryTarget(ctx, StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", 5)); err != nil {
		t.Fatal(err)
	}
	other, err := NewPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 5, []string{"halro-1", "halro-2"}, j, &recordingOutbound{}, func(PrimaryProgress) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer other.Stop()
	if err := other.WaitRecoveryTarget(ctx, target); err == nil {
		t.Fatal("another coordinator accepted old target")
	}
	c.MarkUnavailable(errors.New("authority withdrawn"))
	c.MarkAvailable()
	if err := c.WaitRecoveryTarget(ctx, target); err == nil {
		t.Fatal("withdrawal/restore retained old target")
	}
	fresh, err := c.CaptureRecoveryTarget(ctx, StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.FreezeForStepdown(5); err != nil {
		t.Fatal(err)
	}
	c.MarkAvailable()
	if err := c.WaitRecoveryTarget(ctx, fresh); err == nil {
		t.Fatal("stepdown/restore retained old target")
	}
	fresh, err = c.CaptureRecoveryTarget(ctx, StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	c.Stop()
	if err := c.WaitRecoveryTarget(ctx, fresh); err == nil {
		t.Fatal("stopped coordinator confirmed recovery")
	}
	if _, err := c.CaptureRecoveryTarget(ctx, StoreCursor{1, 1}); err == nil {
		t.Fatal("stopped coordinator captured recovery")
	}
}

func TestRecoveryTargetCancellationStopAndInvalidACKCannotSucceed(t *testing.T) {
	c, _ := recoveryTargetPrimary(t)
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, ack := range []Acknowledgement{testAcknowledgement("unknown", 5), func() Acknowledgement { a := testAcknowledgement("halro-1", 5); a.Term--; return a }(), testAcknowledgement("halro-1", 6)} {
		if _, err := c.Acknowledge(ack); err == nil {
			t.Fatal("invalid ACK accepted", ack)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.WaitRecoveryTarget(ctx, target); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.CaptureRecoveryTarget(ctx, StoreCursor{1, 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryTarget(context.Background(), target) }()
	c.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stop falsely confirmed")
		}
	case <-time.After(time.Second):
		t.Fatal("stopped recovery waiter stranded")
	}
}

func TestRecoveryTargetChecksJournalEvenWhenIndexAlreadyConfirmed(t *testing.T) {
	for _, action := range []string{"close", "tail-tamper", "higher-term", "poison"} {
		t.Run(action, func(t *testing.T) {
			c, j := recoveryTargetPrimary(t)
			target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{1, 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Acknowledge(testAcknowledgement("halro-1", 5)); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "close":
				if err := j.Close(); err != nil {
					t.Fatal(err)
				}
			case "tail-tamper":
				offset := j.dataOffset + 4*orderingRecordBytes + 4 + orderingRecordMACOffset
				var value [1]byte
				if _, err := j.file.ReadAt(value[:], offset); err != nil {
					t.Fatal(err)
				}
				value[0] ^= 1
				if _, err := j.file.WriteAt(value[:], offset); err != nil {
					t.Fatal(err)
				}
			case "higher-term":
				if _, err := j.Append(testLeadershipRecord(6, 8, "new authority")); err != nil {
					t.Fatal(err)
				}
			case "poison":
				j.mu.Lock()
				j.poisoned = errors.New("uncertain append")
				j.mu.Unlock()
			}
			if err := c.WaitRecoveryTarget(context.Background(), target); err == nil {
				t.Fatal("unusable journal falsely confirmed target")
			}
		})
	}
}

func TestRecoveryOrderingSummaryMatchesAuthenticatedHistoryAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.journal")
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01", StoreCursors: [4]StoreCursor{{3, 41}, {1, 5}, {1, 8}, {7, 19}}}
	for n := range header.StoreHeads {
		header.StoreHeads[n] = sha256.Sum256([]byte{byte(n + 1)})
	}
	j, err := OpenOrderingJournal(path, key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(testLeadershipRecord(1, 7, "anchor")); err != nil {
		t.Fatal(err)
	}
	expected := header.StoreCursors
	for n := 0; n < 512; n++ {
		store := Store(int(StoreLedger) + n%4)
		cursor := expected[store-StoreLedger]
		record := OrderingRecord{Kind: KindData, Store: store, Index: uint64(n + 2), Term: 7, StoreGeneration: cursor.Generation, StoreSequenceFirst: cursor.Sequence + 1, StoreSequenceLast: cursor.Sequence + 2, FrameDigest: sha256.Sum256([]byte{byte(n), byte(n >> 8)})}
		if _, err := j.Append(record); err != nil {
			t.Fatal(err)
		}
		expected[store-StoreLedger].Sequence += 2
		prefix, err := j.capturePrefix()
		if err != nil || prefix.Stores != expected {
			t.Fatal(n, prefix, err)
		}
		if n%64 == 0 {
			historical, err := j.StoreCursorsThrough(prefix.Index)
			if err != nil || historical != expected {
				t.Fatal(historical, err)
			}
		}
	}
	before, err := j.capturePrefix()
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExistingOrderingJournal(path, key, header.ClusterID, header.Incarnation, before.Index, before.MAC)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.capturePrefix()
	if err != nil || after != before {
		t.Fatal(after, before, err)
	}
	// An incomplete physical tail must not advance any cached cursor on restart.
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0, 0, 1, 48, 9}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	repaired, err := OpenExistingOrderingJournal(path, key, header.ClusterID, header.Incarnation, before.Index, before.MAC)
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()
	if prefix, err := repaired.capturePrefix(); err != nil || prefix != before {
		t.Fatal(prefix, err)
	}
}

func TestRecoveryOrderingSummaryRefusesSemanticGapAndUncertainAppend(t *testing.T) {
	for _, failure := range []string{"gap", "sync"} {
		t.Run(failure, func(t *testing.T) {
			options := OrderingJournalOptions{}
			if failure == "sync" {
				options.WrapDurability = func(f *os.File) OrderingDurability { return &failingOrderingDurability{file: f} }
			}
			j, err := OpenOrderingJournalWithOptions(filepath.Join(t.TempDir(), "order.journal"), []byte("0123456789abcdef0123456789abcdef"), OrderingHeader{ClusterID: "c", Incarnation: "i"}, 0, [32]byte{}, options)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			_, err = j.Append(testLeadershipRecord(1, 7, "anchor"))
			if failure == "gap" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = j.Append(testOrderingRecord(2, 7, "starts at two"))
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("uncertain append passed")
			}
			if _, err := j.capturePrefix(); err == nil {
				t.Fatal("invalid summary became usable")
			}
		})
	}
}

func TestRecoveryOrderingSummaryKeepsLedgerRollGenerationAndSequence(t *testing.T) {
	header := OrderingHeader{ClusterID: "c", Incarnation: "i"}
	header.StoreCursors[0] = StoreCursor{3, 41}
	header.StoreHeads[0] = sha256.Sum256([]byte("baseline"))
	j, err := OpenOrderingJournal(filepath.Join(t.TempDir(), "order.journal"), []byte("0123456789abcdef0123456789abcdef"), header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.Append(testLeadershipRecord(1, 7, "anchor")); err != nil {
		t.Fatal(err)
	}
	metadata, err := (LedgerRollMetadata{Generation: 3, FirstSequence: 1, LastSequence: 41, Length: 100, EndEpoch: 1}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(OrderingRecord{Kind: KindLedgerRoll, Store: StoreNone, Index: 2, Term: 7, Metadata: metadata, FrameDigest: sha256.Sum256([]byte("roll"))}); err != nil {
		t.Fatal(err)
	}
	prefix, err := j.capturePrefix()
	if err != nil || prefix.Stores[0] != (StoreCursor{4, 41}) {
		t.Fatal(prefix, err)
	}
	record := testOrderingRecord(3, 7, "new generation")
	record.StoreGeneration, record.StoreSequenceFirst, record.StoreSequenceLast = 4, 42, 43
	if _, err := j.Append(record); err != nil {
		t.Fatal(err)
	}
	prefix, err = j.capturePrefix()
	old, oldErr := j.StoreCursorsThrough(3)
	if err != nil || oldErr != nil || prefix.Stores != old || prefix.Stores[0] != (StoreCursor{4, 43}) {
		t.Fatal(prefix, old, err, oldErr)
	}
}

type recoveryTargetDurability struct {
	file *os.File
	fail bool
}

func TestRecoveryTargetPendingWaitObservesJournalClose(t *testing.T) {
	c, j := recoveryTargetPrimary(t)
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryTarget(context.Background(), target) }()
	select {
	case err := <-done:
		t.Fatal("wait did not start blocked", err)
	case <-time.After(10 * time.Millisecond):
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed journal confirmed recovery")
		}
	case <-time.After(time.Second):
		t.Fatal("journal close stranded recovery waiter")
	}
}

func TestRecoveryTargetPendingWaitObservesHigherJournalTerm(t *testing.T) {
	c, j := recoveryTargetPrimary(t)
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryTarget(context.Background(), target) }()
	select {
	case err := <-done:
		t.Fatal("wait did not start blocked", err)
	case <-time.After(10 * time.Millisecond):
	}
	if _, err := j.Append(testLeadershipRecord(6, 8, "new leader")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("new leadership confirmed old recovery target")
		}
	case <-time.After(time.Second):
		t.Fatal("higher journal term stranded recovery waiter")
	}
	newPrefix, err := j.capturePrefix()
	if err != nil {
		t.Fatal(err)
	}
	signal, err := j.verifyRecoveryPrefix(newPrefix)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-signal:
		t.Fatal("new leadership inherited a closed subscription and would spin")
	default:
	}
}

func TestRecoveryTargetPendingWaitObservesInvalidStoreContinuation(t *testing.T) {
	c, j := recoveryTargetPrimary(t)
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryTarget(context.Background(), target) }()
	select {
	case err := <-done:
		t.Fatal("wait did not start blocked", err)
	case <-time.After(10 * time.Millisecond):
	}
	record := testOrderingRecord(6, 7, "invalid audit continuation")
	record.Store, record.StoreGeneration = StoreAudit, 1
	record.StoreSequenceFirst, record.StoreSequenceLast = 3, 3
	if _, err := j.Append(record); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("invalid store continuation confirmed recovery")
		}
	case <-time.After(time.Second):
		t.Fatal("invalid store continuation stranded recovery waiter")
	}
}

func (d *recoveryTargetDurability) Write(value []byte) (int, error) { return d.file.Write(value) }
func (d *recoveryTargetDurability) Sync() error {
	if d.fail {
		return errors.New("injected recovery ordering fsync failure")
	}
	return d.file.Sync()
}

func TestRecoveryTargetWaiterWakesOnUncertainOrderingAppend(t *testing.T) {
	var durability *recoveryTargetDurability
	j, err := OpenOrderingJournalWithOptions(filepath.Join(t.TempDir(), "order.journal"), []byte("0123456789abcdef0123456789abcdef"), OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{}, OrderingJournalOptions{WrapDurability: func(f *os.File) OrderingDurability {
		durability = &recoveryTargetDurability{file: f}
		return durability
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c, err := NewPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1", "halro-2"}, j, &recordingOutbound{}, func(PrimaryProgress) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if _, err := c.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	target, err := c.CaptureRecoveryTarget(context.Background(), StoreCursor{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitRecoveryTarget(context.Background(), target) }()
	select {
	case err := <-done:
		t.Fatal("wait did not start blocked", err)
	case <-time.After(10 * time.Millisecond):
	}
	j.mu.Lock()
	durability.fail = true
	j.mu.Unlock()
	if _, err := c.RecordDurable(Frame{Kind: KindData, Store: StoreAudit, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: []byte("native audit")}); err == nil {
		t.Fatal("uncertain append passed")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("uncertain append confirmed recovery")
		}
	case <-time.After(time.Second):
		t.Fatal("uncertain ordering append stranded recovery waiter")
	}
	if confirmed, available, _ := c.Status(); confirmed != 0 || available {
		t.Fatal(confirmed, available)
	}
}
