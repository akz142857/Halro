package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPeerSessionAuthenticatesAndExchangesTypedRecords(t *testing.T) {
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
			map[string]string{"halro-0": digestText(pin)}, NewNonceCache(16), PeerSessionOptions{})
		serverResult <- result{session, err}
	}()
	client, err := DialPeerSession(ctx, clientSide, clientTLS, primaryHello, key,
		"halro-1", digestText(pin), NewNonceCache(16), PeerSessionOptions{})
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
	cancel()
	for range 2 {
		if err := <-runs; !errors.Is(err, context.Canceled) {
			t.Fatalf("session shutdown error=%v", err)
		}
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

// A peer that is already gone cannot be told the session is ending: the
// close_notify write meets a reset or a broken pipe. The socket is closed
// regardless, so Close reports success — the same as when that alert times
// out — rather than turning a stopped peer into a shutdown failure.
func TestPeerSessionCloseAfterThePeerResetSucceeds(t *testing.T) {
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		raw     net.Conn
		session *PeerSession
		err     error
	}
	accepted := make(chan result, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			accepted <- result{err: err}
			return
		}
		session, err := AcceptPeerSession(ctx, raw, serverTLS, testHello("halro-1", RoleReplica, 0), key,
			map[string]string{"halro-0": digestText(pin)}, NewNonceCache(16), PeerSessionOptions{})
		accepted <- result{raw, session, err}
	}()
	clientRaw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := DialPeerSession(ctx, clientRaw, clientTLS, testHello("halro-0", RolePrimary, 0), key,
		"halro-1", digestText(pin), NewNonceCache(16), PeerSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	if server.err != nil {
		t.Fatal(server.err)
	}
	// An abortive close: the peer's kernel answers with a reset.
	if err := server.raw.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := server.raw.Close(); err != nil {
		t.Fatal(err)
	}
	// Read beneath TLS until the reset arrives — nothing was sent, so no
	// record is consumed — and the close_notify below is written to a socket
	// the kernel already knows is dead. Run is not used: it closes the
	// session itself on a read error, and that close's result is not Close's.
	if err := clientRaw.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := clientRaw.Read(make([]byte, 1)); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("peer abort read=%v, want a reset", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close after the peer reset: %v", err)
	}
}
