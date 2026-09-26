package replication

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"net"
	"strings"
	"testing"
)

func TestAuthenticatedSessionCombinesMTLSPinHelloAndMasterKeyProof(t *testing.T) {
	files, leaf := writeTestCertificateChain(t)
	serverConfig, err := LoadServerTLSConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := LoadClientTLSConfig(files, "halro-1.internal")
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()
	clientConn := tls.Client(clientSide, clientConfig)
	serverConn := tls.Server(serverSide, serverConfig)
	clientHello := testHello("halro-0", RolePrimary, 1)
	serverHello := testHello("halro-1", RoleReplica, 101)
	key := []byte("0123456789abcdef0123456789abcdef")
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	serverResult := make(chan struct {
		peer Hello
		err  error
	}, 1)
	go func() {
		peer, handshakeErr := ServerHandshake(context.Background(), serverConn, serverHello, key, map[string]string{"halro-0": digestText(pin)}, NewNonceCache(8))
		serverResult <- struct {
			peer Hello
			err  error
		}{peer, handshakeErr}
	}()
	peer, err := ClientHandshake(context.Background(), clientConn, clientHello, key, "halro-1", digestText(pin), NewNonceCache(8))
	if err != nil {
		t.Fatal(err)
	}
	server := <-serverResult
	if server.err != nil {
		t.Fatal(server.err)
	}
	if peer.NodeID != "halro-1" || server.peer.NodeID != "halro-0" {
		t.Fatalf("client peer=%#v server peer=%#v", peer, server.peer)
	}
}

func TestAuthenticatedSessionRefusesWrongClusterKey(t *testing.T) {
	files, leaf := writeTestCertificateChain(t)
	serverConfig, err := LoadServerTLSConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := LoadClientTLSConfig(files, "halro-1.internal")
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()
	clientConn := tls.Client(clientSide, clientConfig)
	serverConn := tls.Server(serverSide, serverConfig)
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	serverErr := make(chan error, 1)
	go func() {
		_, handshakeErr := ServerHandshake(context.Background(), serverConn, testHello("halro-1", RoleReplica, 101), []byte("abcdef0123456789abcdef0123456789"), map[string]string{"halro-0": digestText(pin)}, NewNonceCache(8))
		serverErr <- handshakeErr
		_ = serverSide.Close()
	}()
	_, clientErr := ClientHandshake(context.Background(), clientConn, testHello("halro-0", RolePrimary, 1), []byte("0123456789abcdef0123456789abcdef"), "halro-1", digestText(pin), NewNonceCache(8))
	if clientErr == nil && <-serverErr == nil {
		t.Fatal("wrong cluster key was accepted")
	}
	if clientErr != nil && !strings.Contains(clientErr.Error(), "proof") {
		// The server may close immediately after rejecting the proof, so the
		// client can observe EOF instead. The server side remains the oracle.
		if err := <-serverErr; err == nil || !strings.Contains(err.Error(), "proof") {
			t.Fatalf("client error=%v server error=%v", clientErr, err)
		}
	}
}
