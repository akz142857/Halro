package replication

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	PromotionProtocolVersion = 1
	MaxPromotionRecordBytes  = 4096
)

var (
	proposalMagic = [8]byte{'H', 'L', 'R', 'P', 'R', 'O', '0', '1'}
	promiseMagic  = [8]byte{'H', 'L', 'R', 'P', 'M', 'S', '0', '1'}
)

const (
	FencePodDeletedPVCRetained = "pod-deleted-pvc-retained"
	FenceNodeIsolated          = "node-isolated"
	FenceSelfStopped           = "self-stopped"
	FencePrimaryPromise        = "primary-promise"
)

// PromotionProposal is sent only after the ordinary mTLS, SPKI and Master-Key
// proof. The expectation fields bind the operator's command to the prefix they
// inspected; FencedBy records the physical-side-effect fence, not a claim that
// the replication protocol can inspect Kubernetes or a host power state.
type PromotionProposal struct {
	Version              int    `json:"version"`
	ClusterID            string `json:"cluster_id"`
	Incarnation          string `json:"incarnation"`
	CandidateNodeID      string `json:"candidate_node_id"`
	Term                 uint64 `json:"term"`
	ExpectedTerm         uint64 `json:"expected_term"`
	ExpectedAppliedIndex uint64 `json:"expected_applied_index"`
	OldPrimaryNodeID     string `json:"old_primary_node_id"`
	FencedBy             string `json:"fenced_by"`
	Self                 bool   `json:"self"`
	PlannedStepdown      bool   `json:"planned_stepdown"`
	ActorID              string `json:"actor_id,omitempty"`
}

func (p PromotionProposal) Validate() error {
	if p.Version != PromotionProtocolVersion {
		return fmt.Errorf("unsupported promotion proposal version %d", p.Version)
	}
	for name, value := range map[string]string{
		"cluster_id": p.ClusterID, "incarnation": p.Incarnation,
		"candidate_node_id": p.CandidateNodeID, "old_primary_node_id": p.OldPrimaryNodeID,
	} {
		if len(value) == 0 || len(value) > MaxIdentityBytes {
			return fmt.Errorf("promotion proposal %s must be 1-%d bytes", name, MaxIdentityBytes)
		}
	}
	if p.Self && p.CandidateNodeID != p.OldPrimaryNodeID {
		return errors.New("self promotion must name the candidate as the old Primary")
	}
	if !p.Self && p.CandidateNodeID == p.OldPrimaryNodeID {
		return errors.New("promotion candidate and old Primary must differ")
	}
	if p.PlannedStepdown && (p.Self || p.ActorID == "" || len(p.ActorID) > MaxIdentityBytes) {
		return errors.New("planned stepdown requires a bounded actor and a different target")
	}
	if p.ExpectedTerm == 0 || p.Term <= p.ExpectedTerm {
		return errors.New("promotion proposal term must be newer than expected_term")
	}
	if p.FencedBy != FencePodDeletedPVCRetained && p.FencedBy != FenceNodeIsolated &&
		!(p.Self && p.FencedBy == FenceSelfStopped) && !(p.PlannedStepdown && p.FencedBy == FencePrimaryPromise) {
		return errors.New("promotion proposal has an unsupported fencing assertion")
	}
	return nil
}

func (p PromotionProposal) MarshalBinary() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return marshalPromotionRecord(proposalMagic, p)
}

func UnmarshalPromotionProposal(encoded []byte) (PromotionProposal, error) {
	var proposal PromotionProposal
	if err := unmarshalPromotionRecord(encoded, proposalMagic, &proposal); err != nil {
		return PromotionProposal{}, err
	}
	if err := proposal.Validate(); err != nil {
		return PromotionProposal{}, err
	}
	return proposal, nil
}

// PromotionPromise is returned only after promised_term has reached stable
// storage. LastFrameTerm participates in the freshness comparison; comparing
// a bare index would select a longer stale fork.
type PromotionPromise struct {
	Version       int    `json:"version"`
	ClusterID     string `json:"cluster_id"`
	Incarnation   string `json:"incarnation"`
	NodeID        string `json:"node_id"`
	Role          Role   `json:"role"`
	PreviousRole  Role   `json:"previous_role"`
	Term          uint64 `json:"term"`
	PromisedTerm  uint64 `json:"promised_term"`
	DurableIndex  uint64 `json:"durable_index"`
	AppliedIndex  uint64 `json:"applied_index"`
	LastFrameTerm uint64 `json:"last_frame_term"`
}

func (p PromotionPromise) Validate() error {
	if p.Version != PromotionProtocolVersion {
		return fmt.Errorf("unsupported promotion promise version %d", p.Version)
	}
	for name, value := range map[string]string{"cluster_id": p.ClusterID, "incarnation": p.Incarnation, "node_id": p.NodeID} {
		if len(value) == 0 || len(value) > MaxIdentityBytes {
			return fmt.Errorf("promotion promise %s must be 1-%d bytes", name, MaxIdentityBytes)
		}
	}
	if (p.Role != RolePrimary && p.Role != RoleReplica) || (p.PreviousRole != RolePrimary && p.PreviousRole != RoleReplica) {
		return errors.New("promotion promise role is invalid")
	}
	if p.Term == 0 || p.PromisedTerm < p.Term || p.AppliedIndex > p.DurableIndex || p.LastFrameTerm > p.Term {
		return errors.New("promotion promise term or progress is invalid")
	}
	return nil
}

func (p PromotionPromise) MarshalBinary() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return marshalPromotionRecord(promiseMagic, p)
}

func UnmarshalPromotionPromise(encoded []byte) (PromotionPromise, error) {
	var promise PromotionPromise
	if err := unmarshalPromotionRecord(encoded, promiseMagic, &promise); err != nil {
		return PromotionPromise{}, err
	}
	if err := promise.Validate(); err != nil {
		return PromotionPromise{}, err
	}
	return promise, nil
}

func marshalPromotionRecord(magic [8]byte, value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload)+len(magic) > MaxPromotionRecordBytes {
		return nil, errors.New("promotion record exceeds its size bound")
	}
	encoded := make([]byte, 4+len(magic)+len(payload))
	binary.BigEndian.PutUint32(encoded[:4], uint32(len(encoded)-4))
	copy(encoded[4:12], magic[:])
	copy(encoded[12:], payload)
	return encoded, nil
}

func unmarshalPromotionRecord(encoded []byte, magic [8]byte, value any) error {
	if len(encoded) <= streamPrefixBytes || len(encoded)-4 > MaxPromotionRecordBytes || int(binary.BigEndian.Uint32(encoded[:4])) != len(encoded)-4 {
		return errors.New("promotion record is truncated or exceeds its size bound")
	}
	if !bytes.Equal(encoded[4:12], magic[:]) {
		return errors.New("promotion record magic is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded[12:]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode promotion record: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("promotion record has trailing content")
	}
	return nil
}

// ValidatePromotionPromises enforces the safety part of prepare after all
// reachable responses are collected. A three-member cluster needs one remote
// promise because the candidate's own durable promise is the second vote.
func ValidatePromotionPromises(candidate MemberState, proposal PromotionProposal, promises []PromotionPromise, allowNoPeerPromise bool) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	if err := proposal.Validate(); err != nil {
		return err
	}
	if candidate.Role != RoleReplica || candidate.NodeID != proposal.CandidateNodeID ||
		candidate.ClusterID != proposal.ClusterID || candidate.Incarnation != proposal.Incarnation ||
		candidate.Term != proposal.ExpectedTerm || candidate.AppliedIndex != proposal.ExpectedAppliedIndex {
		return errors.New("promotion proposal does not match the candidate's authenticated state")
	}
	if allowNoPeerPromise {
		if len(candidate.Peers) != 1 || len(promises) != 0 {
			return errors.New("--no-peer-promise is valid only for an unreachable two-member cluster")
		}
		return nil
	}
	if len(promises) == 0 {
		return errors.New("promotion requires at least one durable peer promise")
	}
	seen := make(map[string]struct{}, len(promises))
	plannedPrimaryPromised := false
	for _, promise := range promises {
		if err := promise.Validate(); err != nil {
			return err
		}
		if promise.ClusterID != candidate.ClusterID || promise.Incarnation != candidate.Incarnation || promise.PromisedTerm != proposal.Term {
			return errors.New("promotion promise does not match the proposal")
		}
		if promise.Role == RolePrimary {
			return errors.New("a promotion promise responder still reports Primary")
		}
		if proposal.PlannedStepdown && promise.NodeID == proposal.OldPrimaryNodeID && promise.PreviousRole == RolePrimary {
			plannedPrimaryPromised = true
		}
		if _, duplicate := seen[promise.NodeID]; duplicate {
			return errors.New("duplicate promotion promise responder")
		}
		seen[promise.NodeID] = struct{}{}
		if promise.LastFrameTerm > proposal.ExpectedTerm ||
			promise.LastFrameTerm == proposal.ExpectedTerm && promise.DurableIndex > candidate.AppliedIndex {
			return errors.New("candidate is behind a promised peer's durable prefix")
		}
	}
	if proposal.PlannedStepdown && !plannedPrimaryPromised {
		return errors.New("planned stepdown requires a durable promise from the named old Primary")
	}
	return nil
}
