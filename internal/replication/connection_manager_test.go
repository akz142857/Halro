package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"testing"
	"time"
)

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
		OnDataSessionReady: func(peer string, hello Hello) error {
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
