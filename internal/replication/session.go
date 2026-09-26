package replication

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"time"
)

func ClientHandshake(ctx context.Context, conn *tls.Conn, local Hello, key []byte, expectedNode, expectedPin string, nonces *NonceCache) (Hello, error) {
	if conn == nil || nonces == nil {
		return Hello{}, errors.New("client handshake requires a TLS connection and nonce cache")
	}
	cleanup := bindConnectionContext(ctx, conn)
	defer cleanup()
	if err := conn.HandshakeContext(ctx); err != nil {
		return Hello{}, fmt.Errorf("replication TLS client handshake: %w", err)
	}
	if err := VerifyConnectionSPKI(conn.ConnectionState(), expectedPin); err != nil {
		return Hello{}, err
	}
	localBytes, err := local.MarshalBinary()
	if err != nil {
		return Hello{}, err
	}
	if err := WriteLengthDelimited(conn, localBytes, MaxHelloBytes); err != nil {
		return Hello{}, fmt.Errorf("write client hello: %w", err)
	}
	peerBytes, err := ReadLengthDelimited(conn, MaxHelloBytes)
	if err != nil {
		return Hello{}, fmt.Errorf("read server hello: %w", err)
	}
	peer, err := UnmarshalHello(peerBytes)
	if err != nil {
		return Hello{}, err
	}
	if err := validatePeerHelloCompatibility(local, peer, expectedNode); err != nil {
		return Hello{}, err
	}
	exporter, err := ExportHandshakeKey(conn.ConnectionState(), local, peer)
	if err != nil {
		return Hello{}, err
	}
	proof, err := HandshakeProof(key, exporter, ProofClient, local, peer)
	if err != nil {
		return Hello{}, err
	}
	if err := writeAll(conn, proof[:]); err != nil {
		return Hello{}, fmt.Errorf("write client proof: %w", err)
	}
	var serverProof [sha256.Size]byte
	if _, err := io.ReadFull(conn, serverProof[:]); err != nil {
		return Hello{}, fmt.Errorf("read server proof: %w", err)
	}
	if err := VerifyHandshakeProof(serverProof, key, exporter, ProofServer, local, peer); err != nil {
		return Hello{}, err
	}
	if nonces.Seen(peer.NodeID, peer.Nonce) {
		return Hello{}, errors.New("peer hello nonce was already used")
	}
	return peer, nil
}

func ServerHandshake(ctx context.Context, conn *tls.Conn, local Hello, key []byte, peerPins map[string]string, nonces *NonceCache) (Hello, error) {
	if conn == nil || nonces == nil {
		return Hello{}, errors.New("server handshake requires a TLS connection and nonce cache")
	}
	cleanup := bindConnectionContext(ctx, conn)
	defer cleanup()
	if err := conn.HandshakeContext(ctx); err != nil {
		return Hello{}, fmt.Errorf("replication TLS server handshake: %w", err)
	}
	peerBytes, err := ReadLengthDelimited(conn, MaxHelloBytes)
	if err != nil {
		return Hello{}, fmt.Errorf("read client hello: %w", err)
	}
	peer, err := UnmarshalHello(peerBytes)
	if err != nil {
		return Hello{}, err
	}
	pin, ok := peerPins[peer.NodeID]
	if !ok {
		return Hello{}, errors.New("client hello identifies an unconfigured member")
	}
	if err := VerifyConnectionSPKI(conn.ConnectionState(), pin); err != nil {
		return Hello{}, err
	}
	if err := validatePeerHelloCompatibility(local, peer, peer.NodeID); err != nil {
		return Hello{}, err
	}
	localBytes, err := local.MarshalBinary()
	if err != nil {
		return Hello{}, err
	}
	if err := WriteLengthDelimited(conn, localBytes, MaxHelloBytes); err != nil {
		return Hello{}, fmt.Errorf("write server hello: %w", err)
	}
	exporter, err := ExportHandshakeKey(conn.ConnectionState(), peer, local)
	if err != nil {
		return Hello{}, err
	}
	var clientProof [sha256.Size]byte
	if _, err := io.ReadFull(conn, clientProof[:]); err != nil {
		return Hello{}, fmt.Errorf("read client proof: %w", err)
	}
	if err := VerifyHandshakeProof(clientProof, key, exporter, ProofClient, peer, local); err != nil {
		return Hello{}, err
	}
	if nonces.Seen(peer.NodeID, peer.Nonce) {
		return Hello{}, errors.New("peer hello nonce was already used")
	}
	proof, err := HandshakeProof(key, exporter, ProofServer, peer, local)
	if err != nil {
		return Hello{}, err
	}
	if err := writeAll(conn, proof[:]); err != nil {
		return Hello{}, fmt.Errorf("write server proof: %w", err)
	}
	return peer, nil
}

func bindConnectionContext(ctx context.Context, conn *tls.Conn) func() {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	return func() {
		stop()
		_ = conn.SetDeadline(time.Time{})
	}
}
