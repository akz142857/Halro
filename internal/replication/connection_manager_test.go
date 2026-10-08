package replication

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replicationlimits"
)

// Exercise the actual manager callback wiring without a socket handshake:
// preparation fails deliberately before Run, while replacement and another
// peer's ACK run at precisely controlled generation boundaries.
func TestConnectionManagerPreparationAndCleanupRespectReplacementGeneration(t *testing.T) {
	journal, source := windowHistory(t, 50, []byte("source"))
	budget := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = budget
	limits.MaxUnconfirmedEntries = 64
	limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
	limits.RecoveryReserveUnconfirmedBytes = 1 << 20
	c := windowPrimary(t, journal, source, 0, limits, nil)
	defer func() {
		c.Stop()
		if err := c.ReleaseUnconfirmedForShutdown(); err != nil {
			t.Error("release Primary admission at test shutdown:", err)
		}
	}()
	initialAdmission := budget.Snapshot().Used.Unconfirmed
	if initialAdmission == 0 || len(c.unconfirmedEntries) != 50 {
		t.Fatalf("the shared budget must own the recovered suffix before sessions attach: bytes=%d entries=%d", initialAdmission, len(c.unconfirmedEntries))
	}
	other := &windowSender{}
	if err := c.PreparePeer("halro-2", 0, 0, other.send); err != nil {
		t.Fatal(err)
	}
	registry, err := NewSessionRegistry("halro-0", []string{"halro-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	sessionManager, _ := queueBudgetFixture(t, 2, replicationlimits.MinQueueRAMBytes+replicationlimits.MinReadRAMBytes)
	sessionBudget, err := NewPeerSessionBudget(sessionManager, replicationlimits.MinQueueRAMBytes, replicationlimits.MinReadRAMBytes, 2)
	if err != nil {
		t.Fatal(err)
	}
	makeSession := func(nonce byte) *PeerSession {
		left, right := net.Pipe()
		t.Cleanup(func() { _ = right.Close() })
		hello := testHello("halro-1", RoleReplica, nonce)
		hello.DurableIndex, hello.AppliedIndex = 0, 0
		session, err := newPeerSession(tls.Client(left, &tls.Config{}), testHello("halro-0", RolePrimary, 1), hello, PeerSessionOptions{QueueBudget: sessionBudget})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	old, replacement := makeSession(2), makeSession(3)
	oldPrepared, releaseOld := make(chan struct{}), make(chan struct{})
	newRegistered, allowNewPrepare := make(chan struct{}), make(chan struct{})
	newPrepared, releaseNew := make(chan struct{}), make(chan struct{})
	// Always unblock callbacks on a failed assertion, before registry cleanup.
	t.Cleanup(func() {
		for _, ch := range []chan struct{}{releaseOld, allowNewPrepare, releaseNew} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})
	prepareFailed := errors.New("controlled preparation exit")
	var closed atomic.Uint64
	manager := &ConnectionManager{registry: registry, options: ConnectionManagerOptions{
		OnDataSessionReady: func(peer string, hello Hello, send func([]byte) error) error {
			if hello.Nonce == replacement.Peer().Nonce {
				close(newRegistered)
				<-allowNewPrepare
			}
			if err := c.PreparePeer(peer, hello.DurableIndex, hello.AppliedIndex, send); err != nil {
				return err
			}
			if hello.Nonce == old.Peer().Nonce {
				close(oldPrepared)
				<-releaseOld
			} else {
				close(newPrepared)
				<-releaseNew
			}
			return prepareFailed
		},
		OnDataSessionClosed: func(peer string) { closed.Add(1); c.ForgetPeer(peer) },
	}}
	oldDone, newDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := manager.runRegistered(context.Background(), "halro-1", true, old); oldDone <- err }()
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("controlled session phase did not complete")
		}
	}
	wait(oldPrepared)
	go func() {
		_, err := manager.runRegistered(context.Background(), "halro-1", true, replacement)
		newDone <- err
	}()
	// Close proves Register has retired the old generation and reached its
	// lease drain. The new Ready callback cannot run until that lease exits.
	wait(old.closedSignal)
	assertQueueFee(t, sessionManager, sessionBudget, 2)
	select {
	case <-newRegistered:
		t.Fatal("replacement preparation overtook the old delivery lease")
	default:
	}
	close(releaseOld)
	wait(newRegistered)
	if err := <-oldDone; !errors.Is(err, prepareFailed) || closed.Load() != 0 {
		t.Fatal("old cleanup called current-generation closure", err, closed.Load())
	}
	assertQueueFee(t, sessionManager, sessionBudget, 1)
	// The registry now points to the replacement, but its Prepare is held.
	// An ACK on peer 2 may pump peer 1's old window; that send must hit the
	// closed old session, never the new session's queue.
	if _, err := c.Acknowledge(testAcknowledgement("halro-2", 1)); err != nil {
		t.Fatal(err)
	}
	remainingAdmission := budget.Snapshot().Used.Unconfirmed
	if remainingAdmission <= 0 || remainingAdmission >= initialAdmission || len(c.unconfirmedEntries) != 49 {
		t.Fatalf("the other peer's ACK must retire only its confirmed prefix during replacement: before=%d after=%d entries=%d", initialAdmission, remainingAdmission, len(c.unconfirmedEntries))
	}
	if len(replacement.queue) != 0 || c.windowFor("halro-1").ready {
		t.Fatal("old window crossed into replacement before preparation")
	}
	close(allowNewPrepare)
	wait(newPrepared)
	if !c.windowFor("halro-1").ready || len(replacement.queue) != 17 || closed.Load() != 0 {
		t.Fatal("replacement did not retain its own finite data/notice batch", len(replacement.queue), closed.Load())
	}
	close(releaseNew)
	if err := <-newDone; !errors.Is(err, prepareFailed) || closed.Load() != 1 || c.windowFor("halro-1").ready {
		t.Fatal("current session cleanup did not invalidate its window", err, closed.Load())
	}
	if got := budget.Snapshot().Used.Unconfirmed; got != remainingAdmission {
		t.Fatalf("disconnecting the replacement released still-unconfirmed admissions: got=%d want=%d", got, remainingAdmission)
	}
	assertQueueFee(t, sessionManager, sessionBudget, 0)
}

func TestConnectionManagersDialAuthenticateExchangeAndShutdown(t *testing.T) {
	files, leaf := writeTestCertificateChain(t)
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	listenerA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenerB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		listenerA.Close()
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	acknowledged := make(chan Acknowledgement, 1)
	readyHello := make(chan Hello, 1)
	managerA, err := NewConnectionManager(ConnectionManagerOptions{
		LocalNode: "halro-0", TLS: files, ClusterKey: key,
		Peers:                []PeerEndpoint{{NodeID: "halro-1", Address: listenerB.Addr().String(), ServerName: "halro-1.internal", SPKISHA256: digestText(pin)}},
		LocalHello:           func() (Hello, error) { return testHello("halro-0", RolePrimary, 1), nil },
		OnAuthenticatedHello: func(context.Context, Hello) (bool, error) { return true, nil },
		OnDataSessionReady: func(peer string, hello Hello, send func([]byte) error) error {
			if peer != hello.NodeID {
				return errors.New("ready callback peer does not match authenticated Hello")
			}
			readyHello <- hello
			return nil
		},
		Handle: func(_ context.Context, _ Hello, record StreamRecord, _ func([]byte) error) error {
			ack, err := UnmarshalAcknowledgement(record.Encoded)
			if err == nil {
				acknowledged <- ack
			}
			return err
		},
		MinRetryBackoff: 5 * time.Millisecond, MaxRetryBackoff: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := NewConnectionManager(ConnectionManagerOptions{
		LocalNode: "halro-1", TLS: files, ClusterKey: key,
		Peers:                []PeerEndpoint{{NodeID: "halro-0", Address: listenerA.Addr().String(), ServerName: "halro-1.internal", SPKISHA256: digestText(pin)}},
		LocalHello:           func() (Hello, error) { return testHello("halro-1", RoleReplica, 2), nil },
		OnAuthenticatedHello: func(context.Context, Hello) (bool, error) { return true, nil },
		Handle: func(_ context.Context, _ Hello, record StreamRecord, send func([]byte) error) error {
			frame, err := UnmarshalFrame(record.Encoded)
			if err != nil {
				return err
			}
			encoded, err := (Acknowledgement{
				ClusterID: frame.ClusterID, Incarnation: frame.Incarnation, NodeID: "halro-1",
				Index: frame.Index, Term: frame.Term, DurableIndex: frame.Index,
			}).MarshalBinary()
			if err != nil {
				return err
			}
			return send(encoded)
		},
		MinRetryBackoff: 5 * time.Millisecond, MaxRetryBackoff: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	results := make(chan error, 2)
	go func() { results <- managerA.Serve(ctx, listenerA) }()
	go func() { results <- managerB.Serve(ctx, listenerB) }()
	frame, err := (Frame{
		Kind: KindLeadershipEstablished, Index: 1, Term: 7,
		ClusterID: "production-a", Incarnation: "inc_01",
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for {
		err = managerA.Send("halro-1", frame)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrPeerNotConnected) {
			t.Fatalf("send before connection error=%v", err)
		}
		select {
		case <-retry.C:
		case <-deadline.C:
			t.Fatal("connection managers did not establish a session")
		}
	}
	select {
	case hello := <-readyHello:
		if hello.NodeID != "halro-1" || hello.DurableIndex != 11 {
			t.Fatalf("ready hello=%#v", hello)
		}
	case <-deadline.C:
		t.Fatal("data-session callback did not receive authenticated Hello")
	}
	select {
	case ack := <-acknowledged:
		if ack.NodeID != "halro-1" || ack.DurableIndex != 1 {
			t.Fatalf("ack=%#v", ack)
		}
	case <-deadline.C:
		t.Fatal("replication frame was not acknowledged")
	}
	cancel()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("connection manager shutdown error=%v", err)
		}
	}
	if err := managerA.Send("halro-1", frame); !errors.Is(err, ErrPeerNotConnected) {
		t.Fatalf("send after shutdown error=%v", err)
	}
}

func TestConnectionManagerBoundsUnbudgetedAuthenticatedControlAndDataSessions(t *testing.T) {
	files, leaf := writeTestCertificateChain(t)
	pin := digestText(sha256.Sum256(leaf.RawSubjectPublicKeyInfo))
	manager, err := NewConnectionManager(ConnectionManagerOptions{LocalNode: "halro-0", TLS: files, ClusterKey: []byte("0123456789abcdef0123456789abcdef"), Peers: []PeerEndpoint{{NodeID: "halro-1", Address: "127.0.0.1:1", SPKISHA256: pin}, {NodeID: "halro-2", Address: "127.0.0.1:2", SPKISHA256: pin}}, LocalHello: func() (Hello, error) { return testHello("halro-0", RolePrimary, 1), nil }, OnAuthenticatedHello: func(context.Context, Hello) (bool, error) { return true, nil }, Handle: func(context.Context, Hello, StreamRecord, func([]byte) error) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	budget := manager.options.Session.QueueBudget
	if budget.manager != nil || cap(budget.slots) != 3 {
		t.Fatal("default manager omitted fixed session limit")
	}
	sessions := make([]*PeerSession, 0, 3)
	for _, role := range []Role{RoleReplica, RoleReplica, RolePrimary} {
		left, right := net.Pipe()
		t.Cleanup(func() { _ = right.Close() })
		s, err := newPeerSession(tls.Client(left, &tls.Config{}), testHello("halro-0", RolePrimary, 1), testHello("halro-1", role, 2), manager.options.Session)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
		t.Cleanup(func() { _ = s.Close() })
	}
	left, right := net.Pipe()
	_, err = newPeerSession(tls.Client(left, &tls.Config{}), testHello("halro-0", RolePrimary, 1), testHello("halro-1", RolePrimary, 3), manager.options.Session)
	_ = right.Close()
	if !errors.Is(err, ErrPeerQueueBudgetFull) || len(budget.slots) != 3 {
		t.Fatal("control-only sessions escaped fixed bound", err)
	}
	for _, s := range sessions {
		_ = s.Close()
	}
	if len(budget.slots) != 0 {
		t.Fatal("control/data retirement retained slots")
	}
}
