package replication

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// RequestPromotionPromise uses a fresh, one-shot authenticated control
// session. The caller is expected to be an offline candidate holding its data
// directory lock; it advertises awaiting_decision so the peer never mistakes
// this connection for a replication data stream.
func RequestPromotionPromise(
	ctx context.Context,
	tlsFiles TLSFiles,
	clusterKey []byte,
	local Hello,
	peer PeerEndpoint,
	proposal PromotionProposal,
) (PromotionPromise, error) {
	if local.Role != RoleAwaitingDecision || local.NodeID != proposal.CandidateNodeID {
		return PromotionPromise{}, errors.New("promotion control hello must identify the awaiting candidate")
	}
	clientBase, err := loadTLSConfig(tlsFiles)
	if err != nil {
		return PromotionPromise{}, err
	}
	if peer.ServerName == "" {
		host, _, splitErr := net.SplitHostPort(peer.Address)
		if splitErr != nil || host == "" {
			return PromotionPromise{}, errors.New("promotion peer address cannot supply a TLS server name")
		}
		peer.ServerName = host
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", peer.Address)
	if err != nil {
		return PromotionPromise{}, fmt.Errorf("dial promotion peer %s: %w", peer.NodeID, err)
	}
	clientTLS := clientBase.Clone()
	clientTLS.ServerName = peer.ServerName
	session, err := DialPeerSession(ctx, raw, clientTLS, local, clusterKey, peer.NodeID, peer.SPKISHA256, NewNonceCache(8), PeerSessionOptions{})
	if err != nil {
		return PromotionPromise{}, err
	}
	encoded, err := proposal.MarshalBinary()
	if err != nil {
		_ = session.Close()
		return PromotionPromise{}, err
	}
	record, err := session.ExchangeControl(ctx, encoded)
	if err != nil {
		return PromotionPromise{}, err
	}
	if record.Kind != StreamRecordPromotionPromise {
		return PromotionPromise{}, errors.New("promotion peer returned a non-promise record")
	}
	promise, err := UnmarshalPromotionPromise(record.Encoded)
	if err != nil {
		return PromotionPromise{}, err
	}
	if promise.NodeID != peer.NodeID {
		return PromotionPromise{}, errors.New("promotion promise node does not match the authenticated peer")
	}
	return promise, nil
}
