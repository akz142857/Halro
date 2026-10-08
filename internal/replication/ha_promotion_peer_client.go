package replication

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithHAPeerPromotionBoundary owns the bounded one-shot response through its
// real consumer. The authenticated socket is joined before its workspace exits;
// an escaped response buffer is never a permission or a receive/apply owner.
func WithHAPeerPromotionBoundary(ctx context.Context, files TLSFiles, key []byte, local Hello, endpoint PeerEndpoint, own *HAPromotionPeerAuthority, manager *resourcebudget.Manager, visit func(context.Context, *HAPromotionPeerCompletion, []byte) error) (resultErr error) {
	if own == nil || manager == nil || visit == nil || local.Role != RoleAwaitingDecision || len(key) != 32 {
		return ErrMemberAuthority
	}
	p := own.Claims()
	if p.Promise == nil || endpoint.NodeID != p.Proposal.CandidateNodeID || local.NodeID != p.Before.NodeID || local.ClusterID != p.Before.ClusterID || local.Incarnation != p.Before.Incarnation || (local.Term != p.Before.Term && local.Term != p.Proposal.Term) || local.PromisedTerm != p.Proposal.Term || local.DurableIndex != p.Before.DurableIndex || local.AppliedIndex != p.Before.AppliedIndex || local.MetadataBaselineAuthority != p.BaselineDigest || local.Protocol != (VersionRange{Current: 2, Minimum: 2, Maximum: 2}) || local.Metadata != local.Protocol {
		return ErrMemberAuthority
	}
	work, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 512 << 10})
	if err != nil {
		return err
	}
	var session *PeerSession
	defer func() {
		if session != nil {
			if err := session.Close(); err != nil {
				resultErr = &HAPeerPromotionClientFailure{Session: session, Work: work, Cause: errors.Join(resultErr, ownedfile.ErrRetirement, err)}
				return
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &HAPeerPromotionClientFailure{Session: session, Work: work, Cause: resultErr}
			return
		}
		if err := work.Release(); err != nil {
			resultErr = &HAPeerPromotionClientFailure{Session: session, Work: work, Cause: errors.Join(resultErr, ownedfile.ErrRetirement, err)}
		}
	}()
	base, err := loadTLSConfig(files)
	if err != nil {
		return err
	}
	if endpoint.ServerName == "" {
		endpoint.ServerName, _, err = net.SplitHostPort(endpoint.Address)
		if err != nil || endpoint.ServerName == "" {
			return ErrMemberAuthority
		}
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Address)
	if err != nil {
		return fmt.Errorf("dial peer transition: %w", err)
	}
	client := base.Clone()
	client.ServerName = endpoint.ServerName
	session, err = DialPeerSession(ctx, raw, client, local, key, endpoint.NodeID, endpoint.SPKISHA256, NewNonceCache(8), PeerSessionOptions{})
	if err != nil {
		return err
	}
	remote := session.Peer()
	if remote.Role != RolePrimary || remote.Term != p.Proposal.Term || remote.PromisedTerm != remote.Term || remote.MetadataBaselineAuthority != p.BaselineDigest {
		return ErrMemberAuthority
	}
	carrier, err := SignHAPromotionPeerCarrier(p, key)
	if err != nil {
		return err
	}
	request, err := MarshalHAPeerPromotionRequest(carrier)
	clear(carrier)
	if err != nil {
		return err
	}
	record, err := session.ExchangeControl(ctx, request)
	clear(request)
	if err != nil {
		return err
	}
	defer clear(record.Encoded)
	if record.Kind != StreamRecordHAPeerPromotionBoundary {
		return ErrMemberAuthority
	}
	signed, leadership, err := UnmarshalHAPeerPromotionBoundary(record.Encoded)
	if err != nil {
		return err
	}
	candidate, err := QualifyHAPromotionCarrier(signed, key)
	if err != nil {
		return err
	}
	completion, err := QualifyHAPromotionPeerCompletion(own, candidate)
	if err != nil {
		return err
	}
	f, err := UnmarshalFrame(leadership)
	if err != nil || f.ClusterID != p.Before.ClusterID || f.Incarnation != p.Before.Incarnation || f.Index != p.Before.AppliedIndex+1 || f.Term != p.Proposal.Term || f.ConfirmedIndex != p.Before.ConfirmedIndex {
		return ErrMemberAuthority
	}
	return errors.Join(visit(ctx, completion, leadership), ctx.Err())
}

type HAPeerPromotionClientFailure struct {
	Session *PeerSession
	Work    resourcebudget.Permit
	Cause   error
}

func (e *HAPeerPromotionClientFailure) Error() string { return e.Cause.Error() }
func (e *HAPeerPromotionClientFailure) Unwrap() error { return e.Cause }
