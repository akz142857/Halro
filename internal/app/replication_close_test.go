package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

// Exercise the terminal Accounting owner path with an actual transport-owned
// callback. Even after cancellation it must retain its native dependency until
// non-interruptible work exits; merely closing the socket does not establish this.
func TestTerminalAccountingCloseJoinsTransportBeforeNative(t *testing.T) {
	native, _, _ := projectedWriterLog(t, nil, ledger.Options{})
	ca, caKey := makeReplicationCertificate(t, nil, nil, true, "close-ca")
	cert, key := makeReplicationCertificate(t, ca, caKey, false, "close-node")
	files := writeReplicationTLSFiles(t, t.TempDir(), ca, cert, key)
	tlsFiles := replication.TLSFiles{CAFile: files.CAFile, CertFile: files.CertFile, KeyFile: files.KeyFile}
	pin := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	pinText := "sha256:" + hex.EncodeToString(pin[:])
	clusterKey := []byte("0123456789abcdef0123456789abcdef")
	hello := func(node string, role replication.Role) replication.Hello {
		version := replication.VersionRange{Current: 1, Minimum: 1, Maximum: 1}
		return replication.Hello{ClusterID: "production-a", Incarnation: "inc_01", NodeID: node, Role: role, Term: 7, PromisedTerm: 7,
			Binary: version, Protocol: version, Schema: version, Ledger: version, Metadata: version}
	}
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	dependencyResult := make(chan error, 1)
	var releaseOnce sync.Once
	manager, err := replication.NewConnectionManager(replication.ConnectionManagerOptions{
		LocalNode: "halro-1", TLS: tlsFiles, ClusterKey: clusterKey,
		Peers:                []replication.PeerEndpoint{{NodeID: "halro-0", Address: "127.0.0.1:1", SPKISHA256: pinText}},
		LocalHello:           func() (replication.Hello, error) { return hello("halro-1", replication.RoleReplica), nil },
		OnAuthenticatedHello: func(context.Context, replication.Hello) (bool, error) { return true, nil },
		Handle: func(ctx context.Context, _ replication.Hello, _ replication.StreamRecord, _ func([]byte) error) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			_, err := native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, nil)
			dependencyResult <- err
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	served := make(chan error, 1)
	go func() { served <- manager.Serve(ctx, listener) }()
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); cancel(); _ = manager.Close(); _ = listener.Close() })
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := replication.LoadClientTLSConfig(tlsFiles, "127.0.0.1")
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	client, err := replication.DialPeerSession(ctx, raw, clientTLS, hello("halro-0", replication.RolePrimary), clusterKey, "halro-1", pinText, replication.NewNonceCache(8), replication.PeerSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- client.Run(ctx, func(context.Context, replication.Hello, replication.StreamRecord) error { return nil })
	}()
	encoded, err := (replication.Frame{Kind: replication.KindLeadershipEstablished, Index: 1, Term: 7, ClusterID: "production-a", Incarnation: "inc_01"}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send(encoded); err != nil {
		t.Fatal(err)
	}
	awaitDrainSignal(t, entered)
	r := &Runtime{ledger: native, accountingState: runtimeAccountingState{reader: ledger.NewState()}, replication: &replicationRuntime{manager: manager}}
	closed := make(chan error, 1)
	go func() { closed <- r.closeAccountingOwner() }()
	awaitDrainSignal(t, canceled)
	select {
	case err := <-closed:
		t.Fatalf("native owner retired before callback exit: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal close failed to join")
	}
	if err := <-dependencyResult; err != nil {
		t.Fatalf("callback native dependency closed early: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	cancel()
	<-clientDone
	if r.accountingState.reader != nil {
		t.Fatal("historical State alias retained after join")
	}
	if _, err := native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, nil); err == nil {
		t.Fatal("native still open after terminal close")
	}
}
