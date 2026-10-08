package replication

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

const MaxHAPromotionCarrierBytes = 32 << 10

// HAPromotionClaims preserves the independently selected old cut. Neither the
// proposal nor an old seal grants the new term: qualification also requires the
// actual durable Member transition and continuous authenticated Ordering.
type HAPromotionClaims struct {
	Version          uint8
	OccurredAtMicros int64
	BaselineDigest   [32]byte
	Before           MemberState
	Seal             authenticatedstate.CandidateSeal `json:"-"`
	Cursors          [4]StoreCursor
	Heads            [4][32]byte
	Proposal         PromotionProposal
	Promises         []PromotionPromise
	NoPeerPromise    bool
	Fulfilled        bool
}

type HAPromotionAuthority struct {
	claims HAPromotionClaims
	digest [32]byte
}

// The carrier encodes the seven selected entries only, rather than all 64
// capacity slots in CandidateSeal. All unused slots are restored as zero.
type haPromotionSealHeader authenticatedstate.CandidateSeal
type haPromotionSeal struct {
	haPromotionSealHeader
	Roots     []authenticatedstate.SealRoot
	Positions []authenticatedstate.SealPosition
}
type haPromotionEnvelope struct {
	Claims HAPromotionClaims
	Seal   haPromotionSeal
}

func promotionEnvelope(c HAPromotionClaims) haPromotionEnvelope {
	return haPromotionEnvelope{Claims: c, Seal: haPromotionSeal{haPromotionSealHeader: haPromotionSealHeader(c.Seal), Roots: append([]authenticatedstate.SealRoot(nil), c.Seal.Roots[:7]...), Positions: append([]authenticatedstate.SealPosition(nil), c.Seal.Positions[:5]...)}}
}

func (c HAPromotionClaims) validate() error {
	if err := c.Before.Validate(); err != nil {
		return fmt.Errorf("promotion old Member: %w", err)
	}
	if err := c.Proposal.Validate(); err != nil {
		return fmt.Errorf("promotion proposal: %w", err)
	}
	if c.Version != 1 || c.OccurredAtMicros <= 0 || c.BaselineDigest == ([32]byte{}) || c.Before.Validate() != nil || c.Proposal.Validate() != nil || c.Before.Term != c.Before.PromisedTerm || c.Before.DurableIndex != c.Before.AppliedIndex || c.Before.ConfirmedIndex != c.Before.AppliedIndex || c.Before.Projection.Index != c.Before.AppliedIndex {
		return fmt.Errorf("promotion old full cut: %w", ErrMemberAuthority)
	}
	p, b, s := c.Proposal, c.Before, c.Seal
	if p.ClusterID != b.ClusterID || p.Incarnation != b.Incarnation || p.CandidateNodeID != b.NodeID || p.ExpectedTerm != b.Term || p.ExpectedAppliedIndex != b.AppliedIndex || p.Term != b.PromisedTerm+1 || p.Term == 0 || b.Role != RoleReplica && !(p.Self && b.Role == RolePrimary) {
		return fmt.Errorf("promotion candidate proposal differs: %w", ErrMemberAuthority)
	}
	role := authenticatedstate.SealReplica
	if b.Role == RolePrimary {
		role = authenticatedstate.SealPrimary
	}
	incarnation := sha256.Sum256([]byte(b.Incarnation))
	if s.Role != role || s.Term != b.Term || s.OrderingTerm != b.Term || s.OrderingIndex != b.AppliedIndex || s.OrderingMAC != b.OrderingHeadMAC || s.Cluster != sha256.Sum256([]byte(b.ClusterID)) || s.Member != sha256.Sum256([]byte(b.NodeID)) || !bytes.Equal(s.Incarnation[:], incarnation[:16]) || s.RootCount != 7 || s.PositionCount != 5 || s.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return fmt.Errorf("promotion old selected seal differs: %w", ErrMemberAuthority)
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
		if generation != c.Cursors[slot].Generation || position.Sequence != c.Cursors[slot].Sequence || position.Hash != c.Heads[slot] {
			return fmt.Errorf("promotion source slot %d differs (generation %d/%d sequence %d/%d): %w", slot, generation, c.Cursors[slot].Generation, position.Sequence, c.Cursors[slot].Sequence, ErrMemberAuthority)
		}
	}
	if len(c.Promises) > 2 {
		return ErrMemberAuthority
	}
	for i, p := range c.Promises {
		if i > 0 && c.Promises[i-1].NodeID >= p.NodeID {
			return ErrMemberAuthority
		}
	}
	if c.Fulfilled {
		// The real Promise converts an explicit self Primary to Replica as well.
		candidate := cloneMemberState(b)
		candidate.Role = RoleReplica
		candidate.PromisedTerm = p.Term
		if err := ValidatePromotionPromises(candidate, p, c.Promises, c.NoPeerPromise); err != nil {
			return err
		}
	} else if len(c.Promises) != 0 {
		return ErrMemberAuthority
	}
	return nil
}

func SignHAPromotionCarrier(c HAPromotionClaims, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrMemberAuthority
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(promotionEnvelope(c))
	if err != nil {
		return nil, err
	}
	if len(body)+32 > MaxHAPromotionCarrierBytes {
		return nil, fmt.Errorf("promotion carrier exceeds fixed bound (bytes=%d maximum=%d): %w", len(body)+32, MaxHAPromotionCarrierBytes, ErrMemberAuthority)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("halro:cluster:ha-promotion:v1\x00"))
	h.Write(body)
	return append(body, h.Sum(nil)...), nil
}

func QualifyHAPromotionCarrier(encoded, key []byte) (*HAPromotionAuthority, error) {
	if len(key) != 32 || len(encoded) < 33 || len(encoded) > MaxHAPromotionCarrierBytes {
		return nil, ErrMemberAuthority
	}
	body, tag := encoded[:len(encoded)-32], encoded[len(encoded)-32:]
	h := hmac.New(sha256.New, key)
	h.Write([]byte("halro:cluster:ha-promotion:v1\x00"))
	h.Write(body)
	if !hmac.Equal(tag, h.Sum(nil)) {
		return nil, ErrMemberAuthority
	}
	var envelope haPromotionEnvelope
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&envelope); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, ErrMemberAuthority
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, body) {
		return nil, ErrMemberAuthority
	}
	if len(envelope.Seal.Roots) != 7 || len(envelope.Seal.Positions) != 5 {
		return nil, ErrMemberAuthority
	}
	c := envelope.Claims
	c.Seal = authenticatedstate.CandidateSeal(envelope.Seal.haPromotionSealHeader)
	copy(c.Seal.Roots[:], envelope.Seal.Roots)
	copy(c.Seal.Positions[:], envelope.Seal.Positions)
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &HAPromotionAuthority{claims: c, digest: sha256.Sum256(encoded)}, nil
}
func (a *HAPromotionAuthority) Claims() HAPromotionClaims {
	if a == nil {
		return HAPromotionClaims{}
	}
	c := a.claims
	c.Before = cloneMemberState(c.Before)
	c.Promises = append([]PromotionPromise(nil), c.Promises...)
	return c
}
func (a *HAPromotionAuthority) Digest() [32]byte {
	if a == nil {
		return [32]byte{}
	}
	return a.digest
}

// ValidateRecoveryMember grants only continuation of a stopped command. The
// old term is never accepted by ordinary selected source/read/write factories.
func (a *HAPromotionAuthority) ValidateRecoveryMember(member MemberState) error {
	if a == nil || member.Validate() != nil {
		return ErrMemberAuthority
	}
	b, p := a.claims.Before, a.claims.Proposal
	if member.ClusterID != b.ClusterID || member.Incarnation != b.Incarnation || member.NodeID != b.NodeID || !slices.Equal(member.Peers, b.Peers) || member.Projection != b.Projection || member.AppliedIndex != b.AppliedIndex || member.ConfirmedIndex != b.ConfirmedIndex {
		return ErrMemberAuthority
	}
	if member.Term == b.Term && (member.PromisedTerm == b.PromisedTerm && member.Role == b.Role || member.PromisedTerm == p.Term && member.Role == RoleReplica) && member.DurableIndex == b.DurableIndex && member.OrderingHeadMAC == b.OrderingHeadMAC {
		return nil
	}
	if a.claims.Fulfilled && member.Term == p.Term && member.PromisedTerm == p.Term && member.Role == RolePrimary && member.DurableIndex >= b.DurableIndex {
		return nil
	}
	return ErrMemberAuthority
}

// ValidateOrdering requires the original MACed cut and the actual new-term
// leadership frame. A signed proposal or a durable Promise alone cannot pass.
func (a *HAPromotionAuthority) ValidateOrdering(j *OrderingJournal) error {
	if a == nil || !a.claims.Fulfilled || j == nil {
		return ErrMemberAuthority
	}
	c := a.claims
	old, err := j.Record(c.Before.AppliedIndex)
	if err != nil {
		return err
	}
	if old.MAC != c.Before.OrderingHeadMAC || old.Term != c.Before.Term {
		return ErrMemberAuthority
	}
	cursors, err := j.StoreCursorsThrough(c.Before.AppliedIndex)
	if err != nil || cursors != c.Cursors {
		return errors.Join(ErrMemberAuthority, err)
	}
	anchor, err := j.Record(c.Before.AppliedIndex + 1)
	if err != nil {
		return err
	}
	if anchor.Kind != KindLeadershipEstablished || anchor.Store != StoreNone || anchor.Term != c.Proposal.Term || anchor.PreviousMAC != old.MAC || anchor.ConfirmedIndex != c.Before.ConfirmedIndex {
		return ErrMemberAuthority
	}
	return nil
}

func (a *HAPromotionAuthority) validateBaseline(c MetadataBaselineClaims, digest [32]byte) error {
	if a == nil || a.claims.BaselineDigest != digest || a.claims.Before.ClusterID != c.ClusterID || a.claims.Before.Incarnation != c.Incarnation || a.claims.Before.Term != c.Term || a.claims.Before.AppliedIndex < c.Index+1 || a.claims.Proposal.OldPrimaryNodeID != c.Primary {
		return ErrMemberAuthority
	}
	members := []string{a.claims.Before.NodeID}
	for _, p := range a.claims.Before.Peers {
		members = append(members, p.Name)
	}
	sort.Strings(members)
	if !slices.Equal(members, c.Members) {
		return ErrMemberAuthority
	}
	return nil
}

func (a *HAPromotionAuthority) MatchesPreparation(prepared *HAPromotionAuthority) bool {
	if a == nil || prepared == nil {
		return false
	}
	x, y := a.claims, prepared.claims
	xb, yb := x.Before, y.Before
	return x.Version == y.Version && x.OccurredAtMicros == y.OccurredAtMicros && x.BaselineDigest == y.BaselineDigest && x.Seal == y.Seal && x.Cursors == y.Cursors && x.Heads == y.Heads && x.Proposal == y.Proposal && x.NoPeerPromise == y.NoPeerPromise && xb.Version == yb.Version && xb.ClusterID == yb.ClusterID && xb.Incarnation == yb.Incarnation && xb.NodeID == yb.NodeID && xb.Role == yb.Role && xb.Term == yb.Term && xb.PromisedTerm == yb.PromisedTerm && xb.DurableIndex == yb.DurableIndex && xb.ConfirmedIndex == yb.ConfirmedIndex && xb.AppliedIndex == yb.AppliedIndex && xb.OrderingHeadMAC == yb.OrderingHeadMAC && xb.Projection == yb.Projection && slices.Equal(xb.Peers, yb.Peers)
}
