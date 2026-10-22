package replication

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type streamTestFrames struct {
	mu     sync.Mutex
	head   uint64
	frames map[uint64][]byte
}

func (f *streamTestFrames) OrderingHead() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head
}

func (f *streamTestFrames) EncodedFrame(index uint64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	encoded, ok := f.frames[index]
	if !ok {
		return nil, errors.New("no such frame")
	}
	return encoded, nil
}

func (f *streamTestFrames) commit(t *testing.T, index uint64) []byte {
	t.Helper()
	encoded, err := (Frame{Kind: KindLeadershipEstablished, Index: index, Term: 1, ClusterID: "production-a", Incarnation: "inc_01"}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames[index] = encoded
	f.head = index
	return encoded
}

func streamTestNotice(t *testing.T, confirmedIndex uint64) []byte {
	t.Helper()
	encoded, err := (CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 1, ConfirmedIndex: confirmedIndex}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// newStreamTestSession is a Primary session to a Replica whose writer does not
// run: the test takes records off the queue itself, in the order the writer
// would put them on the wire.
func newStreamTestSession(t *testing.T, capacity int) *PeerSession {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() { right.Close() })
	session, err := newPeerSession(tls.Client(left, &tls.Config{}), testHello("halro-0", RolePrimary, 1),
		testHello("halro-1", RoleReplica, 2), PeerSessionOptions{QueueCapacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// takeQueued removes one record as the writer would and returns its index.
func takeQueued(t *testing.T, session *PeerSession) uint64 {
	t.Helper()
	select {
	case record := <-session.queue:
		session.queued.Add(-int64(len(record.encoded)))
		select {
		case session.drained <- struct{}{}:
		default:
		}
		frame, _, err := UnmarshalFrameAndDigest(record.encoded)
		if err != nil {
			t.Fatal(err)
		}
		return frame.Index
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was queued")
		return 0
	}
}

func TestPeerStreamKeepsLiveFramesBehindTheReplicasDurableIndex(t *testing.T) {
	session := newStreamTestSession(t, 2)
	frames := &streamTestFrames{frames: map[uint64][]byte{}}
	first, second, third := frames.commit(t, 1), frames.commit(t, 2), frames.commit(t, 3)
	stream := newPeerStream(session, frames, 0)

	if err := stream.offerFrame(2, second); !errors.Is(err, ErrPeerCatchingUp) {
		t.Fatalf("frame ahead of the Replica error=%v", err)
	}
	if err := stream.offerCommitNotice(1, streamTestNotice(t, 1)); !errors.Is(err, ErrPeerCatchingUp) {
		t.Fatalf("notice for an index the session has not carried error=%v", err)
	}
	if err := stream.offerFrame(1, first); err != nil {
		t.Fatal(err)
	}
	if err := stream.offerFrame(1, first); err != nil {
		t.Fatalf("a frame this session already carries must count as queued: %v", err)
	}
	if err := stream.offerFrame(2, second); err != nil {
		t.Fatal(err)
	}
	err := stream.offerFrame(3, third)
	if !errors.Is(err, ErrPeerCatchingUp) || !errors.Is(err, ErrPeerSessionQueueFull) {
		t.Fatalf("live frame meeting a full queue error=%v", err)
	}
	if got := []uint64{takeQueued(t, session), takeQueued(t, session)}; got[0] != 1 || got[1] != 2 {
		t.Fatalf("queued indexes=%v, want [1 2]", got)
	}
	if len(session.queue) != 0 {
		t.Fatalf("a refused or repeated frame was queued: %d left", len(session.queue))
	}
}

// A Replica far behind is carried through a queue much smaller than the gap,
// and live commits made meanwhile neither jump ahead nor go missing.
func TestPeerStreamCatchesUpThroughASmallQueueWithoutReordering(t *testing.T) {
	session := newStreamTestSession(t, 2)
	frames := &streamTestFrames{frames: map[uint64][]byte{}}
	for index := uint64(1); index <= 40; index++ {
		frames.commit(t, index)
	}
	stream := newPeerStream(session, frames, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- stream.run(ctx) }()

	var got []uint64
	for index := uint64(41); index <= 60; index++ {
		got = append(got, takeQueued(t, session))
		// A live commit, offered as the coordinator would. While the
		// stream is behind it is refused and carried by catch-up instead.
		encoded := frames.commit(t, index)
		if err := stream.offerFrame(index, encoded); err != nil && !errors.Is(err, ErrPeerCatchingUp) {
			t.Fatal(err)
		}
	}
	for len(got) < 57 {
		got = append(got, takeQueued(t, session))
	}
	for position, index := range got {
		if index != uint64(position)+4 {
			t.Fatalf("session carried %v, want 4..60 in order", got)
		}
	}
	// Caught up: the next live commit is queued directly.
	if err := stream.offerFrame(61, frames.commit(t, 61)); err != nil {
		t.Fatalf("live commit after catch-up error=%v", err)
	}
	if index := takeQueued(t, session); index != 61 {
		t.Fatalf("live commit after catch-up queued index %d", index)
	}
	if err := stream.offerCommitNotice(61, streamTestNotice(t, 61)); err != nil || len(session.queue) != 1 {
		t.Fatalf("notice for a carried index error=%v queued=%d", err, len(session.queue))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
