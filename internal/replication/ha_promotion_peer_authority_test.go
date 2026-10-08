package replication

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func peerPromotionCarrierFixture(t *testing.T) (HAPromotionPeerClaims, []byte) {
	t.Helper()
	candidate, _, key := promotionCarrierFixture(t)
	b := candidate.Before
	b.NodeID, b.Role = candidate.Proposal.OldPrimaryNodeID, RolePrimary
	for i := range b.Peers {
		if b.Peers[i].Name == b.NodeID {
			b.Peers[i].Name = candidate.Before.NodeID
		}
	}
	s := candidate.Seal
	s.Role, s.Member = authenticatedstate.SealPrimary, sha256.Sum256([]byte(b.NodeID))
	return HAPromotionPeerClaims{Version: 1, OccurredAtMicros: 1, BaselineDigest: candidate.BaselineDigest, Before: b, Seal: s, Cursors: candidate.Cursors, Heads: candidate.Heads, Proposal: candidate.Proposal}, key
}

func TestHAPromotionPeerCarrierBindsOwnOldRoleAndPreparation(t *testing.T) {
	c, key := peerPromotionCarrierFixture(t)
	preparedBytes, err := SignHAPromotionPeerCarrier(c, key)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := QualifyHAPromotionPeerCarrier(preparedBytes, key)
	if err != nil {
		t.Fatal(err)
	}
	promised := c.Before
	promised.Role, promised.PromisedTerm = RoleReplica, c.Proposal.Term
	if err := prepared.ValidateRecoveryMember(promised); err != nil {
		t.Fatal(err)
	}
	c.Promise = &PromotionPromise{Version: 1, ClusterID: promised.ClusterID, Incarnation: promised.Incarnation, NodeID: promised.NodeID, Role: promised.Role, PreviousRole: c.Before.Role, Term: promised.Term, PromisedTerm: promised.PromisedTerm, DurableIndex: promised.DurableIndex, AppliedIndex: promised.AppliedIndex, LastFrameTerm: promised.Term, PeerPreparationDigest: prepared.Digest()}
	fulfilledBytes, err := SignHAPromotionPeerCarrier(c, key)
	if err != nil {
		t.Fatal(err)
	}
	fulfilled, err := QualifyHAPromotionPeerCarrier(fulfilledBytes, key)
	if err != nil || fulfilled.Claims().Seal.Role != authenticatedstate.SealPrimary || fulfilled.Claims().Before.Role != RolePrimary || fulfilled.ValidateRecoveryMember(promised) != nil {
		t.Fatal("old Primary root relabeled by Promise", err)
	}
	copy := fulfilled.Claims()
	copy.Promise.PeerPreparationDigest[0] ^= 1
	copy.Before.Peers[0].Name = "foreign"
	if fulfilled.Claims().Promise.PeerPreparationDigest != prepared.Digest() {
		t.Fatal("claims escaped mutable authority")
	}
	if _, err := SignHAPromotionPeerCarrier(copy, key); err == nil {
		t.Fatal("different preparation digest signed")
	}
	promised.Term++
	if fulfilled.ValidateRecoveryMember(promised) == nil {
		t.Fatal("new term authority inferred without Leadership")
	}
	fulfilledBytes[len(fulfilledBytes)-1] ^= 1
	if _, err := QualifyHAPromotionPeerCarrier(fulfilledBytes, key); err == nil {
		t.Fatal("tampered peer carrier accepted")
	}
	for name, mutate := range map[string]func(*HAPromotionPeerClaims){
		"candidate root": func(c *HAPromotionPeerClaims) { c.Seal.Member = sha256.Sum256([]byte(c.Proposal.CandidateNodeID)) },
		"Replica label":  func(c *HAPromotionPeerClaims) { c.Seal.Role = authenticatedstate.SealReplica },
		"foreign source": func(c *HAPromotionPeerClaims) { c.Heads[0][0] ^= 1 },
		"new term":       func(c *HAPromotionPeerClaims) { c.Seal.Term++ },
		"missing root":   func(c *HAPromotionPeerClaims) { c.Seal.RootCount-- },
	} {
		t.Run(name, func(t *testing.T) {
			copy := fulfilled.Claims()
			mutate(&copy)
			if _, err := SignHAPromotionPeerCarrier(copy, key); err == nil {
				t.Fatal("independent peer qualification mismatch accepted")
			}
		})
	}
}

func TestPromotionPromiseLegacyEncodingOmitsAbsentPeerPreparation(t *testing.T) {
	c, _ := peerPromotionCarrierFixture(t)
	p := PromotionPromise{Version: 1, ClusterID: c.Before.ClusterID, Incarnation: c.Before.Incarnation, NodeID: c.Before.NodeID, Role: RoleReplica, PreviousRole: RolePrimary, Term: c.Before.Term, PromisedTerm: c.Proposal.Term, DurableIndex: c.Before.DurableIndex, AppliedIndex: c.Before.AppliedIndex, LastFrameTerm: c.Before.Term}
	encoded, err := p.MarshalBinary()
	if err != nil || bytes.Contains(encoded, []byte("peer_preparation_digest")) {
		t.Fatal("legacy zero digest changed the original promise", err)
	}
	decoded, err := UnmarshalPromotionPromise(encoded)
	if err != nil || decoded != p {
		t.Fatal(err)
	}
	p.PeerPreparationDigest = sha256.Sum256([]byte("real preparation carrier"))
	encoded, err = p.MarshalBinary()
	decoded, decodeErr := UnmarshalPromotionPromise(encoded)
	if err != nil || decodeErr != nil || decoded != p || !bytes.Contains(encoded, []byte("peer_preparation_digest")) {
		t.Fatal("new peer digest did not survive actual framing", err, decodeErr)
	}
}
