package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

// This control producer serves only its actual fulfilled promotion and single
// structural boundary. Its original common/source/Member remain authoritative;
// a peer's signed old root is never installed as this Primary's permission.
func (f *haSelectedRuntimeFoundation) handleSelectedPeerBoundaryRequest(ctx context.Context, hello replication.Hello, encoded []byte, send func([]byte) error) error {
	if f == nil || f.replication == nil || f.reopened == nil || f.promotionRecovery != nil || f.peerPromotionRecovery != nil || f.cfg.ResourceBudget.PhysicalTerminalReserve || send == nil || hello.Role != replication.RoleAwaitingDecision {
		return authenticatedstate.ErrSealIneligible
	}
	r := f.replication
	raw, err := replication.UnmarshalHAPeerPromotionRequest(encoded)
	if err != nil {
		return err
	}
	own, err := replication.QualifyHAPromotionPeerCarrier(raw, r.clusterKey[:])
	if err != nil {
		return err
	}
	p := own.Claims()
	if p.Promise == nil || hello.NodeID != p.Before.NodeID || hello.ClusterID != p.Before.ClusterID || hello.Incarnation != p.Before.Incarnation || (hello.Term != p.Before.Term && hello.Term != p.Proposal.Term) || hello.PromisedTerm != p.Proposal.Term || hello.DurableIndex != p.Before.DurableIndex || hello.AppliedIndex != p.Before.AppliedIndex || hello.MetadataBaselineAuthority != p.BaselineDigest {
		return replication.ErrMemberAuthority
	}
	candidate, err := readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(f.cfg, true), r.clusterKey[:], f.owners.Components.Budget)
	if err != nil {
		return err
	}
	if _, err = replication.QualifyHAPromotionPeerCompletion(own, candidate); err != nil {
		return err
	}
	return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
		m := cut.ActualMember
		if m.NodeID != p.Proposal.CandidateNodeID || m.Role != replication.RolePrimary || m.Term != p.Proposal.Term || m.Term != m.PromisedTerm || cut.Seal.Term != m.Term || cut.Seal.Role != authenticatedstate.SealPrimary || cut.AllowedIndex != m.DurableIndex || f.owners.Baseline.CurrentPrimary() != m.NodeID {
			return replication.ErrMemberAuthority
		}
		if err := candidate.ValidateOrdering(r.journal); err != nil {
			return err
		}
		var response []byte
		err := replication.ReconstructRangeEach(m.ClusterID, m.Incarnation, p.Before.AppliedIndex+1, p.Before.AppliedIndex+1, r.journal, r.source, func(commit replication.LocalCommit) error {
			carrier, err := replication.SignHAPromotionCarrier(candidate.Claims(), r.clusterKey[:])
			if err != nil {
				return err
			}
			defer clear(carrier)
			response, err = replication.MarshalHAPeerPromotionBoundary(carrier, commit.Encoded)
			return err
		})
		defer clear(response)
		if err != nil || len(response) == 0 {
			return errors.Join(replication.ErrMemberAuthority, err)
		}
		return send(response)
	})
}

func (f *haSelectedRuntimeFoundation) authorizePeerBoundaryControl(ctx context.Context, hello replication.Hello) (bool, error) {
	r := f.replication
	m := r.publisher.Snapshot()
	if m.Role != replication.RolePrimary || hello.Role != replication.RoleAwaitingDecision || hello.Term > m.Term {
		return false, replication.ErrMemberAuthority
	}
	a, err := readHASelectedPromotionCarrier(ctx, haSelectedPromotionPath(f.cfg, true), r.clusterKey[:], f.owners.Components.Budget)
	if err != nil {
		return false, err
	}
	c := a.Claims()
	if !c.Fulfilled || c.NoPeerPromise || c.Proposal.CandidateNodeID != m.NodeID || c.Proposal.Term != m.Term || m.Term != m.PromisedTerm || (hello.Term != c.Before.Term && hello.Term != m.Term) || hello.PromisedTerm != m.Term || hello.ClusterID != m.ClusterID || hello.Incarnation != m.Incarnation || hello.AppliedIndex != c.Before.AppliedIndex || hello.DurableIndex != hello.AppliedIndex || hello.MetadataBaselineAuthority != c.BaselineDigest || hello.Protocol != (replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}) || hello.Metadata != hello.Protocol {
		return false, replication.ErrMemberAuthority
	}
	for _, p := range c.Promises {
		if p.NodeID == hello.NodeID && p.PeerPreparationDigest != ([32]byte{}) {
			return false, a.ValidateOrdering(r.journal)
		}
	}
	return false, replication.ErrMemberAuthority
}
