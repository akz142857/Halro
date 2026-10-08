package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replicationlimits"
)

// Individual socket Close deliberately does not join the reader's borrowed
// callback. The manager is the outer owner barrier, including control sessions
// which never enter the data registry and Hello adjudication before registration.
func TestConnectionManagerCloseJoinsAuthenticatedCallbacks(t *testing.T) {
	for _, phase := range []string{"data", "control", "hello", "prepare"} {
		t.Run(phase, func(t *testing.T) {
			files, leaf := writeTestCertificateChain(t)
			pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			key := []byte("0123456789abcdef0123456789abcdef")
			block := func(ctx context.Context) {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release // Durable work may not be interruptible after cancellation.
			}
			var prepareCtx context.Context
			budgetManager, _ := queueBudgetFixture(t, 2, (2<<20)+replicationlimits.MinReadRAMBytes)
			budget, err := NewPeerSessionBudget(budgetManager, 2<<20, replicationlimits.MinReadRAMBytes, 2)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := NewConnectionManager(ConnectionManagerOptions{
				LocalNode: "halro-1", TLS: files, ClusterKey: key,
				Peers:      []PeerEndpoint{{NodeID: "halro-0", Address: "127.0.0.1:1", ServerName: "halro-1.internal", SPKISHA256: digestText(pin)}},
				LocalHello: func() (Hello, error) { return testHello("halro-1", RoleReplica, 1), nil },
				Session:    PeerSessionOptions{QueueBudget: budget, QueueCapacity: 2, MaxQueuedBytes: 1 << 20},
				OnAuthenticatedHello: func(ctx context.Context, _ Hello) (bool, error) {
					if phase == "hello" {
						block(ctx)
						return false, ctx.Err()
					}
					prepareCtx = ctx
					return phase == "data" || phase == "prepare", nil
				},
				OnDataSessionReady: func(string, Hello, func([]byte) error) error {
					if phase == "prepare" {
						block(prepareCtx)
						return prepareCtx.Err()
					}
					return nil
				},
				Handle: func(ctx context.Context, _ Hello, record StreamRecord, _ func([]byte) error) error {
					if phase == "data" && record.Kind != StreamRecordFrame {
						return errors.New("expected data")
					}
					if phase == "control" && record.Kind != StreamRecordPromotionProposal {
						return errors.New("expected control")
					}
					block(ctx)
					return ctx.Err()
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			served := make(chan error, 1)
			go func() { served <- manager.Serve(ctx, listener) }()
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); cancel(); _ = manager.Close() })
			raw, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			clientTLS, err := LoadClientTLSConfig(files, "halro-1.internal")
			if err != nil {
				_ = raw.Close()
				t.Fatal(err)
			}
			role := RolePrimary
			if phase == "control" {
				role = RoleReplica
			}
			client, err := DialPeerSession(ctx, raw, clientTLS, testHello("halro-0", role, 2), key, "halro-1", digestText(pin), NewNonceCache(16), PeerSessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			clientDone := make(chan error, 1)
			go func() { clientDone <- client.Run(ctx, func(context.Context, Hello, StreamRecord) error { return nil }) }()
			if phase == "data" || phase == "control" {
				var encoded []byte
				if phase == "data" {
					encoded, err = (Frame{Kind: KindLeadershipEstablished, Index: 1, Term: 7, ClusterID: "production-a", Incarnation: "inc_01"}).MarshalBinary()
				} else {
					encoded, err = testPromotionProposal().MarshalBinary()
				}
				if err != nil {
					t.Fatal(err)
				}
				if err = client.Send(encoded); err != nil {
					t.Fatal(err)
				}
			}
			wait := func(ch <-chan struct{}) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(3 * time.Second):
					t.Fatal("callback phase did not complete")
				}
			}
			wait(entered)
			closed := make(chan error, 3)
			for range 3 {
				go func() { closed <- manager.Close() }()
			}
			wait(canceled)
			select {
			case err := <-closed:
				t.Fatalf("Close returned before callback exited: %v", err)
			default:
			}
			manager.serveMu.Lock()
			retained := manager.key == [sha256.Size]byte(key)
			manager.serveMu.Unlock()
			if !retained {
				t.Fatal("key retired before callback join")
			}
			// The authenticated session's capacity remains owned while its handler lives.
			if len(manager.options.Session.QueueBudget.slots) != 1 {
				t.Fatal("session capacity returned before callback join")
			}
			assertQueueFee(t, budgetManager, budget, 1)
			releaseOnce.Do(func() { close(release) })
			for range 3 {
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Close did not join")
				}
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
			cancel()
			<-clientDone
			if manager.key != [sha256.Size]byte{} {
				t.Fatal("key retained after join")
			}
			if len(manager.options.Session.QueueBudget.slots) != 0 {
				t.Fatal("session capacity retained after join")
			}
			assertQueueFee(t, budgetManager, budget, 0)
			if err := manager.Send("halro-0", nil); !errors.Is(err, ErrPeerNotConnected) {
				t.Fatalf("send after Close: %v", err)
			}
			if err := manager.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConnectionManagerCloseBeforeServePermanentlyWithdraws(t *testing.T) {
	files, leaf := writeTestCertificateChain(t)
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	manager, err := NewConnectionManager(ConnectionManagerOptions{
		LocalNode: "halro-1", TLS: files, ClusterKey: []byte("0123456789abcdef0123456789abcdef"),
		Peers:                []PeerEndpoint{{NodeID: "halro-0", Address: "127.0.0.1:1", SPKISHA256: digestText(pin)}},
		LocalHello:           func() (Hello, error) { t.Error("Hello after Close"); return Hello{}, nil },
		OnAuthenticatedHello: func(context.Context, Hello) (bool, error) { return true, nil },
		Handle:               func(context.Context, Hello, StreamRecord, func([]byte) error) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.key != [sha256.Size]byte{} {
		t.Fatal("unserved key retained")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := manager.Serve(context.Background(), listener); err == nil {
		t.Fatal("closed manager served")
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("rejected listener still live: %v", err)
	}
}
