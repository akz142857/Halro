package replication

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
)

// This adapter selects the old shared-cursor behavior without changing the
// source decoder, wire work, confirmation logic or ACK schedule.
type sharedRangeSource struct{ DurableSource }

type discardWindowOutbound struct{}

func (discardWindowOutbound) QueueFrame(uint64, []byte) error        { return nil }
func (discardWindowOutbound) QueueCommitNotice(uint64, []byte) error { return nil }
func (discardWindowOutbound) Acknowledge(string, uint64, uint64)     {}

func auditWindowHistory(t testing.TB, count int) (NativeSourceOptions, *OrderingJournal) {
	t.Helper()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x31}, 32)
	options := NativeSourceOptions{LedgerPath: filepath.Join(root, "ledger.log"), AuditPath: filepath.Join(root, "audit.log"), GovernancePath: filepath.Join(root, "governance.log"), MetadataPath: filepath.Join(root, "metadata.log"), LedgerKey: key, AuditKey: key, GovernanceKey: key, MetadataKey: key}
	journal, err := OpenOrderingJournal(filepath.Join(root, "ordering.journal"), key, OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	appendFrame := func(frame Frame) error {
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			return err
		}
		_, err = journal.Append(OrderingRecord{Kind: frame.Kind, Store: frame.Store, Index: frame.Index, Term: frame.Term, StoreGeneration: frame.StoreGeneration, StoreSequenceFirst: frame.StoreSequenceFirst, StoreSequenceLast: frame.StoreSequenceLast, FrameDigest: digest})
		return err
	}
	if err := appendFrame(testConfirmationFrame(1, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	log, err := audit.OpenWithOptions(options.AuditPath, key, audit.Options{AfterDurable: func(batch audit.DurableBatch) error {
		frame := testConfirmationFrame(batch.LastSequence+1, KindData)
		frame.Store, frame.StoreGeneration = StoreAudit, 1
		frame.StoreSequenceFirst, frame.StoreSequenceLast, frame.Payload = batch.FirstSequence, batch.LastSequence, batch.Frames
		return appendFrame(frame)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for i := 0; i < count; i++ {
		if _, err := log.Append(context.Background(), audit.Event{EventID: fmt.Sprintf("evt_windows_%d", i), OccurredAt: time.Unix(123, 0).UTC(), ActorType: "system", Action: "native.windows", Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	return options, journal
}

func runNativeWindowWork(t testing.TB, options NativeSourceOptions, journal *OrderingJournal, isolated bool, slowStride int) {
	t.Helper()
	native, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	var source DurableSource = native
	if !isolated {
		source = sharedRangeSource{native}
	}
	c, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1", "halro-2"}, journal, discardWindowOutbound{}, func(PrimaryProgress) error { return nil }, source, DefaultPrimaryWindowLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if c.pending != nil {
		t.Fatal("source-backed benchmark retained payload map")
	}
	frames := [2]uint64{}
	for i, node := range []string{"halro-1", "halro-2"} {
		if err := c.PreparePeer(node, 0, 0, func(encoded []byte) error {
			if len(encoded) >= streamPrefixBytes && bytes.Equal(encoded[4:streamPrefixBytes], frameMagic[:]) {
				frames[i]++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	head, _, _ := journal.Head()
	ack := func(node string, index uint64) {
		if _, err := c.Acknowledge(testAcknowledgement(node, index)); err != nil {
			t.Fatal(err)
		}
	}
	slow := uint64(0)
	for index := uint64(1); index <= head; index++ {
		ack("halro-1", index)
		if index%uint64(slowStride) == 0 {
			slow++
			ack("halro-2", slow)
		}
	}
	for slow < head {
		slow++
		ack("halro-2", slow)
	}
	for _, node := range []string{"halro-1", "halro-2"} {
		acknowledged := testAcknowledgement(node, head)
		acknowledged.AppliedIndex = head
		if _, err := c.Acknowledge(acknowledged); err != nil {
			t.Fatal(err)
		}
	}
	if frames != [2]uint64{head, head} || c.confirmed != head || len(c.PendingCommitNotice()) != 0 {
		t.Fatal("work did not finish identical native data/confirmation", frames, head)
	}
	if isolated && nativeReaderCount(native) != 2 {
		t.Fatal("production work did not use two independent reader slots")
	}
}

func TestNativeSourceWindowsCompleteInterleavedAndLaggingPeerWork(t *testing.T) {
	options, journal := auditWindowHistory(t, 64)
	for _, isolated := range []bool{false, true} {
		for _, stride := range []int{1, 4} {
			t.Run(fmt.Sprintf("isolated-%t/stride-%d", isolated, stride), func(t *testing.T) {
				runNativeWindowWork(t, options, journal, isolated, stride)
			})
		}
	}
}

// Actual authenticated Audit files and ordering digests, with equal constructor,
// frame/notice and ACK work. Fixture fsync is outside the measured interval;
// the sender counts without retaining frames or doing network IO. B/op is total
// allocation, not retained heap, RSS, shared budget or production capacity.
func BenchmarkNativeSourceInterleavedWindows(b *testing.B) {
	for _, count := range []int{100, 1000} {
		options, journal := auditWindowHistory(b, count)
		for _, isolated := range []bool{false, true} {
			for _, stride := range []int{1, 4} {
				b.Run(fmt.Sprintf("count-%d/isolated-%t/stride-%d", count, isolated, stride), func(b *testing.B) {
					b.ReportAllocs()
					b.ReportMetric(float64(2*(count+1)), "data-frames/op")
					for n := 0; n < b.N; n++ {
						runNativeWindowWork(b, options, journal, isolated, stride)
					}
				})
			}
		}
	}
}
