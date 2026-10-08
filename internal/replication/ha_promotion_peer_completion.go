package replication

import (
	"errors"
	"slices"
)

// HAPromotionPeerCompletion pairs two independently authenticated carriers:
// this peer's own durable promise and the candidate's fulfilled prepare. The
// pair still grants no root/ordinary role. Actual Leadership receipt, original
// Receiver confirmation/application and whole-root publication remain required.
type HAPromotionPeerCompletion struct {
	peer      *HAPromotionPeerAuthority
	candidate *HAPromotionAuthority
}

func QualifyHAPromotionPeerCompletion(peer *HAPromotionPeerAuthority, candidate *HAPromotionAuthority) (*HAPromotionPeerCompletion, error) {
	if peer == nil || candidate == nil {
		return nil, ErrMemberAuthority
	}
	p, c := peer.Claims(), candidate.Claims()
	if p.Promise == nil || !c.Fulfilled || c.NoPeerPromise || c.Proposal != p.Proposal || c.BaselineDigest != p.BaselineDigest || c.Before.NodeID != p.Proposal.CandidateNodeID || c.Before.ClusterID != p.Before.ClusterID || c.Before.Incarnation != p.Before.Incarnation || c.Before.Role != RoleReplica || c.Before.Term != p.Before.Term || c.Before.PromisedTerm != p.Before.PromisedTerm || c.Before.DurableIndex != p.Before.DurableIndex || c.Before.ConfirmedIndex != p.Before.ConfirmedIndex || c.Before.AppliedIndex != p.Before.AppliedIndex || c.Before.OrderingHeadMAC != p.Before.OrderingHeadMAC || c.Cursors != p.Cursors || c.Heads != p.Heads {
		return nil, ErrMemberAuthority
	}
	matched := false
	for _, promise := range c.Promises {
		if promise.NodeID == p.Before.NodeID {
			if promise != *p.Promise {
				return nil, ErrMemberAuthority
			}
			matched = true
		}
	}
	if !matched {
		return nil, ErrMemberAuthority
	}
	return &HAPromotionPeerCompletion{peer: peer, candidate: candidate}, nil
}

func (a *HAPromotionPeerCompletion) Peer() *HAPromotionPeerAuthority {
	if a == nil {
		return nil
	}
	return a.peer
}
func (a *HAPromotionPeerCompletion) Candidate() *HAPromotionAuthority {
	if a == nil {
		return nil
	}
	return a.candidate
}

// ValidateLeadershipMember is a transition-only boundary, deliberately limited
// to the single new structural frame. It never permits a native semantic suffix
// or treats the candidate's seven roots as this peer's roots.
func (a *HAPromotionPeerCompletion) ValidateLeadershipMember(member MemberState, journal *OrderingJournal) error {
	if a == nil || a.peer == nil || a.candidate == nil || member.Validate() != nil || journal == nil {
		return ErrMemberAuthority
	}
	p := a.peer.Claims()
	b := p.Before
	expected := cloneMemberState(b)
	expected.Role, expected.Term, expected.PromisedTerm = RoleReplica, p.Proposal.Term, p.Proposal.Term
	expected.DurableIndex, expected.ConfirmedIndex = b.AppliedIndex+1, b.AppliedIndex+1
	if member.AppliedIndex != b.AppliedIndex && member.AppliedIndex != b.AppliedIndex+1 {
		return ErrMemberAuthority
	}
	expected.AppliedIndex, expected.Projection.Index = member.AppliedIndex, member.AppliedIndex
	if err := a.candidate.ValidateOrdering(journal); err != nil {
		return err
	}
	record, err := journal.Record(expected.DurableIndex)
	if err != nil {
		return err
	}
	expected.OrderingHeadMAC = record.MAC
	actualIndex, actualTerm, actualMAC := journal.Head()
	if actualIndex != expected.DurableIndex || actualTerm != expected.Term || actualMAC != expected.OrderingHeadMAC || !sameHAPeerLeadershipMember(member, expected) {
		return ErrMemberAuthority
	}
	cursors, err := journal.StoreCursorsThrough(expected.DurableIndex)
	if err != nil || cursors != p.Cursors {
		return errors.Join(ErrMemberAuthority, err)
	}
	return nil
}

// ValidateRecoveryMember authenticates the only three stopped states: the
// original durable Promise; adoption with no new frame; and original Receiver
// receipt/application of the single structural frame. Actual Ordering is
// required even for adoption, whose stored head still has the previous term.
func (a *HAPromotionPeerCompletion) ValidateRecoveryMember(member MemberState, journal *OrderingJournal) error {
	if a == nil || a.peer == nil || a.candidate == nil || journal == nil {
		return ErrMemberAuthority
	}
	p := a.peer.Claims()
	b := p.Before
	index, term, mac := journal.Head()
	if index == b.DurableIndex {
		if term != b.Term || mac != b.OrderingHeadMAC {
			return ErrMemberAuthority
		}
		cursors, err := journal.StoreCursorsThrough(index)
		if err != nil || cursors != p.Cursors {
			return errors.Join(ErrMemberAuthority, err)
		}
		if a.peer.ValidateRecoveryMember(member) == nil {
			return nil
		}
		expected := cloneMemberState(b)
		expected.Role, expected.Term, expected.PromisedTerm = RoleReplica, p.Proposal.Term, p.Proposal.Term
		if sameHAPeerLeadershipMember(member, expected) {
			return nil
		}
		return ErrMemberAuthority
	}
	return a.ValidateLeadershipMember(member, journal)
}

func sameHAPeerLeadershipMember(a, b MemberState) bool {
	return a.Version == b.Version && a.ClusterID == b.ClusterID && a.Incarnation == b.Incarnation && a.NodeID == b.NodeID && a.Role == b.Role && a.Term == b.Term && a.PromisedTerm == b.PromisedTerm && a.DurableIndex == b.DurableIndex && a.ConfirmedIndex == b.ConfirmedIndex && a.AppliedIndex == b.AppliedIndex && a.OrderingHeadMAC == b.OrderingHeadMAC && a.Projection == b.Projection && slices.Equal(a.Peers, b.Peers)
}
