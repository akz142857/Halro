package replication

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func testHello(nodeID string, role Role, nonceByte byte) Hello {
	hello := Hello{
		ClusterID: "production-a", Incarnation: "inc_01", NodeID: nodeID,
		Role: role, Term: 7, PromisedTerm: 7, DurableIndex: 11, AppliedIndex: 10,
		Binary:   VersionRange{Current: 8, Minimum: 7, Maximum: 9},
		Protocol: VersionRange{Current: ProtocolVersion, Minimum: 1, Maximum: 1},
		Schema:   VersionRange{Current: 38, Minimum: 37, Maximum: 39},
		Ledger:   VersionRange{Current: 5, Minimum: 4, Maximum: 5},
		Metadata: VersionRange{Current: 1, Minimum: 1, Maximum: 1},
	}
	for index := range hello.Nonce {
		hello.Nonce[index] = nonceByte + byte(index)
	}
	return hello
}

func TestHelloVersionOneGoldenEncoding(t *testing.T) {
	hello := testHello("halro-0", RolePrimary, 1)
	encoded, err := hello.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "0000008b484c5248454c3031000100010000000c0006000700000000000000070000000000000007000000000000000b000000000000000a0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f2000080007000900010001000100260025002700050004000500010001000170726f64756374696f6e2d61696e635f303168616c726f2d30"
	if got := hex.EncodeToString(encoded); got != wantHex {
		t.Fatalf("version-1 hello fixture changed\n got: %s\nwant: %s", got, wantHex)
	}
	decoded, err := UnmarshalHello(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != hello {
		t.Fatalf("decoded=%#v want=%#v", decoded, hello)
	}
}

func TestHandshakeProofBindsDirectionExporterAndBothHellos(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	client := testHello("halro-0", RolePrimary, 1)
	server := testHello("halro-1", RoleReplica, 101)
	transcript, err := HandshakeTranscriptHash(client, server)
	if err != nil {
		t.Fatal(err)
	}
	clientProof, err := HandshakeProof(key, []byte("tls-exporter"), ProofClient, client, server)
	if err != nil {
		t.Fatal(err)
	}
	serverProof, err := HandshakeProof(key, []byte("tls-exporter"), ProofServer, client, server)
	if err != nil {
		t.Fatal(err)
	}
	if clientProof == serverProof || transcript == ([sha256.Size]byte{}) {
		t.Fatal("direction or transcript did not affect the handshake")
	}
	if err := VerifyHandshakeProof(clientProof, key, []byte("tls-exporter"), ProofClient, client, server); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHandshakeProof(clientProof, key, []byte("other-exporter"), ProofClient, client, server); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("exporter substitution error=%v", err)
	}
	server.Nonce[0] ^= 1
	if err := VerifyHandshakeProof(clientProof, key, []byte("tls-exporter"), ProofClient, client, server); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("hello substitution error=%v", err)
	}
}

func TestPeerHelloRefusesIdentityReplayAndIncompatibleRanges(t *testing.T) {
	local := testHello("halro-0", RolePrimary, 1)
	peer := testHello("halro-1", RoleReplica, 101)
	seen := map[[NonceBytes]byte]bool{}
	replayCache := func(_ string, nonce [NonceBytes]byte) bool {
		already := seen[nonce]
		seen[nonce] = true
		return already
	}
	if err := ValidatePeerHello(local, peer, "halro-1", replayCache); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePeerHello(local, peer, "halro-1", replayCache); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("replay error=%v", err)
	}

	checks := []struct {
		name string
		edit func(*Hello)
		want string
	}{
		{name: "cluster", edit: func(h *Hello) { h.ClusterID = "other" }, want: "cluster_id"},
		{name: "incarnation", edit: func(h *Hello) { h.Incarnation = "inc_other" }, want: "incarnation"},
		{name: "node", edit: func(h *Hello) { h.NodeID = "halro-2" }, want: "node_id"},
		{name: "binary", edit: func(h *Hello) { h.Binary = VersionRange{Current: 11, Minimum: 10, Maximum: 12} }, want: "binary"},
		{name: "schema", edit: func(h *Hello) { h.Schema = VersionRange{Current: 41, Minimum: 40, Maximum: 42} }, want: "schema"},
	}
	for index, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			candidate := testHello("halro-1", RoleReplica, byte(151+index))
			check.edit(&candidate)
			if err := ValidatePeerHello(local, candidate, "halro-1", func(string, [NonceBytes]byte) bool { return false }); err == nil || !strings.Contains(err.Error(), check.want) {
				t.Fatalf("error=%v, want %q", err, check.want)
			}
		})
	}
	if err := ValidatePeerHello(local, testHello("halro-1", RoleReplica, 201), "halro-1", nil); err == nil || !strings.Contains(err.Error(), "cache") {
		t.Fatalf("missing replay cache error=%v", err)
	}
}

func TestHelloRefusesNonCanonicalAndInvalidShapes(t *testing.T) {
	hello := testHello("halro-0", RolePrimary, 1)
	encoded, err := hello.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func([]byte) []byte
		want   string
	}{
		{name: "trailing", mutate: func(value []byte) []byte { return append(value, 0) }, want: "length"},
		{name: "reserved", mutate: func(value []byte) []byte { value[4+12] = 1; return value }, want: "reserved"},
		{name: "version", mutate: func(value []byte) []byte { value[4+9] = 2; return value }, want: "unsupported"},
		{name: "role", mutate: func(value []byte) []byte { value[4+11] = 99; return value }, want: "role"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := test.mutate(append([]byte(nil), encoded...))
			if _, err := UnmarshalHello(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}
