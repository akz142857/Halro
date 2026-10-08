package replication

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/akz142857/Halro/internal/metadatajournal"
)

const MaxMetadataBaselineCarrierBytes = 8192

// MetadataBaselineClaims is issued by the stopped Primary's authenticated
// preparation. The four old native cuts and ordering MAC preserve the existing
// incarnation; New independently grants the complete format2 file permission.
// Root permission is separate and must come from the verified preparation owner.
type MetadataBaselineClaims struct {
	Version                         uint8
	ClusterID, Incarnation, Primary string
	Members                         []string
	Term, Index                     uint64
	OrderingHead                    [32]byte
	Cursors                         [4]StoreCursor
	Heads                           [4][32]byte
	New                             metadatajournal.BoundReplicationAuthority
	HeaderHash                      [32]byte
	BackupID, ManifestDigest        string
	CiphertextDigest                [32]byte
	PreparationDigest               [32]byte
}

func (c MetadataBaselineClaims) validate() error {
	if c.Version != 1 || c.Term == 0 || c.Index == 0 || c.Index == ^uint64(0) || c.OrderingHead == ([32]byte{}) || c.HeaderHash == ([32]byte{}) || c.BackupID == "" || len(c.BackupID) > 128 || len(c.ManifestDigest) > 128 || c.ManifestDigest == "" || c.CiphertextDigest == ([32]byte{}) || c.PreparationDigest == ([32]byte{}) {
		return errors.New("Metadata baseline carrier lacks authenticated old cut or backup provenance")
	}
	for _, v := range []string{c.ClusterID, c.Incarnation, c.Primary} {
		if len(v) == 0 || len(v) > MaxIdentityBytes {
			return errors.New("Metadata baseline carrier identity is invalid")
		}
	}
	if len(c.Members) < 2 || len(c.Members) > 3 || !sort.StringsAreSorted(c.Members) {
		return errors.New("Metadata baseline carrier membership is not canonical")
	}
	primary := false
	for i, v := range c.Members {
		if len(v) == 0 || len(v) > MaxIdentityBytes || i > 0 && v == c.Members[i-1] {
			return errors.New("Metadata baseline carrier membership is invalid")
		}
		primary = primary || v == c.Primary
	}
	if !primary {
		return errors.New("Metadata baseline Primary is outside membership")
	}
	if err := c.New.Validate(); err != nil {
		return err
	}
	for i := range c.Cursors {
		hasHistory := c.Cursors[i].Sequence > 0 || i == int(StoreMetadata-StoreLedger)
		if c.Cursors[i].Generation == 0 || hasHistory != (c.Heads[i] != ([32]byte{})) {
			return errors.New("Metadata baseline carrier lacks a canonical authenticated source head")
		}
	}
	old := c.Cursors[StoreMetadata-StoreLedger]
	if old.Generation == ^uint64(0) || c.New.Baseline.Epoch != old.Generation+1 {
		return errors.New("Metadata baseline epoch does not continue old Metadata")
	}
	return nil
}

// SignMetadataBaselineCarrier does not inspect candidates or confer permission
// itself. The app producer supplies facts read under the actual data-dir lock
// from MemberState, ordering, all four native sources and VerifiedArchive.
func SignMetadataBaselineCarrier(c MetadataBaselineClaims, clusterKey []byte) ([]byte, error) {
	if len(clusterKey) != 32 {
		return nil, errors.New("Metadata baseline signing key must be 32 bytes")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if len(body)+32 > MaxMetadataBaselineCarrierBytes {
		return nil, errors.New("Metadata baseline carrier exceeds fixed bound")
	}
	mac := hmac.New(sha256.New, clusterKey)
	mac.Write([]byte("halro:cluster:metadata-baseline:v1\x00"))
	mac.Write(body)
	return append(body, mac.Sum(nil)...), nil
}

// MetadataBaselineAuthority retains only authenticated fixed-size claims. It
// cannot be constructed from a candidate's header, and exposes copies only.
type MetadataBaselineAuthority struct {
	claims            MetadataBaselineClaims
	digest            [32]byte
	promotion         *HAPromotionAuthority
	promotionOrdering *OrderingJournal
}

func decodeMetadataBaselineCarrier(encoded, clusterKey []byte) (*MetadataBaselineAuthority, error) {
	if len(clusterKey) != 32 || len(encoded) < 33 || len(encoded) > MaxMetadataBaselineCarrierBytes {
		return nil, errors.New("Metadata baseline carrier length or key is invalid")
	}
	body, tag := encoded[:len(encoded)-32], encoded[len(encoded)-32:]
	mac := hmac.New(sha256.New, clusterKey)
	mac.Write([]byte("halro:cluster:metadata-baseline:v1\x00"))
	mac.Write(body)
	if !hmac.Equal(tag, mac.Sum(nil)) {
		return nil, errors.New("Metadata baseline carrier MAC is invalid")
	}
	var c MetadataBaselineClaims
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("Metadata baseline carrier contains trailing data")
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, body) {
		return nil, errors.New("Metadata baseline carrier is not canonical")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}

	c.Members = append([]string(nil), c.Members...)
	return &MetadataBaselineAuthority{claims: c, digest: sha256.Sum256(encoded)}, nil
}

// QualifyMetadataBaselineCarrier retains the original same-term contract.
func QualifyMetadataBaselineCarrier(encoded, clusterKey []byte, member MemberState) (*MetadataBaselineAuthority, error) {
	a, err := decodeMetadataBaselineCarrier(encoded, clusterKey)
	if err != nil {
		return nil, err
	}
	c := a.claims
	if err := member.Validate(); err != nil {
		return nil, err
	}
	if member.ClusterID != c.ClusterID || member.Incarnation != c.Incarnation || member.Term != c.Term || member.PromisedTerm != c.Term || member.Role != RolePrimary && member.Role != RoleReplica || member.Role == RolePrimary && member.NodeID != c.Primary || member.Role == RoleReplica && member.NodeID == c.Primary {
		return nil, errors.New("Metadata baseline carrier is outside this member role, term or incarnation")
	}
	members := []string{member.NodeID}
	for _, peer := range member.Peers {
		members = append(members, peer.Name)
	}
	sort.Strings(members)
	if len(members) != len(c.Members) {
		return nil, errors.New("Metadata baseline membership differs")
	}
	for i, v := range members {
		if v != c.Members[i] {
			return nil, errors.New("Metadata baseline membership differs")
		}
	}
	return a, nil
}

// QualifyMetadataBaselineForPromotionRecovery preserves the old source binding
// while granting only continuation of the authenticated stopped transition.
func QualifyMetadataBaselineForPromotionRecovery(encoded, key []byte, member MemberState, promotion *HAPromotionAuthority) (*MetadataBaselineAuthority, error) {
	a, err := decodeMetadataBaselineCarrier(encoded, key)
	if err != nil {
		return nil, err
	}
	if err = promotion.validateBaseline(a.claims, a.digest); err != nil {
		return nil, err
	}
	if err = promotion.ValidateRecoveryMember(member); err != nil {
		return nil, err
	}
	return a, nil
}

// QualifyMetadataBaselineAfterPromotion requires the actual authenticated
// continuous leadership frame in addition to the independent fulfilled carrier.
func QualifyMetadataBaselineAfterPromotion(encoded, key []byte, member MemberState, promotion *HAPromotionAuthority, journal *OrderingJournal) (*MetadataBaselineAuthority, error) {
	a, err := decodeMetadataBaselineCarrier(encoded, key)
	if err != nil {
		return nil, err
	}
	if err = promotion.validateBaseline(a.claims, a.digest); err != nil {
		return nil, err
	}
	if err = promotion.ValidateOrdering(journal); err != nil {
		return nil, err
	}
	a.promotion, a.promotionOrdering = promotion, journal
	if err = a.ValidateMember(member); err != nil {
		return nil, err
	}
	return a, nil
}

// ValidateMember never accepts a promise in progress as ordinary authority.
func (a *MetadataBaselineAuthority) ValidateMember(member MemberState) error {
	if a == nil || member.Validate() != nil {
		return ErrMemberAuthority
	}
	c := a.claims
	members := []string{member.NodeID}
	for _, p := range member.Peers {
		members = append(members, p.Name)
	}
	sort.Strings(members)
	if member.ClusterID != c.ClusterID || member.Incarnation != c.Incarnation || len(members) != len(c.Members) || member.Role != RolePrimary && member.Role != RoleReplica {
		return ErrMemberAuthority
	}
	for i, v := range members {
		if v != c.Members[i] {
			return ErrMemberAuthority
		}
	}
	term, primary := c.Term, c.Primary
	if a.promotion != nil {
		if err := a.promotion.ValidateOrdering(a.promotionOrdering); err != nil {
			return err
		}
		term, primary = a.promotion.claims.Proposal.Term, a.promotion.claims.Proposal.CandidateNodeID
		if member.DurableIndex < a.promotion.claims.Before.AppliedIndex+1 {
			return ErrMemberAuthority
		}
	}
	if member.Term != term || member.PromisedTerm != term || member.Role == RolePrimary && member.NodeID != primary || member.Role == RoleReplica && member.NodeID == primary {
		return ErrMemberAuthority
	}
	return nil
}
func (a *MetadataBaselineAuthority) CurrentPrimary() string {
	if a == nil {
		return ""
	}
	if a.promotion != nil {
		return a.promotion.claims.Proposal.CandidateNodeID
	}
	return a.claims.Primary
}
func (a *MetadataBaselineAuthority) CurrentTerm() uint64 {
	if a == nil {
		return 0
	}
	if a.promotion != nil {
		return a.promotion.claims.Proposal.Term
	}
	return a.claims.Term
}

func (a *MetadataBaselineAuthority) Claims() MetadataBaselineClaims {
	if a == nil {
		return MetadataBaselineClaims{}
	}
	c := a.claims
	c.Members = append([]string(nil), c.Members...)
	return c
}
func (a *MetadataBaselineAuthority) Descriptor() MetadataBaselineMetadata {
	if a == nil {
		return MetadataBaselineMetadata{}
	}
	c := a.claims
	old := c.Cursors[StoreMetadata-StoreLedger]
	return MetadataBaselineMetadata{PreviousEpoch: old.Generation, PreviousSequence: old.Sequence, PreviousHead: c.Heads[StoreMetadata-StoreLedger], Epoch: c.New.Baseline.Epoch, HeaderHash: c.HeaderHash, AuthorityDigest: a.digest}
}
func (a *MetadataBaselineAuthority) validateRecord(record OrderingRecord) error {
	if a == nil {
		return errors.New("Metadata baseline requires independent group permission")
	}
	descriptor, err := DecodeMetadataBaselineMetadata(record.Metadata)
	if err != nil {
		return err
	}
	if (record.MAC != ([32]byte{}) && record.PreviousMAC != a.claims.OrderingHead) || descriptor != a.Descriptor() || record.Kind != KindMetadataBaseline || record.Index != a.claims.Index+1 || record.Term != a.claims.Term || record.ConfirmedIndex != a.claims.Index {
		return errors.New("Metadata baseline ordering record differs from its independently authorized boundary")
	}
	return nil
}
func (a *MetadataBaselineAuthority) validateOldCut(journal *OrderingJournal) error {
	if a == nil || journal == nil {
		return errors.New("Metadata baseline requires independent group permission and ordering")
	}
	record, err := journal.Record(a.claims.Index)
	if err != nil {
		return err
	}
	if record.MAC != a.claims.OrderingHead {
		return errors.New("Metadata baseline old ordering MAC differs")
	}
	cursors, err := journal.StoreCursorsThrough(a.claims.Index)
	if err != nil {
		return err
	}
	if cursors != a.claims.Cursors {
		return fmt.Errorf("Metadata baseline old four-source cursors differ")
	}
	return nil
}
