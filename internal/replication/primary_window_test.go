package replication

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

type windowSender struct {
	frames, notices []uint64
	err             error
}

func (s *windowSender) send(encoded []byte) error {
	if s.err != nil {
		return s.err
	}
	if frame, err := UnmarshalFrame(encoded); err == nil {
		s.frames = append(s.frames, frame.Index)
		return nil
	}
	notice, err := UnmarshalCommitNotice(encoded)
	if err != nil {
		return err
	}
	s.notices = append(s.notices, notice.ConfirmedIndex)
	return nil
}

func windowHistory(t *testing.T, count int, payload []byte) (*OrderingJournal, *fakeDurableSource) {
	t.Helper()
	journal, err := OpenOrderingJournal(filepath.Join(t.TempDir(), "ordering.journal"), []byte("0123456789abcdef0123456789abcdef"), OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	source := &fakeDurableSource{payloads: map[string][]byte{}}
	for i := uint64(1); i <= uint64(count); i++ {
		frame := testConfirmationFrame(i, KindLeadershipEstablished)
		if i > 1 {
			frame.Kind = KindData
			frame.Store = StoreLedger
			frame.StoreGeneration = 1
			frame.StoreSequenceFirst = i - 1
			frame.StoreSequenceLast = i - 1
			frame.Payload = payload
			source.payloads[sourceKey(StoreLedger, 1, i-1, i-1)] = payload
		}
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Append(OrderingRecord{Kind: frame.Kind, Store: frame.Store, Index: i, Term: frame.Term, StoreGeneration: frame.StoreGeneration, StoreSequenceFirst: frame.StoreSequenceFirst, StoreSequenceLast: frame.StoreSequenceLast, FrameDigest: digest}); err != nil {
			t.Fatal(err)
		}
	}
	return journal, source
}

func windowPrimary(t *testing.T, journal *OrderingJournal, source DurableSource, confirmed uint64, limits PrimaryWindowLimits, persist PrimaryStateWriter) *PrimaryCoordinator {
	t.Helper()
	if persist == nil {
		persist = func(PrimaryProgress) error { return nil }
	}
	c, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, confirmed, []string{"halro-1", "halro-2"}, journal, &recordingOutbound{}, persist, source, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if c.pending != nil {
		t.Fatal("source coordinator retained a payload map")
	}
	return c
}

func assertPeerBound(t *testing.T, c *PrimaryCoordinator, node string) {
	t.Helper()
	peer := c.windowFor(node)
	if peer.queuedThrough-peer.durable > uint64(c.windowLimits.MaxFrames) || peer.bytes < 0 || peer.bytes > c.windowLimits.MaxBytes {
		t.Fatal("peer window exceeded bounds", *peer)
	}
	sum := 0
	for _, n := range peer.sizes {
		sum += n
	}
	if sum != peer.bytes {
		t.Fatal("peer byte balance differs", sum, peer.bytes)
	}
}

func TestPrimarySourceWindowsRecoverThousandFramesAndKeepLaggingPeerIndependent(t *testing.T) {
	journal, source := windowHistory(t, 1000, []byte("source-native-frame"))
	c := windowPrimary(t, journal, source, 0, DefaultPrimaryWindowLimits(), nil)
	fast, slow := &windowSender{}, &windowSender{}
	if err := c.PreparePeer("halro-1", 0, 0, fast.send); err != nil {
		t.Fatal(err)
	}
	if err := c.PreparePeer("halro-2", 0, 0, slow.send); err != nil {
		t.Fatal(err)
	}
	if len(fast.frames) != 16 || len(slow.frames) != 16 {
		t.Fatal("initial batch was not finite", len(fast.frames), len(slow.frames))
	}
	for i := uint64(1); i <= 1000; i++ {
		if fast.frames[i-1] != i {
			t.Fatal("send sequence skipped", i)
		}
		ack := testAcknowledgement("halro-1", i)
		ack.AppliedIndex = i
		if got, err := c.Acknowledge(ack); err != nil || got != i {
			t.Fatal(i, got, err)
		}
		assertPeerBound(t, c, "halro-1")
		assertPeerBound(t, c, "halro-2")
	}
	if len(slow.frames) != 16 || c.windowFor("halro-1").bytes != 0 {
		t.Fatal("fast quorum changed lagger window or retained bytes")
	}
	for i := uint64(1); i <= 1000; i++ {
		ack := testAcknowledgement("halro-2", i)
		ack.AppliedIndex = i
		if got, err := c.Acknowledge(ack); err != nil || got != 1000 {
			t.Fatal(i, got, err)
		}
		assertPeerBound(t, c, "halro-2")
	}
	if len(slow.frames) != 1000 || len(c.PendingCommitNotice()) != 0 {
		t.Fatal("confirmed lagger history did not finish")
	}
	commit, err := c.RecordSchemaBoundary(1, 2)
	if err != nil || commit.Index != 1001 || !commit.ReplicationAvailable {
		t.Fatal(commit, err)
	}
	if fast.frames[len(fast.frames)-1] != 1001 || slow.frames[len(slow.frames)-1] != 1001 {
		t.Fatal("live append did not use peer windows")
	}
}

func TestPrimarySourceWindowsByteLimitDuplicateAndFutureACK(t *testing.T) {
	journal, source := windowHistory(t, 5, bytes.Repeat([]byte{1}, 8<<20))
	limits := PrimaryWindowLimits{MaxFrames: 16, MaxBytes: maxEncodedPrimaryFrame}
	c := windowPrimary(t, journal, source, 0, limits, nil)
	sender := &windowSender{}
	if err := c.PreparePeer("halro-1", 0, 0, sender.send); err != nil {
		t.Fatal(err)
	}
	if len(sender.frames) != 3 {
		t.Fatal("byte limit did not stop before third large data frame", sender.frames)
	}
	assertPeerBound(t, c, "halro-1")
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", 4)); err == nil || c.confirmed != 0 {
		t.Fatal("ACK exceeded queued peer prefix", err)
	}
	ack := testAcknowledgement("halro-1", 1)
	if _, err := c.Acknowledge(ack); err != nil {
		t.Fatal(err)
	}
	bytesBefore := c.windowFor("halro-1").bytes
	if _, err := c.Acknowledge(ack); err != nil || bytesBefore != c.windowFor("halro-1").bytes {
		t.Fatal("duplicate ACK double released bytes", err)
	}
	if _, err := c.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil {
		t.Fatal(err)
	}
	if len(sender.frames) != 4 {
		t.Fatal("byte release did not admit next frame", sender.frames)
	}
	page, err := c.PendingFrom(0)
	if err != nil || len(page) != 2 {
		t.Fatal("pending page exceeded byte bound", len(page), err)
	}
}

func TestPrimarySourceWindowsQueuesBoundedNoticesAndStopsNoticeACKLoops(t *testing.T) {
	journal, source := windowHistory(t, 200, []byte("source"))
	c := windowPrimary(t, journal, source, 200, DefaultPrimaryWindowLimits(), nil)
	sender := &windowSender{}
	if err := c.PreparePeer("halro-1", 0, 0, sender.send); err != nil {
		t.Fatal(err)
	}
	if len(sender.notices) != 1 || sender.notices[0] != 16 {
		t.Fatal("notice exceeded queued prefix", sender.notices)
	}
	for i := uint64(1); i <= 200; i++ {
		ack := testAcknowledgement("halro-1", i)
		ack.AppliedIndex = i
		if _, err := c.Acknowledge(ack); err != nil {
			t.Fatal(err)
		}
	}
	notices := len(sender.notices)
	ack := testAcknowledgement("halro-1", 200)
	ack.AppliedIndex = 200
	for i := 0; i < 20; i++ {
		if _, err := c.Acknowledge(ack); err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.notices) != notices {
		t.Fatal("quiet notice created an ACK loop")
	}
	for _, through := range sender.notices {
		if through > 200 {
			t.Fatal(through)
		}
	}
}

func TestPrimarySourceWindowsFailClosedOnSourceAndPersistenceButKeepHealthyQuorum(t *testing.T) {
	for _, stage := range []string{"queue", "source", "persist"} {
		t.Run(stage, func(t *testing.T) {
			journal, source := windowHistory(t, 40, []byte("source"))
			failed := false
			c := windowPrimary(t, journal, source, 0, DefaultPrimaryWindowLimits(), func(progress PrimaryProgress) error {
				if failed && stage == "persist" {
					return errors.New("state fsync failed")
				}
				return nil
			})
			first, second := &windowSender{}, &windowSender{}
			if err := c.PreparePeer("halro-1", 0, 0, first.send); err != nil {
				t.Fatal(err)
			}
			if err := c.PreparePeer("halro-2", 0, 0, second.send); err != nil {
				t.Fatal(err)
			}
			failed = true
			if stage == "source" {
				source.payloads[sourceKey(StoreLedger, 1, 16, 16)] = []byte("changed")
			}
			if stage == "queue" {
				first.err = ErrPeerSessionClosed
			}
			_, err := c.Acknowledge(testAcknowledgement("halro-1", 1))
			if err == nil {
				t.Fatal("failure was hidden")
			}
			_, available, _ := c.Status()
			if (stage == "queue") != available {
				t.Fatal("queue/source failure quorum policy incorrect", available, err)
			}
			if stage == "persist" && c.confirmed != 0 {
				t.Fatal("published uncertain confirmation")
			}
			if stage != "queue" && c.poisoned == nil {
				t.Fatal("source/fsync failure did not poison progress")
			}
		})
	}
}

func TestPrimarySourceWindowsVerifyCompleteSuffixBeforePublicationAndRejectPriorTermCatchup(t *testing.T) {
	journal, source := windowHistory(t, 100, []byte("source"))
	source.payloads[sourceKey(StoreLedger, 1, 99, 99)] = []byte("bad-tail")
	published := 0
	if _, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { published++; return nil }, source, DefaultPrimaryWindowLimits()); err == nil || published != 0 {
		t.Fatal("late corruption leaked publication", err, published)
	}
	source.payloads[sourceKey(StoreLedger, 1, 99, 99)] = []byte("source")
	// A future term with a fully confirmed prior prefix may establish a new
	// anchor. A peer missing that prior term cannot replay it through Receive.
	c, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 8, 100, []string{"halro-1"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { return nil }, source, DefaultPrimaryWindowLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if _, err := c.RecordDurable(Frame{Kind: KindLeadershipEstablished}); err != nil {
		t.Fatal(err)
	}
	sender := &windowSender{}
	if err := c.PreparePeer("halro-1", 0, 0, sender.send); !errors.Is(err, ErrMemberRequiresFullReseed) || len(sender.frames) != 0 {
		t.Fatal("prior-term history enqueued", err)
	}
	if err := c.PreparePeer("halro-1", 100, 100, sender.send); err != nil || len(sender.frames) != 1 || sender.frames[0] != 101 {
		t.Fatal(err, sender.frames)
	}
}

func TestPrimarySourceWindowsBoundPendingPagesAndStopLifecycle(t *testing.T) {
	journal, source := windowHistory(t, 100, []byte("source"))
	c := windowPrimary(t, journal, source, 0, DefaultPrimaryWindowLimits(), nil)
	for after := uint64(0); after < 100; {
		page, err := c.PendingFrom(after)
		if err != nil || len(page) == 0 || len(page) > 16 {
			t.Fatal(err, len(page))
		}
		after = page[len(page)-1].Index
	}
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sender := &windowSender{}
			for i := 0; i < 10; i++ {
				_ = c.PreparePeer("halro-1", 0, 0, sender.send)
				_ = c.RetryPending()
			}
		}()
	}
	c.Stop()
	wg.Wait()
	c.MarkAvailable()
	for _, peer := range c.peerWindows {
		if peer.send != nil || peer.bytes != 0 {
			t.Fatal("Stop retained session/window")
		}
	}
	if _, err := c.RecordDurable(Frame{Kind: KindSchemaBoundary, Metadata: []byte{1}}); !errors.Is(err, ErrReplicationUnavailable) {
		t.Fatal(err)
	}
	if page, err := c.PendingFrom(0); !errors.Is(err, ErrReplicationUnavailable) || len(page) != 0 {
		t.Fatal("stopped coordinator reopened native reads", err)
	}
}

func TestPrimaryWindowReservesInitialCommitNoticeInSessionQueue(t *testing.T) {
	limits := DefaultPrimaryWindowLimits()
	for _, options := range []PeerSessionOptions{
		{},
		{QueueCapacity: limits.MaxFrames + 1, MaxQueuedBytes: int64(limits.MaxBytes) + maxEncodedCommitNotice},
	} {
		if err := limits.ValidateSession(options); err != nil {
			t.Fatal(err)
		}
	}
	for _, options := range []PeerSessionOptions{
		{QueueCapacity: limits.MaxFrames},
		{MaxQueuedBytes: int64(limits.MaxBytes) + maxEncodedCommitNotice - 1},
		{QueueCapacity: -1},
	} {
		if err := limits.ValidateSession(options); err == nil {
			t.Fatal("window without control reserve accepted", options)
		}
	}
}
