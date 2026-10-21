package replication

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	HandshakeVersion uint16 = 1
	NonceBytes              = 32
	MaxHelloBytes           = 1024

	helloFixedBytes = 114
)

var (
	helloMagic      = [8]byte{'H', 'L', 'R', 'H', 'E', 'L', '0', '1'}
	handshakeDomain = []byte("halro:cluster:v1\x00handshake\x00")
)

type ProofDirection byte

const (
	ProofClient ProofDirection = 1
	ProofServer ProofDirection = 2
)

// VersionRange declares what this binary writes now and the inclusive range it
// can consume. Current must itself be in the range.
type VersionRange struct {
	Current uint16
	Minimum uint16
	Maximum uint16
}

func (r VersionRange) validate(name string) error {
	if r.Minimum == 0 || r.Maximum < r.Minimum || r.Current < r.Minimum || r.Current > r.Maximum {
		return fmt.Errorf("hello %s version range is invalid", name)
	}
	return nil
}

func (r VersionRange) accepts(version uint16) bool {
	return version >= r.Minimum && version <= r.Maximum
}

func rangesIntersect(left, right VersionRange) bool {
	return left.Minimum <= right.Maximum && right.Minimum <= left.Maximum
}

type Hello struct {
	ClusterID    string
	Incarnation  string
	NodeID       string
	Role         Role
	Term         uint64
	PromisedTerm uint64
	DurableIndex uint64
	AppliedIndex uint64
	Nonce        [NonceBytes]byte
	Binary       VersionRange
	Protocol     VersionRange
	Schema       VersionRange
	Ledger       VersionRange
	Metadata     VersionRange
}

func (h Hello) Validate() error {
	for name, value := range map[string]string{
		"cluster_id": h.ClusterID, "incarnation": h.Incarnation, "node_id": h.NodeID,
	} {
		if len(value) == 0 || len(value) > MaxIdentityBytes {
			return fmt.Errorf("hello %s must be 1-%d bytes", name, MaxIdentityBytes)
		}
	}
	switch h.Role {
	case RolePrimary, RoleReplica, RoleAwaitingDecision:
	default:
		return fmt.Errorf("hello role %q is invalid", h.Role)
	}
	if h.Term == 0 || h.PromisedTerm < h.Term {
		return errors.New("hello term must be positive and not exceed promised_term")
	}
	if h.AppliedIndex > h.DurableIndex {
		return errors.New("hello applied_index cannot exceed durable_index")
	}
	if h.Nonce == ([NonceBytes]byte{}) {
		return errors.New("hello nonce must be non-zero")
	}
	for name, versionRange := range map[string]VersionRange{
		"binary": h.Binary, "protocol": h.Protocol, "schema": h.Schema,
		"ledger": h.Ledger, "metadata": h.Metadata,
	} {
		if err := versionRange.validate(name); err != nil {
			return err
		}
	}
	if h.Protocol.Current != ProtocolVersion {
		return fmt.Errorf("hello protocol current version must be %d", ProtocolVersion)
	}
	return nil
}

func roleCode(role Role) uint16 {
	switch role {
	case RolePrimary:
		return 1
	case RoleReplica:
		return 2
	case RoleAwaitingDecision:
		return 3
	default:
		return 0
	}
}

func roleFromCode(code uint16) Role {
	switch code {
	case 1:
		return RolePrimary
	case 2:
		return RoleReplica
	case 3:
		return RoleAwaitingDecision
	default:
		return ""
	}
}

func (h Hello) MarshalBinary() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	bodyLength := helloFixedBytes + len(h.ClusterID) + len(h.Incarnation) + len(h.NodeID)
	encoded := make([]byte, 4+bodyLength)
	binary.BigEndian.PutUint32(encoded[:4], uint32(bodyLength))
	body := encoded[4:]
	copy(body[:8], helloMagic[:])
	binary.BigEndian.PutUint16(body[8:10], HandshakeVersion)
	binary.BigEndian.PutUint16(body[10:12], roleCode(h.Role))
	// body[12:14] is reserved.
	binary.BigEndian.PutUint16(body[14:16], uint16(len(h.ClusterID)))
	binary.BigEndian.PutUint16(body[16:18], uint16(len(h.Incarnation)))
	binary.BigEndian.PutUint16(body[18:20], uint16(len(h.NodeID)))
	binary.BigEndian.PutUint64(body[20:28], h.Term)
	binary.BigEndian.PutUint64(body[28:36], h.PromisedTerm)
	binary.BigEndian.PutUint64(body[36:44], h.DurableIndex)
	binary.BigEndian.PutUint64(body[44:52], h.AppliedIndex)
	copy(body[52:84], h.Nonce[:])
	offset := 84
	for _, versionRange := range []VersionRange{h.Binary, h.Protocol, h.Schema, h.Ledger, h.Metadata} {
		binary.BigEndian.PutUint16(body[offset:offset+2], versionRange.Current)
		binary.BigEndian.PutUint16(body[offset+2:offset+4], versionRange.Minimum)
		binary.BigEndian.PutUint16(body[offset+4:offset+6], versionRange.Maximum)
		offset += 6
	}
	copy(body[helloFixedBytes:], h.ClusterID)
	copy(body[helloFixedBytes+len(h.ClusterID):], h.Incarnation)
	copy(body[helloFixedBytes+len(h.ClusterID)+len(h.Incarnation):], h.NodeID)
	return encoded, nil
}

func UnmarshalHello(encoded []byte) (Hello, error) {
	if len(encoded) < 4+helloFixedBytes || len(encoded) > MaxHelloBytes {
		return Hello{}, errors.New("hello is truncated or exceeds its size bound")
	}
	declared := int(binary.BigEndian.Uint32(encoded[:4]))
	if declared != len(encoded)-4 {
		return Hello{}, errors.New("hello length is not canonical")
	}
	body := encoded[4:]
	if !bytes.Equal(body[:8], helloMagic[:]) {
		return Hello{}, errors.New("hello magic is invalid")
	}
	if version := binary.BigEndian.Uint16(body[8:10]); version != HandshakeVersion {
		return Hello{}, fmt.Errorf("unsupported hello version %d", version)
	}
	if binary.BigEndian.Uint16(body[12:14]) != 0 {
		return Hello{}, errors.New("hello reserved field is non-zero")
	}
	clusterLength := int(binary.BigEndian.Uint16(body[14:16]))
	incarnationLength := int(binary.BigEndian.Uint16(body[16:18]))
	nodeLength := int(binary.BigEndian.Uint16(body[18:20]))
	if clusterLength == 0 || clusterLength > MaxIdentityBytes || incarnationLength == 0 || incarnationLength > MaxIdentityBytes ||
		nodeLength == 0 || nodeLength > MaxIdentityBytes || helloFixedBytes+clusterLength+incarnationLength+nodeLength != len(body) {
		return Hello{}, errors.New("hello identity lengths are invalid")
	}
	hello := Hello{
		Role:         roleFromCode(binary.BigEndian.Uint16(body[10:12])),
		Term:         binary.BigEndian.Uint64(body[20:28]),
		PromisedTerm: binary.BigEndian.Uint64(body[28:36]),
		DurableIndex: binary.BigEndian.Uint64(body[36:44]),
		AppliedIndex: binary.BigEndian.Uint64(body[44:52]),
	}
	copy(hello.Nonce[:], body[52:84])
	offset := 84
	ranges := []*VersionRange{&hello.Binary, &hello.Protocol, &hello.Schema, &hello.Ledger, &hello.Metadata}
	for _, versionRange := range ranges {
		versionRange.Current = binary.BigEndian.Uint16(body[offset : offset+2])
		versionRange.Minimum = binary.BigEndian.Uint16(body[offset+2 : offset+4])
		versionRange.Maximum = binary.BigEndian.Uint16(body[offset+4 : offset+6])
		offset += 6
	}
	offset = helloFixedBytes
	hello.ClusterID = string(body[offset : offset+clusterLength])
	offset += clusterLength
	hello.Incarnation = string(body[offset : offset+incarnationLength])
	offset += incarnationLength
	hello.NodeID = string(body[offset : offset+nodeLength])
	if err := hello.Validate(); err != nil {
		return Hello{}, err
	}
	return hello, nil
}

// ValidatePeerHello applies the checks that are independent of TLS chain and
// SPKI verification. seenNonce must be a bounded per-peer replay cache owned by
// the connection manager; nil is refused rather than silently disabling replay
// protection.
func ValidatePeerHello(local, peer Hello, expectedPeerNode string, seenNonce func(nodeID string, nonce [NonceBytes]byte) bool) error {
	if err := validatePeerHelloCompatibility(local, peer, expectedPeerNode); err != nil {
		return err
	}
	if seenNonce == nil {
		return errors.New("peer nonce replay cache is required")
	}
	if seenNonce(peer.NodeID, peer.Nonce) {
		return errors.New("peer hello nonce was already used")
	}
	return nil
}

func validatePeerHelloCompatibility(local, peer Hello, expectedPeerNode string) error {
	if err := local.Validate(); err != nil {
		return fmt.Errorf("local hello: %w", err)
	}
	if err := peer.Validate(); err != nil {
		return fmt.Errorf("peer hello: %w", err)
	}
	if local.ClusterID != peer.ClusterID {
		return errors.New("peer cluster_id does not match")
	}
	if local.Incarnation != peer.Incarnation {
		return errors.New("peer incarnation does not match")
	}
	if peer.NodeID != expectedPeerNode || peer.NodeID == local.NodeID {
		return errors.New("peer node_id does not match the authenticated member")
	}
	for name, pair := range map[string][2]VersionRange{
		"binary": {local.Binary, peer.Binary}, "protocol": {local.Protocol, peer.Protocol},
		"schema": {local.Schema, peer.Schema}, "ledger": {local.Ledger, peer.Ledger},
		"metadata": {local.Metadata, peer.Metadata},
	} {
		if !rangesIntersect(pair[0], pair[1]) || !pair[0].accepts(pair[1].Current) || !pair[1].accepts(pair[0].Current) {
			return fmt.Errorf("peer %s versions are incompatible", name)
		}
	}
	return nil
}

// HandshakeTranscriptHash is the context passed to TLS ExportKeyingMaterial
// with label "EXPORTER-Halro-Replication-v1". Length prefixes prevent two
// different hello pairs from having the same concatenation.
func HandshakeTranscriptHash(client, server Hello) ([sha256.Size]byte, error) {
	var transcript [sha256.Size]byte
	clientBytes, err := client.MarshalBinary()
	if err != nil {
		return transcript, fmt.Errorf("client hello: %w", err)
	}
	serverBytes, err := server.MarshalBinary()
	if err != nil {
		return transcript, fmt.Errorf("server hello: %w", err)
	}
	hash := sha256.New()
	writeProofField(hash, clientBytes)
	writeProofField(hash, serverBytes)
	copy(transcript[:], hash.Sum(nil))
	return transcript, nil
}

// HandshakeProof authenticates an exporter produced with the transcript hash
// above. The caller obtains that exporter from the already-verified TLS 1.3
// connection; this package never treats certificate possession as the proof.
func HandshakeProof(key, exporter []byte, direction ProofDirection, client, server Hello) ([sha256.Size]byte, error) {
	var proof [sha256.Size]byte
	if len(key) != sha256.Size {
		return proof, errors.New("handshake key must be 32 bytes")
	}
	if len(exporter) == 0 || len(exporter) > 65535 {
		return proof, errors.New("TLS exporter must be 1-65535 bytes")
	}
	if direction != ProofClient && direction != ProofServer {
		return proof, errors.New("handshake proof direction is invalid")
	}
	clientBytes, err := client.MarshalBinary()
	if err != nil {
		return proof, fmt.Errorf("client hello: %w", err)
	}
	serverBytes, err := server.MarshalBinary()
	if err != nil {
		return proof, fmt.Errorf("server hello: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(handshakeDomain)
	mac.Write([]byte{byte(direction)})
	writeProofField(mac, exporter)
	mac.Write(client.Nonce[:])
	mac.Write(server.Nonce[:])
	writeProofField(mac, clientBytes)
	writeProofField(mac, serverBytes)
	copy(proof[:], mac.Sum(nil))
	return proof, nil
}

func VerifyHandshakeProof(provided [sha256.Size]byte, key, exporter []byte, direction ProofDirection, client, server Hello) error {
	expected, err := HandshakeProof(key, exporter, direction, client, server)
	if err != nil {
		return err
	}
	if !hmac.Equal(provided[:], expected[:]) {
		return errors.New("handshake proof mismatch")
	}
	return nil
}

func writeProofField(mac interface{ Write([]byte) (int, error) }, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(value)
}
