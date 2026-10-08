package replication

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// HAPromotionPeerClaims binds the peer's own independently selected seven roots
// before its irreversible promise. Candidate claims cannot relabel this peer's
// old Primary roots, and this receipt grants no new-term ordinary permission.
type HAPromotionPeerClaims struct {
	Version          uint8
	OccurredAtMicros int64
	BaselineDigest   [32]byte
	Before           MemberState
	Seal             authenticatedstate.CandidateSeal `json:"-"`
	Cursors          [4]StoreCursor
	Heads            [4][32]byte
	Proposal         PromotionProposal
	Promise          *PromotionPromise
}
type HAPromotionPeerAuthority struct {
	claims HAPromotionPeerClaims
	digest [32]byte
}
type haPromotionPeerEnvelope struct {
	Claims HAPromotionPeerClaims
	Seal   haPromotionSeal
}

func peerPromotionEnvelope(c HAPromotionPeerClaims) haPromotionPeerEnvelope {
	return haPromotionPeerEnvelope{Claims: c, Seal: haPromotionSeal{haPromotionSealHeader: haPromotionSealHeader(c.Seal), Roots: append([]authenticatedstate.SealRoot(nil), c.Seal.Roots[:7]...), Positions: append([]authenticatedstate.SealPosition(nil), c.Seal.Positions[:5]...)}}
}
func (c HAPromotionPeerClaims) validate() error {
	b, p, s := c.Before, c.Proposal, c.Seal
	if c.Version != 1 || c.OccurredAtMicros <= 0 || c.BaselineDigest == ([32]byte{}) || b.Validate() != nil || p.Validate() != nil || len(p.ActorID) > MaxIdentityBytes || b.Term != b.PromisedTerm || b.DurableIndex != b.ConfirmedIndex || b.ConfirmedIndex != b.AppliedIndex || b.Projection.Index != b.AppliedIndex || b.Role != RolePrimary && b.Role != RoleReplica {
		return ErrMemberAuthority
	}
	if p.ClusterID != b.ClusterID || p.Incarnation != b.Incarnation || p.CandidateNodeID == b.NodeID || p.ExpectedTerm != b.Term || p.ExpectedAppliedIndex != b.AppliedIndex || p.Term != b.PromisedTerm+1 || p.Term == 0 {
		return ErrMemberAuthority
	}
	member := false
	for _, peer := range b.Peers {
		if peer.Name == p.CandidateNodeID {
			member = true
		}
	}
	if !member || b.Role == RolePrimary && p.OldPrimaryNodeID != b.NodeID {
		return ErrMemberAuthority
	}
	role := authenticatedstate.SealReplica
	if b.Role == RolePrimary {
		role = authenticatedstate.SealPrimary
	}
	inc := sha256.Sum256([]byte(b.Incarnation))
	if s.Role != role || s.Term != b.Term || s.OrderingTerm != b.Term || s.OrderingIndex != b.AppliedIndex || s.OrderingMAC != b.OrderingHeadMAC || s.Cluster != sha256.Sum256([]byte(b.ClusterID)) || s.Member != sha256.Sum256([]byte(b.NodeID)) || !bytes.Equal(s.Incarnation[:], inc[:16]) || s.RootCount != 7 || s.PositionCount != 5 || s.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return ErrMemberAuthority
	}
	for slot, positionIndex := range []int{0, 3, 2, 1} {
		position := s.Positions[positionIndex]
		generation := position.Generation
		if slot == 3 {
			generation = position.Epoch
		}
		if slot == 1 || slot == 2 {
			if generation != 0 {
				return ErrMemberAuthority
			}
			generation = 1
		}
		if c.Cursors[slot] != (StoreCursor{Generation: generation, Sequence: position.Sequence}) || c.Heads[slot] != position.Hash {
			return ErrMemberAuthority
		}
	}
	if c.Promise != nil {
		promise := *c.Promise
		if promise.Validate() != nil || promise.ClusterID != b.ClusterID || promise.Incarnation != b.Incarnation || promise.NodeID != b.NodeID || promise.Role != RoleReplica || promise.PreviousRole != b.Role || promise.Term != b.Term || promise.PromisedTerm != p.Term || promise.DurableIndex != b.DurableIndex || promise.AppliedIndex != b.AppliedIndex || promise.LastFrameTerm != b.Term || promise.PeerPreparationDigest == ([32]byte{}) {
			return ErrMemberAuthority
		}
	}
	return nil
}
func SignHAPromotionPeerCarrier(c HAPromotionPeerClaims, key []byte) ([]byte, error) {
	if len(key) != 32 || c.validate() != nil {
		return nil, ErrMemberAuthority
	}
	if c.Promise != nil {
		before := c
		before.Promise = nil
		prepared, err := SignHAPromotionPeerCarrier(before, key)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(prepared)
		clear(prepared)
		if c.Promise.PeerPreparationDigest != digest {
			return nil, ErrMemberAuthority
		}
	}
	body, err := json.Marshal(peerPromotionEnvelope(c))
	if err != nil || len(body)+32 > MaxHAPromotionCarrierBytes {
		return nil, errors.Join(ErrMemberAuthority, err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:cluster:ha-peer-promotion:v1\x00"))
	mac.Write(body)
	return append(body, mac.Sum(nil)...), nil
}
func QualifyHAPromotionPeerCarrier(encoded, key []byte) (*HAPromotionPeerAuthority, error) {
	if len(key) != 32 || len(encoded) < 33 || len(encoded) > MaxHAPromotionCarrierBytes {
		return nil, ErrMemberAuthority
	}
	body, tag := encoded[:len(encoded)-32], encoded[len(encoded)-32:]
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:cluster:ha-peer-promotion:v1\x00"))
	mac.Write(body)
	if !hmac.Equal(tag, mac.Sum(nil)) {
		return nil, ErrMemberAuthority
	}
	var envelope haPromotionPeerEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, ErrMemberAuthority
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, body) || len(envelope.Seal.Roots) != 7 || len(envelope.Seal.Positions) != 5 {
		return nil, ErrMemberAuthority
	}
	c := envelope.Claims
	c.Seal = authenticatedstate.CandidateSeal(envelope.Seal.haPromotionSealHeader)
	copy(c.Seal.Roots[:], envelope.Seal.Roots)
	copy(c.Seal.Positions[:], envelope.Seal.Positions)
	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.Promise != nil {
		before := c
		before.Promise = nil
		prepared, err := SignHAPromotionPeerCarrier(before, key)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(prepared)
		clear(prepared)
		if c.Promise.PeerPreparationDigest != digest {
			return nil, ErrMemberAuthority
		}
	}
	return &HAPromotionPeerAuthority{claims: c, digest: sha256.Sum256(encoded)}, nil
}
func (a *HAPromotionPeerAuthority) Claims() HAPromotionPeerClaims {
	if a == nil {
		return HAPromotionPeerClaims{}
	}
	c := a.claims
	c.Before = cloneMemberState(c.Before)
	if c.Promise != nil {
		promise := *c.Promise
		c.Promise = &promise
	}
	return c
}
func (a *HAPromotionPeerAuthority) Digest() [32]byte {
	if a == nil {
		return [32]byte{}
	}
	return a.digest
}
func (a *HAPromotionPeerAuthority) ValidateRecoveryMember(member MemberState) error {
	if a == nil || member.Validate() != nil {
		return ErrMemberAuthority
	}
	b, p := a.claims.Before, a.claims.Proposal
	if member.Version != b.Version || member.ClusterID != b.ClusterID || member.Incarnation != b.Incarnation || member.NodeID != b.NodeID || !slices.Equal(member.Peers, b.Peers) || member.DurableIndex != b.DurableIndex || member.ConfirmedIndex != b.ConfirmedIndex || member.AppliedIndex != b.AppliedIndex || member.OrderingHeadMAC != b.OrderingHeadMAC || member.Projection != b.Projection || member.Term != b.Term {
		return ErrMemberAuthority
	}
	if member.PromisedTerm == b.Term && member.Role == b.Role || member.PromisedTerm == p.Term && member.Role == RoleReplica {
		return nil
	}
	return ErrMemberAuthority
}
