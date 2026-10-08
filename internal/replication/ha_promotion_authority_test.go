package replication

import (
	"crypto/sha256"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func promotionCarrierFixture(t *testing.T) (HAPromotionClaims, *OrderingJournal, []byte) {
	t.Helper()
	source, journal, baseline, _, _, _ := metadataBoundaryFixture(t)
	descriptor, err := baseline.Descriptor().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	n, term, mac := journal.Head()
	if _, err = journal.Append(OrderingRecord{Index: n + 1, Term: term, Kind: KindMetadataBaseline, Store: StoreNone, ConfirmedIndex: n, Metadata: descriptor, FrameDigest: sha256.Sum256(descriptor)}); err != nil {
		t.Fatal(err)
	}
	if err = source.ActivateMetadataBaseline(journal, n+1); err != nil {
		t.Fatal(err)
	}
	n, term, mac = journal.Head()
	cursors, err := journal.StoreCursors()
	if err != nil {
		t.Fatal(err)
	}
	b := validMemberState()
	b.NodeID = "halro-1"
	b.Role = RoleReplica
	b.Peers[1].Name = "halro-0"
	b.Term = term
	b.PromisedTerm = term
	b.DurableIndex = n
	b.ConfirmedIndex = n
	b.AppliedIndex = n
	b.OrderingHeadMAC = mac
	b.Projection = ProjectionState{Index: n, MetadataEpoch: 2}
	seal := authenticatedstate.CandidateSeal{RootCount: 7, PositionCount: 5, Role: authenticatedstate.SealReplica, Term: term, OrderingTerm: term, OrderingIndex: n, OrderingMAC: mac, Cluster: sha256.Sum256([]byte(b.ClusterID)), Member: sha256.Sum256([]byte(b.NodeID)), Coverage: authenticatedstate.SealCoverageAuthenticated}
	inc := sha256.Sum256([]byte(b.Incarnation))
	copy(seal.Incarnation[:], inc[:16])
	c := HAPromotionClaims{Version: 1, OccurredAtMicros: 1, BaselineDigest: baseline.Descriptor().AuthorityDigest, Before: b, Seal: seal, Cursors: cursors, Proposal: PromotionProposal{Version: 1, ClusterID: b.ClusterID, Incarnation: b.Incarnation, CandidateNodeID: b.NodeID, Term: term + 1, ExpectedTerm: term, ExpectedAppliedIndex: n, OldPrimaryNodeID: "halro-0", FencedBy: FenceNodeIsolated}}
	for slot, positionIndex := range []int{0, 3, 2, 1} {
		hash, err := source.HeadAt(StoreLedger+Store(slot), cursors[slot])
		if err != nil {
			t.Fatal(err)
		}
		c.Heads[slot] = hash
		pos := authenticatedstate.SealPosition{Component: uint8(positionIndex + 1), Sequence: cursors[slot].Sequence, Hash: hash}
		if slot == 0 {
			pos.Generation = cursors[slot].Generation
		}
		if slot == 3 {
			pos.Epoch = cursors[slot].Generation
		}
		c.Seal.Positions[positionIndex] = pos
	}
	c.Seal.Positions[4] = c.Seal.Positions[0]
	c.Seal.Positions[4].Component = 5
	return c, journal, []byte("0123456789abcdef0123456789abcdef")
}

func TestHAPromotionCarrierRejectsUnfulfilledOrUnanchoredTermAuthority(t *testing.T) {
	c, j, key := promotionCarrierFixture(t)
	encoded, err := SignHAPromotionCarrier(c, key)
	if err != nil {
		t.Fatal(err)
	}
	a, err := QualifyHAPromotionCarrier(encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	promised := c.Before
	promised.PromisedTerm = c.Proposal.Term
	if err = a.ValidateRecoveryMember(promised); err != nil {
		t.Fatal("authentic stopped promise", err)
	}
	if err = a.ValidateOrdering(j); err == nil {
		t.Fatal("proposal acquired ordinary new term authority")
	}
	c.Fulfilled = true
	c.Promises = []PromotionPromise{{Version: 1, ClusterID: c.Before.ClusterID, Incarnation: c.Before.Incarnation, NodeID: "halro-2", Role: RoleReplica, PreviousRole: RoleReplica, Term: c.Before.Term, PromisedTerm: c.Proposal.Term, DurableIndex: c.Before.DurableIndex, AppliedIndex: c.Before.AppliedIndex, LastFrameTerm: c.Before.Term}}
	encoded, err = SignHAPromotionCarrier(c, key)
	if err != nil {
		t.Fatal(err)
	}
	a, err = QualifyHAPromotionCarrier(encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ValidateOrdering(j); err == nil {
		t.Fatal("promises without actual ordering anchor passed")
	}
	_, err = j.Append(OrderingRecord{Index: c.Before.AppliedIndex + 1, Term: c.Proposal.Term, Kind: KindLeadershipEstablished, Store: StoreNone, ConfirmedIndex: c.Before.ConfirmedIndex, FrameDigest: sha256.Sum256([]byte("actual new term structural frame"))})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ValidateOrdering(j); err != nil {
		t.Fatal("actual continuous leadership", err)
	}
	changed := promised
	changed.AppliedIndex--
	if err = a.ValidateRecoveryMember(changed); err == nil {
		t.Fatal("different applied cut passed recovery")
	}
	encoded[len(encoded)-1] ^= 1
	if _, err = QualifyHAPromotionCarrier(encoded, key); err == nil {
		t.Fatal("tampered carrier passed")
	}
}

func TestHAPromotionCarrierDoesNotInferMembershipOrOldRootFromCandidate(t *testing.T) {
	c, _, key := promotionCarrierFixture(t)
	for name, change := range map[string]func(*HAPromotionClaims){
		"new role":         func(c *HAPromotionClaims) { c.Seal.Role = authenticatedstate.SealPrimary },
		"new term":         func(c *HAPromotionClaims) { c.Seal.Term++ },
		"foreign MAC":      func(c *HAPromotionClaims) { c.Seal.OrderingMAC[0] ^= 1 },
		"foreign source":   func(c *HAPromotionClaims) { c.Heads[0][0] ^= 1 },
		"mixed cut":        func(c *HAPromotionClaims) { c.Cursors[3].Sequence++ },
		"incomplete roots": func(c *HAPromotionClaims) { c.Seal.RootCount = 6 },
		"unapplied":        func(c *HAPromotionClaims) { c.Before.AppliedIndex-- },
	} {
		t.Run(name, func(t *testing.T) {
			x := c
			change(&x)
			if _, err := SignHAPromotionCarrier(x, key); err == nil {
				t.Fatal("foreign cut signed")
			}
		})
	}
}
