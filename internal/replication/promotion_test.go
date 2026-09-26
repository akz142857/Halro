package replication

import (
	"encoding/binary"
	"testing"
)

func testPromotionProposal() PromotionProposal {
	return PromotionProposal{
		Version: PromotionProtocolVersion, ClusterID: "production-a", Incarnation: "inc_01",
		CandidateNodeID: "halro-1", Term: 8, ExpectedTerm: 7, ExpectedAppliedIndex: 42,
		OldPrimaryNodeID: "halro-0", FencedBy: FenceNodeIsolated,
	}
}

func testPromotionCandidate() MemberState {
	candidate := publisherTestState(RoleReplica)
	candidate.NodeID = "halro-1"
	candidate.Peers[0].Name = "halro-0"
	candidate.DurableIndex = 42
	candidate.ConfirmedIndex = 42
	candidate.AppliedIndex = 42
	candidate.OrderingHeadMAC = [32]byte{1}
	candidate.Projection.Index = 42
	return candidate
}

func TestPromotionRecordsRoundTripAndRejectUnknownFields(t *testing.T) {
	proposal := testPromotionProposal()
	encoded, err := proposal.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalPromotionProposal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != proposal {
		t.Fatalf("proposal=%#v, want %#v", decoded, proposal)
	}

	promise := PromotionPromise{
		Version: PromotionProtocolVersion, ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-2",
		Role: RoleReplica, Term: 7, PromisedTerm: 8, DurableIndex: 42, AppliedIndex: 42, LastFrameTerm: 7,
	}
	encoded, err = promise.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedPromise, err := UnmarshalPromotionPromise(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decodedPromise != promise {
		t.Fatalf("promise=%#v, want %#v", decodedPromise, promise)
	}

	body := append([]byte(`{"version":1,"cluster_id":"production-a","incarnation":"inc_01","candidate_node_id":"halro-1","term":8,"expected_term":7,"expected_applied_index":42,"old_primary_node_id":"halro-0","fenced_by":"node-isolated","surprise":true}`), nil...)
	unknown := make([]byte, 4+8+len(body))
	binary.BigEndian.PutUint32(unknown[:4], uint32(len(unknown)-4))
	copy(unknown[4:12], proposalMagic[:])
	copy(unknown[12:], body)
	if _, err := UnmarshalPromotionProposal(unknown); err == nil {
		t.Fatal("unknown promotion field was accepted")
	}
}

func TestValidatePromotionPromisesUsesTermThenIndexFreshness(t *testing.T) {
	candidate := testPromotionCandidate()
	proposal := testPromotionProposal()
	promise := PromotionPromise{
		Version: PromotionProtocolVersion, ClusterID: candidate.ClusterID, Incarnation: candidate.Incarnation, NodeID: "halro-1",
		Role: RoleReplica, Term: 7, PromisedTerm: 8, DurableIndex: 42, AppliedIndex: 41, LastFrameTerm: 7,
	}
	if err := ValidatePromotionPromises(candidate, proposal, []PromotionPromise{promise}, false); err != nil {
		t.Fatal(err)
	}
	promise.DurableIndex = 43
	if err := ValidatePromotionPromises(candidate, proposal, []PromotionPromise{promise}, false); err == nil {
		t.Fatal("candidate behind same-term peer was accepted")
	}
	promise.DurableIndex = 1
	promise.LastFrameTerm = 8
	promise.Term = 8
	if err := ValidatePromotionPromises(candidate, proposal, []PromotionPromise{promise}, false); err == nil {
		t.Fatal("candidate behind newer-term peer was accepted")
	}
}

func TestNoPeerPromiseIsRestrictedToTwoMemberCluster(t *testing.T) {
	candidate := testPromotionCandidate()
	proposal := testPromotionProposal()
	if err := ValidatePromotionPromises(candidate, proposal, nil, true); err != nil {
		t.Fatal(err)
	}
	candidate.Peers = append(candidate.Peers, StatePeer{Name: "halro-2", Address: "halro-2.internal:9910", SPKISHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if err := ValidatePromotionPromises(candidate, proposal, nil, true); err == nil {
		t.Fatal("three-member no-peer promotion was accepted")
	}
}
