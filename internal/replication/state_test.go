package replication

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func validMemberState() MemberState {
	return MemberState{
		Version: StateVersion, ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0",
		Role: RolePrimary, Term: 7, PromisedTerm: 7, DurableIndex: 11, AppliedIndex: 10,
		OrderingHeadMAC: sha256.Sum256([]byte("ordering")),
		Projection:      ProjectionState{Index: 10, MetadataEpoch: 2, MetadataSequence: 8},
		Peers: []StatePeer{
			{Name: "halro-2", Address: "halro-2.internal:9910", SPKISHA256: "sha256:" + strings.Repeat("b", 64)},
			{Name: "halro-1", Address: "halro-1.internal:9910", SPKISHA256: "sha256:" + strings.Repeat("a", 64)},
		},
	}
}

func TestMemberStateIsAuthenticatedAndCanonical(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	state := validMemberState()
	encoded, err := MarshalState(state, key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(encoded), `"name":"halro-1"`) > strings.Index(string(encoded), `"name":"halro-2"`) {
		t.Fatalf("peers are not canonically sorted: %s", encoded)
	}
	decoded, err := UnmarshalState(encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Peers[0].Name != "halro-1" || decoded.Term != state.Term || decoded.OrderingHeadMAC != state.OrderingHeadMAC {
		t.Fatalf("decoded=%#v", decoded)
	}

	tampered := []byte(strings.Replace(string(encoded), `"durable_index":11`, `"durable_index":12`, 1))
	if _, err := UnmarshalState(tampered, key); err == nil || !strings.Contains(err.Error(), "MAC mismatch") {
		t.Fatalf("tamper error=%v", err)
	}
	if _, err := UnmarshalState(encoded, []byte("abcdef0123456789abcdef0123456789")); err == nil || !strings.Contains(err.Error(), "MAC mismatch") {
		t.Fatalf("wrong-key error=%v", err)
	}
}

func TestMemberStateRefusesUnknownFieldsAndInvalidWatermarks(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	state := validMemberState()
	encoded, err := MarshalState(state, key)
	if err != nil {
		t.Fatal(err)
	}
	unknown := []byte(strings.Replace(string(encoded), `"version":1`, `"unknown":true,"version":1`, 1))
	if _, err := UnmarshalState(unknown, key); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error=%v", err)
	}

	state.AppliedIndex = state.DurableIndex + 1
	if _, err := MarshalState(state, key); err == nil || !strings.Contains(err.Error(), "applied_index") {
		t.Fatalf("watermark error=%v", err)
	}
	state = validMemberState()
	state.PromisedTerm = state.Term - 1
	if _, err := MarshalState(state, key); err == nil || !strings.Contains(err.Error(), "promised_term") {
		t.Fatalf("promise error=%v", err)
	}
	state = validMemberState()
	state.DurableIndex, state.AppliedIndex = 0, 0
	state.Projection = ProjectionState{}
	if _, err := MarshalState(state, key); err == nil || !strings.Contains(err.Error(), "without a durable index") {
		t.Fatalf("zero-index head error=%v", err)
	}
}

func TestMemberStateVersionOneGoldenJSON(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	state := validMemberState()
	encoded, err := MarshalState(state, key)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"version":1,"cluster_id":"production-a","incarnation":"inc_01","node_id":"halro-0","role":"primary","term":7,"promised_term":7,"durable_index":11,"applied_index":10,"ordering_head_mac":"sha256:eee949668040f46c7d28fb995e9cb3a3a97df66f4a4d51e756ad95f7086ceb79","projection":{"index":10,"metadata_epoch":2,"metadata_sequence":8},"peers":[{"name":"halro-1","address":"halro-1.internal:9910","spki_sha256":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"name":"halro-2","address":"halro-2.internal:9910","spki_sha256":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}],"mac":"sha256:d0db827e23f2f7f523859801b9b49b925d2110c6a4ea81433bd5d93bfcc0c0ad"}`
	if string(encoded) != want {
		t.Fatalf("version-1 state fixture changed\n got: %s\nwant: %s", encoded, want)
	}
}
