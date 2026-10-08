package replication

// QualifyMetadataBaselineForPeerRecovery authenticates the original group
// carrier against the peer's own signed old Member and current real Promise.
// It retains old bindings only; ValidateMember still refuses the in-progress
// promise as ordinary authority.
func QualifyMetadataBaselineForPeerRecovery(encoded, key []byte, member MemberState, peer *HAPromotionPeerAuthority) (*MetadataBaselineAuthority, error) {
	if peer == nil || peer.ValidateRecoveryMember(member) != nil {
		return nil, ErrMemberAuthority
	}
	c := peer.Claims()
	a, err := QualifyMetadataBaselineCarrier(encoded, key, c.Before)
	if err != nil {
		return nil, err
	}
	if c.BaselineDigest != a.Descriptor().AuthorityDigest || c.Before.AppliedIndex < a.Claims().Index+1 || c.Proposal.OldPrimaryNodeID != a.Claims().Primary {
		return nil, ErrMemberAuthority
	}
	return a, nil
}
