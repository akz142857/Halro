package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replicationlimits"
)

func TestPeerSessionAuthenticatesAndExchangesTypedRecords(t *testing.T) {
	budget, queue := queueBudgetFixture(t, 3, 4096)
	options := PeerSessionOptions{QueueCapacity: 2, MaxQueuedBytes: 1024, QueueBudget: queue}
	files, leaf := writeTestCertificateChain(t)
	serverTLS, err := LoadServerTLSConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := LoadClientTLSConfig(files, "halro-1.internal")
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	key := []byte("0123456789abcdef0123456789abcdef")
	primaryHello := testHello("halro-0", RolePrimary, 0)
	replicaHello := testHello("halro-1", RoleReplica, 0)
	clientSide, serverSide := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		session *PeerSession
		err     error
	}
	serverResult := make(chan result, 1)
	go func() {
		session, err := AcceptPeerSession(ctx, serverSide, serverTLS, replicaHello, key,
			map[string]string{"halro-0": digestText(pin)}, NewNonceCache(16), options)
		serverResult <- result{session, err}
	}()
	client, err := DialPeerSession(ctx, clientSide, clientTLS, primaryHello, key,
		"halro-1", digestText(pin), NewNonceCache(16), options)
	if err != nil {
		t.Fatal(err)
	}
	server := <-serverResult
	if server.err != nil {
		t.Fatal(server.err)
	}
	defer client.Close()
	defer server.session.Close()

	frame, err := (Frame{
		Kind: KindLeadershipEstablished, Index: 1, Term: 1,
		ClusterID: "production-a", Incarnation: "inc_01",
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	ack, err := (Acknowledgement{
		ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-1",
		Index: 1, Term: 1, DurableIndex: 1,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	clientReceived := make(chan StreamRecord, 1)
	serverReceived := make(chan StreamRecord, 1)
	runs := make(chan error, 2)
	go func() {
		runs <- client.Run(ctx, func(_ context.Context, _ Hello, record StreamRecord) error {
			clientReceived <- record
			return nil
		})
	}()
	go func() {
		runs <- server.session.Run(ctx, func(_ context.Context, _ Hello, record StreamRecord) error {
			serverReceived <- record
			return server.session.Send(ack)
		})
	}()
	if err := client.Send(frame); err != nil {
		t.Fatal(err)
	}
	select {
	case record := <-serverReceived:
		if record.Kind != StreamRecordFrame || !bytes.Equal(record.Encoded, frame) {
			t.Fatalf("server record=%#v", record)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case record := <-clientReceived:
		if record.Kind != StreamRecordAcknowledgement || !bytes.Equal(record.Encoded, ack) {
			t.Fatalf("client record=%#v", record)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if stats := budget.Snapshot(); stats.RecoveryClaims != 2 {
		t.Fatal("writing/reflected ACK refunded session capacity", stats)
	}
	cancel()
	for range 2 {
		if err := <-runs; !errors.Is(err, context.Canceled) {
			t.Fatalf("session shutdown error=%v", err)
		}
	}
	if stats := budget.Snapshot(); stats.Used.RAM != 0 || len(queue.slots) != 0 {
		t.Fatal("session drain leaked queue fee", stats)
	}
}

func TestPrimaryConcurrentRecoveredRetransmissionOverTLSRetiresAdmissionOnce(t *testing.T) {
	files, leaf := writeTestCertificateChain(t)
	serverTLS, err := LoadServerTLSConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := LoadClientTLSConfig(files, "halro-1.internal")
	if err != nil {
		t.Fatal(err)
	}
	pin := digestText(sha256.Sum256(leaf.RawSubjectPublicKeyInfo))
	key := []byte("0123456789abcdef0123456789abcdef")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type pair struct {
		primary *PeerSession
		replica *PeerSession
		peer    string
	}
	links := make([]pair, 0, 2)
	for i, peer := range []string{"halro-1", "halro-2"} {
		clientSide, serverSide := net.Pipe()
		serverResult := make(chan struct {
			session *PeerSession
			err     error
		}, 1)
		go func(peer string, raw net.Conn) {
			session, err := AcceptPeerSession(ctx, raw, serverTLS, testHello(peer, RoleReplica, byte(i+2)), key,
				map[string]string{"halro-0": pin}, NewNonceCache(16), PeerSessionOptions{QueueCapacity: 4, MaxQueuedBytes: 1 << 20})
			serverResult <- struct {
				session *PeerSession
				err     error
			}{session, err}
		}(peer, serverSide)
		primary, err := DialPeerSession(ctx, clientSide, clientTLS, testHello("halro-0", RolePrimary, byte(i+1)), key,
			peer, pin, NewNonceCache(16), PeerSessionOptions{QueueCapacity: 4, MaxQueuedBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		accepted := <-serverResult
		if accepted.err != nil {
			t.Fatal(accepted.err)
		}
		links = append(links, pair{primary: primary, replica: accepted.session, peer: peer})
	}

	journal, source := windowHistory(t, 2, []byte("recovered"))
	budget := unconfirmedManager(t, 1<<20, 1<<20, 4)
	limits := DefaultPrimaryWindowLimits()
	limits.UnconfirmedBudget = budget
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
	defer func() {
		coordinator.Stop()
		if err := coordinator.ReleaseUnconfirmedForShutdown(); err != nil {
			t.Error("release Primary admission at test shutdown:", err)
		}
	}()
	if got := budget.Snapshot().Used.Unconfirmed; got == 0 {
		t.Fatal("recovered suffix was not charged before transport preparation")
	}
	for _, link := range links {
		if err := coordinator.PreparePeer(link.peer, 1, 1, link.primary.Send); err != nil {
			t.Fatal(err)
		}
	}

	runResults := make(chan error, 4)
	acks := make(chan error, 2)
	frames := make(chan string, 2)
	for _, link := range links {
		link := link
		go func() {
			runResults <- link.primary.Run(ctx, func(_ context.Context, _ Hello, record StreamRecord) error {
				ack, err := UnmarshalAcknowledgement(record.Encoded)
				if err != nil {
					return err
				}
				_, err = coordinator.Acknowledge(ack)
				acks <- err
				return err
			})
		}()
		go func() {
			runResults <- link.replica.Run(ctx, func(_ context.Context, _ Hello, record StreamRecord) error {
				if record.Kind == StreamRecordCommitNotice {
					return nil
				}
				frame, err := UnmarshalFrame(record.Encoded)
				if err != nil {
					return err
				}
				if frame.Index != 2 {
					return errors.New("TLS peer received an unexpected recovered frame")
				}
				frames <- link.peer
				ack, err := testAcknowledgement(link.peer, frame.Index).MarshalBinary()
				if err != nil {
					return err
				}
				return link.replica.Send(ack)
			})
		}()
	}

	seen := map[string]bool{}
	for range 2 {
		select {
		case peer := <-frames:
			seen[peer] = true
		case <-ctx.Done():
			t.Fatal("both authenticated peers did not receive the recovered frame:", ctx.Err())
		}
	}
	if !seen["halro-1"] || !seen["halro-2"] {
		t.Fatalf("recovered suffix was not independently retransmitted to both TLS peers: %v", seen)
	}
	for range 2 {
		select {
		case err := <-acks:
			if err != nil {
				t.Fatal("TLS peer ACK failed:", err)
			}
		case <-ctx.Done():
			t.Fatal("both authenticated peer ACKs did not arrive:", ctx.Err())
		}
	}
	if stateWrites.Load() != 2 || len(coordinator.unconfirmedEntries) != 0 || budget.Snapshot().Used.Unconfirmed != 0 {
		t.Fatalf("TLS cumulative ACKs did not publish/refund exactly once: writes=%d entries=%v budget=%+v", stateWrites.Load(), coordinator.unconfirmedEntries, budget.Snapshot())
	}
	cancel()
	for range 4 {
		<-runResults
	}
	for _, link := range links {
		_ = link.primary.Close()
		_ = link.replica.Close()
	}
}

func TestPeerSessionBoundsHandshakeAndWriterQueue(t *testing.T) {
	files, _ := writeTestCertificateChain(t)
	serverTLS, err := LoadServerTLSConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	start := time.Now()
	_, err = AcceptPeerSession(context.Background(), serverSide, serverTLS, testHello("halro-1", RoleReplica, 0),
		[]byte("0123456789abcdef0123456789abcdef"), map[string]string{}, NewNonceCache(1),
		PeerSessionOptions{HandshakeTimeout: 20 * time.Millisecond, QueueCapacity: 1})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("silent handshake error=%v duration=%s", err, time.Since(start))
	}

	local := testHello("halro-0", RolePrimary, 1)
	peer := testHello("halro-1", RoleReplica, 2)
	left, right := net.Pipe()
	session, err := newPeerSession(tls.Client(left, &tls.Config{}), local, peer, PeerSessionOptions{QueueCapacity: 1, WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	defer right.Close()
	frame, err := (Frame{Kind: KindLeadershipEstablished, Index: 1, Term: 1, ClusterID: "production-a", Incarnation: "inc_01"}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Send(frame); err != nil {
		t.Fatal(err)
	}
	if err := session.Send(frame); !errors.Is(err, ErrPeerSessionQueueFull) {
		t.Fatalf("full queue error=%v", err)
	}
	if err := session.Send([]byte("invalid")); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("invalid queued record error=%v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Send(frame); !errors.Is(err, ErrPeerSessionClosed) {
		t.Fatalf("send after close error=%v", err)
	}
}

func TestPeerSessionBoundsQueuedBytesAndKeepsSameRoleHelloControlOnly(t *testing.T) {
	local := testHello("halro-0", RolePrimary, 1)
	peer := testHello("halro-1", RoleReplica, 2)
	frame, err := (Frame{Kind: KindLeadershipEstablished, Index: 1, Term: 1, ClusterID: "production-a", Incarnation: "inc_01"}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	session, err := newPeerSession(tls.Client(left, &tls.Config{}), local, peer, PeerSessionOptions{
		QueueCapacity: 2, MaxQueuedBytes: int64(len(frame)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	defer right.Close()
	if err := session.Send(frame); err != nil {
		t.Fatal(err)
	}
	if err := session.Send(frame); !errors.Is(err, ErrPeerSessionQueueFull) {
		t.Fatalf("byte-full queue error=%v", err)
	}

	controlLeft, controlRight := net.Pipe()
	control, err := newPeerSession(tls.Client(controlLeft, &tls.Config{}), local, testHello("halro-1", RolePrimary, 3), PeerSessionOptions{})
	if err != nil {
		t.Fatalf("authenticated same-role hello was discarded before adjudication: %v", err)
	}
	defer control.Close()
	defer controlRight.Close()
	if control.DataAuthorized() {
		t.Fatal("same-role hello incorrectly authorized a data stream")
	}
	if err := control.Send(frame); err == nil || !strings.Contains(err.Error(), "control-only") {
		t.Fatalf("control-only send error=%v", err)
	}
}
