package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeartbeatRecordIsBoundedAndBidirectional(t *testing.T) {
	heartbeat := encodeHeartbeat()
	for _, direction := range []StreamDirection{StreamPrimaryToReplica, StreamReplicaToPrimary} {
		var stream bytes.Buffer
		if err := WriteStreamRecord(&stream, direction, heartbeat); err != nil {
			t.Fatal(err)
		}
		record, err := ReadStreamRecord(&stream, direction)
		if err != nil || record.Kind != StreamRecordHeartbeat || !bytes.Equal(record.Encoded, heartbeat) {
			t.Fatalf("direction=%d record=%#v err=%v", direction, record, err)
		}
		if err := validateHeartbeat(record.Encoded); err != nil {
			t.Fatal(err)
		}
	}
	for _, offset := range []int{12, 15} {
		bad := append([]byte(nil), heartbeat...)
		bad[offset]++
		if err := validateHeartbeat(bad); err == nil {
			t.Fatalf("modified heartbeat field at %d was accepted", offset)
		}
	}
	tooLong := append(append([]byte(nil), heartbeat...), 0)
	if err := WriteStreamRecord(&bytes.Buffer{}, StreamPrimaryToReplica, tooLong); err == nil {
		t.Fatal("oversized heartbeat was accepted")
	}
}

func TestPeerSessionHeartbeatKeepsIdleAuthenticatedPeersConnected(t *testing.T) {
	client, server := heartbeatTestSessions(t, true)
	if !client.heartbeats || !server.heartbeats {
		t.Fatal("both peers advertised heartbeat support but it was not enabled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delivered atomic.Int64
	runs := make(chan error, 2)
	handler := func(_ context.Context, _ Hello, _ StreamRecord) error {
		delivered.Add(1)
		return nil
	}
	go func() { runs <- client.Run(ctx, handler) }()
	go func() { runs <- server.Run(ctx, handler) }()
	select {
	case err := <-runs:
		t.Fatalf("idle authenticated session ended before four read timeouts: %v", err)
	case <-time.After(1200 * time.Millisecond):
	}
	if delivered.Load() != 0 {
		t.Fatal("internal heartbeats reached the replication data handler")
	}
	cancel()
	for range 2 {
		if err := <-runs; !errors.Is(err, context.Canceled) {
			t.Fatalf("session shutdown error=%v", err)
		}
	}
}

func TestPeerSessionDoesNotSendHeartbeatToVersionOnePeer(t *testing.T) {
	client, server := heartbeatTestSessions(t, false)
	if client.heartbeats || server.heartbeats {
		t.Fatal("heartbeat was enabled for a peer advertising protocol maximum 1")
	}
	if err := client.Send(encodeHeartbeat()); err == nil || !strings.Contains(err.Error(), "owned by the session writer") {
		t.Fatalf("application-supplied heartbeat error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := make(chan error, 2)
	handler := func(context.Context, Hello, StreamRecord) error { return nil }
	go func() { runs <- client.Run(ctx, handler) }()
	go func() { runs <- server.Run(ctx, handler) }()
	select {
	case err := <-runs:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("legacy idle peer ended for a reason other than its original read timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy peer did not retain its bounded read timeout")
	}
	cancel()
	<-runs
}

func TestPeerSessionRejectsUnnegotiatedHeartbeat(t *testing.T) {
	client, server := heartbeatTestSessions(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := make(chan error, 1)
	var delivered atomic.Bool
	go func() {
		run <- server.readLoop(ctx, func(context.Context, Hello, StreamRecord) error {
			delivered.Store(true)
			return nil
		})
	}()
	if err := WriteStreamRecord(client.conn, StreamPrimaryToReplica, encodeHeartbeat()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-run:
		if err == nil || !strings.Contains(err.Error(), "not negotiated") {
			t.Fatalf("unnegotiated heartbeat error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unnegotiated heartbeat was not rejected")
	}
	if delivered.Load() {
		t.Fatal("unnegotiated heartbeat reached the data handler")
	}
}

func heartbeatTestSessions(t *testing.T, bothSupport bool) (*PeerSession, *PeerSession) {
	t.Helper()
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
	primaryHello.Protocol.Maximum = HeartbeatProtocolVersion
	replicaHello := testHello("halro-1", RoleReplica, 0)
	if bothSupport {
		replicaHello.Protocol.Maximum = HeartbeatProtocolVersion
	}
	left, right := net.Pipe()
	handshake, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		session *PeerSession
		err     error
	}
	serverResult := make(chan result, 1)
	options := PeerSessionOptions{ReadTimeout: 300 * time.Millisecond, WriteTimeout: time.Second}
	go func() {
		session, err := AcceptPeerSession(handshake, right, serverTLS, replicaHello, key,
			map[string]string{"halro-0": digestText(pin)}, NewNonceCache(16), options)
		serverResult <- result{session, err}
	}()
	client, err := DialPeerSession(handshake, left, clientTLS, primaryHello, key,
		"halro-1", digestText(pin), NewNonceCache(16), options)
	if err != nil {
		t.Fatal(err)
	}
	server := <-serverResult
	if server.err != nil {
		client.Close()
		t.Fatal(server.err)
	}
	t.Cleanup(func() {
		client.Close()
		server.session.Close()
	})
	return client, server.session
}
