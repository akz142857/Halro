package replication

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func pairedPeerCompletionFixture(t *testing.T) (*HAPromotionPeerCompletion, *OrderingJournal, []byte) {
	t.Helper()
	c, j, key := promotionCarrierFixture(t)
	b := cloneMemberState(c.Before)
	b.NodeID, b.Role = c.Proposal.OldPrimaryNodeID, RolePrimary
	for i := range b.Peers {
		if b.Peers[i].Name == b.NodeID {
			b.Peers[i].Name = c.Before.NodeID
		}
	}
	s := c.Seal
	s.Role, s.Member = authenticatedstate.SealPrimary, sha256.Sum256([]byte(b.NodeID))
	p := HAPromotionPeerClaims{Version: 1, OccurredAtMicros: 1, BaselineDigest: c.BaselineDigest, Before: b, Seal: s, Cursors: c.Cursors, Heads: c.Heads, Proposal: c.Proposal}
	raw, e := SignHAPromotionPeerCarrier(p, key)
	if e != nil {
		t.Fatal(e)
	}
	prepare, e := QualifyHAPromotionPeerCarrier(raw, key)
	if e != nil {
		t.Fatal(e)
	}
	p.Promise = &PromotionPromise{Version: 1, ClusterID: b.ClusterID, Incarnation: b.Incarnation, NodeID: b.NodeID, Role: RoleReplica, PreviousRole: b.Role, Term: b.Term, PromisedTerm: c.Proposal.Term, DurableIndex: b.DurableIndex, AppliedIndex: b.AppliedIndex, LastFrameTerm: b.Term, PeerPreparationDigest: prepare.Digest()}
	raw, e = SignHAPromotionPeerCarrier(p, key)
	if e != nil {
		t.Fatal(e)
	}
	peer, e := QualifyHAPromotionPeerCarrier(raw, key)
	if e != nil {
		t.Fatal(e)
	}
	c.Fulfilled = true
	c.Promises = []PromotionPromise{*p.Promise}
	raw, e = SignHAPromotionCarrier(c, key)
	if e != nil {
		t.Fatal(e)
	}
	candidate, e := QualifyHAPromotionCarrier(raw, key)
	if e != nil {
		t.Fatal(e)
	}
	pair, e := QualifyHAPromotionPeerCompletion(peer, candidate)
	if e != nil {
		t.Fatal(e)
	}
	return pair, j, key
}

func TestHAPeerCompletionRequiresOwnPromiseAndRealSingleLeadership(t *testing.T) {
	a, j, key := pairedPeerCompletionFixture(t)
	p := a.Peer().Claims()
	m := cloneMemberState(p.Before)
	m.Role, m.PromisedTerm = RoleReplica, p.Proposal.Term
	if a.ValidateRecoveryMember(m, j) != nil {
		t.Fatal("own durable Promise rejected")
	}
	m.Term = p.Proposal.Term
	if a.ValidateRecoveryMember(m, j) != nil || a.ValidateLeadershipMember(m, j) == nil {
		t.Fatal("adoption inferred nonexistent Leadership")
	}
	for name, change := range map[string]func(*HAPromotionClaims){
		"missing own promise":   func(c *HAPromotionClaims) { c.Promises[0].NodeID = "halro-2"; c.Promises[0].PreviousRole = RoleReplica },
		"different preparation": func(c *HAPromotionClaims) { c.Promises[0].PeerPreparationDigest[0] ^= 1 },
		"different own roots provenance": func(c *HAPromotionClaims) {
			c.Heads[0][0] ^= 1
			c.Seal.Positions[0].Hash = c.Heads[0]
			c.Seal.Positions[4].Hash = c.Heads[0]
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := a.Candidate().Claims()
			change(&c)
			raw, e := SignHAPromotionCarrier(c, key)
			if e != nil {
				t.Fatal("test must preserve valid MAC/schema", e)
			}
			other, e := QualifyHAPromotionCarrier(raw, key)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = QualifyHAPromotionPeerCompletion(a.Peer(), other); e == nil {
				t.Fatal("MAC-valid independent mismatch accepted")
			}
		})
	}
	r, e := j.Append(OrderingRecord{Index: p.Before.AppliedIndex + 1, Term: p.Proposal.Term, Kind: KindLeadershipEstablished, Store: StoreNone, ConfirmedIndex: p.Before.ConfirmedIndex, FrameDigest: sha256.Sum256([]byte("actual structural frame"))})
	if e != nil {
		t.Fatal(e)
	}
	m.DurableIndex, m.ConfirmedIndex, m.OrderingHeadMAC = r.Index, r.Index, r.MAC
	if e = a.ValidateLeadershipMember(m, j); e != nil {
		t.Fatal("real received boundary", e)
	}
	m.AppliedIndex, m.Projection.Index = r.Index, r.Index
	if e = a.ValidateLeadershipMember(m, j); e != nil {
		t.Fatal("real complete boundary", e)
	}
	for _, change := range []func(*MemberState){func(m *MemberState) { m.Role = RolePrimary }, func(m *MemberState) { m.NodeID = "foreign" }, func(m *MemberState) { m.Projection.MetadataSequence++ }, func(m *MemberState) { m.DurableIndex++ }} {
		bad := cloneMemberState(m)
		change(&bad)
		if a.ValidateLeadershipMember(bad, j) == nil {
			t.Fatal("foreign Member/role/native projection permission accepted")
		}
	}
	if _, e = j.Append(OrderingRecord{Index: r.Index + 1, Term: p.Proposal.Term, Kind: KindData, Store: StoreAudit, StoreGeneration: 1, StoreSequenceFirst: p.Cursors[1].Sequence + 1, StoreSequenceLast: p.Cursors[1].Sequence + 1, ConfirmedIndex: r.Index, FrameDigest: sha256.Sum256([]byte("another semantic frame"))}); e != nil {
		t.Fatal(e)
	}
	if a.ValidateRecoveryMember(m, j) == nil {
		t.Fatal("second ordering frame admitted to cold transition")
	}
}

func TestHAPeerBoundaryStreamRejectsSemanticOrUnboundedRecords(t *testing.T) {
	a, _, key := pairedPeerCompletionFixture(t)
	p := a.Peer().Claims()
	peer, e := SignHAPromotionPeerCarrier(p, key)
	if e != nil {
		t.Fatal(e)
	}
	request, e := MarshalHAPeerPromotionRequest(peer)
	if e != nil {
		t.Fatal(e)
	}
	candidate, e := SignHAPromotionCarrier(a.Candidate().Claims(), key)
	if e != nil {
		t.Fatal(e)
	}
	f := Frame{Kind: KindLeadershipEstablished, Store: StoreNone, Index: p.Before.AppliedIndex + 1, Term: p.Proposal.Term, ConfirmedIndex: p.Before.ConfirmedIndex, ClusterID: p.Before.ClusterID, Incarnation: p.Before.Incarnation}
	frame, e := f.MarshalBinary()
	if e != nil {
		t.Fatal(e)
	}
	boundary, e := MarshalHAPeerPromotionBoundary(candidate, frame)
	if e != nil {
		t.Fatal(e)
	}
	for _, direction := range []StreamDirection{StreamPrimaryToReplica, StreamReplicaToPrimary} {
		for _, encoded := range [][]byte{request, boundary} {
			var wire bytes.Buffer
			if e = WriteStreamRecord(&wire, direction, encoded); e != nil {
				t.Fatal(e)
			}
			record, e := ReadStreamRecord(&wire, direction)
			if e != nil || !bytes.Equal(record.Encoded, encoded) {
				t.Fatal(e)
			}
		}
	}
	got, leader, e := UnmarshalHAPeerPromotionBoundary(boundary)
	if e != nil || !bytes.Equal(got, candidate) || !bytes.Equal(leader, frame) {
		t.Fatal(e)
	}
	f.Kind, f.Store = KindData, StoreAudit
	f.StoreGeneration, f.StoreSequenceFirst, f.StoreSequenceLast = 1, 1, 1
	f.Payload = []byte("semantic")
	semantic, e := f.MarshalBinary()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = MarshalHAPeerPromotionBoundary(candidate, semantic); e == nil {
		t.Fatal("semantic receive bypassed cold boundary")
	}
	bad := append([]byte(nil), boundary...)
	binary.BigEndian.PutUint32(bad[12:16], MaxHAPromotionCarrierBytes+1)
	if _, _, e = UnmarshalHAPeerPromotionBoundary(bad); e == nil {
		t.Fatal("independent carrier length ignored")
	}
	oversize := append([]byte(nil), boundary[:12]...)
	binary.BigEndian.PutUint32(oversize[:4], maxHAPeerBoundaryBodyBytes+1)
	if _, e = ReadStreamRecord(bytes.NewReader(oversize), StreamPrimaryToReplica); e == nil {
		t.Fatal("oversize body allocated/read before type ceiling")
	}
}
