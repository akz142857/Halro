package replication

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	StateVersion       = 1
	MaxMemberStateJSON = 64 << 10
)

var stateDomain = []byte("halro:cluster:v1\x00state\x00")

type Role string

const (
	RolePrimary          Role = "primary"
	RoleReplica          Role = "replica"
	RoleAwaitingDecision Role = "awaiting_decision"
)

type StatePeer struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	SPKISHA256 string `json:"spki_sha256"`
}

type ProjectionState struct {
	Index            uint64 `json:"index"`
	MetadataEpoch    uint64 `json:"metadata_epoch"`
	MetadataSequence uint64 `json:"metadata_sequence"`
}

type MemberState struct {
	Version         int
	ClusterID       string
	Incarnation     string
	NodeID          string
	Role            Role
	Term            uint64
	PromisedTerm    uint64
	DurableIndex    uint64
	AppliedIndex    uint64
	OrderingHeadMAC [sha256.Size]byte
	Projection      ProjectionState
	Peers           []StatePeer
}

type memberStateJSON struct {
	Version         int             `json:"version"`
	ClusterID       string          `json:"cluster_id"`
	Incarnation     string          `json:"incarnation"`
	NodeID          string          `json:"node_id"`
	Role            Role            `json:"role"`
	Term            uint64          `json:"term"`
	PromisedTerm    uint64          `json:"promised_term"`
	DurableIndex    uint64          `json:"durable_index"`
	AppliedIndex    uint64          `json:"applied_index"`
	OrderingHeadMAC string          `json:"ordering_head_mac"`
	Projection      ProjectionState `json:"projection"`
	Peers           []StatePeer     `json:"peers"`
	MAC             string          `json:"mac"`
}

func (s MemberState) Validate() error {
	if s.Version != StateVersion {
		return fmt.Errorf("unsupported member-state version %d", s.Version)
	}
	for name, value := range map[string]string{"cluster_id": s.ClusterID, "incarnation": s.Incarnation, "node_id": s.NodeID} {
		if len(value) == 0 || len(value) > MaxIdentityBytes {
			return fmt.Errorf("member-state %s must be 1-%d bytes", name, MaxIdentityBytes)
		}
	}
	switch s.Role {
	case RolePrimary, RoleReplica:
		if s.Term == 0 {
			return errors.New("primary or replica member-state requires a positive term")
		}
	case RoleAwaitingDecision:
	default:
		return fmt.Errorf("unknown member-state role %q", s.Role)
	}
	if s.PromisedTerm < s.Term {
		return errors.New("member-state promised_term cannot be lower than term")
	}
	if s.AppliedIndex > s.DurableIndex {
		return errors.New("member-state applied_index cannot exceed durable_index")
	}
	if s.Projection.Index > s.AppliedIndex {
		return errors.New("member-state projection index cannot exceed applied_index")
	}
	if s.DurableIndex > 0 && s.OrderingHeadMAC == ([sha256.Size]byte{}) {
		return errors.New("member-state with a durable index requires an ordering head MAC")
	}
	if s.DurableIndex == 0 && s.OrderingHeadMAC != ([sha256.Size]byte{}) {
		return errors.New("member-state without a durable index cannot name an ordering head MAC")
	}
	if len(s.Peers) < 1 || len(s.Peers) > 2 {
		return errors.New("member-state must snapshot one or two peers")
	}
	seenNames := make(map[string]struct{}, len(s.Peers))
	seenAddresses := make(map[string]struct{}, len(s.Peers))
	seenPins := make(map[string]struct{}, len(s.Peers))
	for _, peer := range s.Peers {
		if len(peer.Name) == 0 || len(peer.Name) > MaxIdentityBytes || peer.Name == s.NodeID || peer.Address == "" || !validStatePin(peer.SPKISHA256) {
			return errors.New("member-state peer identity, address or SPKI pin is invalid")
		}
		if _, ok := seenNames[peer.Name]; ok {
			return errors.New("member-state peer name is duplicated")
		}
		if _, ok := seenAddresses[peer.Address]; ok {
			return errors.New("member-state peer address is duplicated")
		}
		if _, ok := seenPins[peer.SPKISHA256]; ok {
			return errors.New("member-state peer SPKI pin is duplicated")
		}
		seenNames[peer.Name] = struct{}{}
		seenAddresses[peer.Address] = struct{}{}
		seenPins[peer.SPKISHA256] = struct{}{}
	}
	return nil
}

func MarshalState(state MemberState, key []byte) ([]byte, error) {
	if len(key) != sha256.Size {
		return nil, errors.New("member-state key must be 32 bytes")
	}
	state.Peers = append([]StatePeer(nil), state.Peers...)
	sort.Slice(state.Peers, func(i, j int) bool { return state.Peers[i].Name < state.Peers[j].Name })
	if err := state.Validate(); err != nil {
		return nil, err
	}
	mac, err := memberStateMAC(state, key)
	if err != nil {
		return nil, err
	}
	payload := memberStateJSON{
		Version: state.Version, ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: state.NodeID,
		Role: state.Role, Term: state.Term, PromisedTerm: state.PromisedTerm,
		DurableIndex: state.DurableIndex, AppliedIndex: state.AppliedIndex,
		OrderingHeadMAC: digestText(state.OrderingHeadMAC), Projection: state.Projection, Peers: state.Peers,
		MAC: digestText(mac),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode member-state JSON: %w", err)
	}
	if len(encoded) > MaxMemberStateJSON {
		return nil, errors.New("member-state JSON exceeds its size bound")
	}
	return encoded, nil
}

func UnmarshalState(encoded, key []byte) (MemberState, error) {
	if len(key) != sha256.Size {
		return MemberState{}, errors.New("member-state key must be 32 bytes")
	}
	if len(encoded) == 0 || len(encoded) > MaxMemberStateJSON {
		return MemberState{}, errors.New("member-state JSON is empty or exceeds its size bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var payload memberStateJSON
	if err := decoder.Decode(&payload); err != nil {
		return MemberState{}, fmt.Errorf("decode member-state JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return MemberState{}, errors.New("member-state JSON has trailing content")
	}
	orderingHead, err := parseDigestText(payload.OrderingHeadMAC)
	if err != nil {
		return MemberState{}, fmt.Errorf("ordering_head_mac: %w", err)
	}
	providedMAC, err := parseDigestText(payload.MAC)
	if err != nil {
		return MemberState{}, fmt.Errorf("mac: %w", err)
	}
	state := MemberState{
		Version: payload.Version, ClusterID: payload.ClusterID, Incarnation: payload.Incarnation, NodeID: payload.NodeID,
		Role: payload.Role, Term: payload.Term, PromisedTerm: payload.PromisedTerm,
		DurableIndex: payload.DurableIndex, AppliedIndex: payload.AppliedIndex,
		OrderingHeadMAC: orderingHead, Projection: payload.Projection, Peers: append([]StatePeer(nil), payload.Peers...),
	}
	if !sort.SliceIsSorted(state.Peers, func(i, j int) bool { return state.Peers[i].Name < state.Peers[j].Name }) {
		return MemberState{}, errors.New("member-state peers are not in canonical name order")
	}
	if err := state.Validate(); err != nil {
		return MemberState{}, err
	}
	expectedMAC, err := memberStateMAC(state, key)
	if err != nil {
		return MemberState{}, err
	}
	if !hmac.Equal(providedMAC[:], expectedMAC[:]) {
		return MemberState{}, errors.New("member-state MAC mismatch")
	}
	return state, nil
}

func memberStateMAC(state MemberState, key []byte) ([sha256.Size]byte, error) {
	input, err := canonicalStateInput(state)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(stateDomain)
	mac.Write(input)
	var sum [sha256.Size]byte
	copy(sum[:], mac.Sum(nil))
	return sum, nil
}

func canonicalStateInput(state MemberState) ([]byte, error) {
	var buffer bytes.Buffer
	writeUint16 := func(value uint16) { _ = binary.Write(&buffer, binary.BigEndian, value) }
	writeUint64 := func(value uint64) { _ = binary.Write(&buffer, binary.BigEndian, value) }
	writeString := func(value string) error {
		if len(value) > 65535 {
			return errors.New("member-state string exceeds canonical encoding")
		}
		writeUint16(uint16(len(value)))
		buffer.WriteString(value)
		return nil
	}
	writeUint16(uint16(state.Version))
	for _, value := range []string{state.ClusterID, state.Incarnation, state.NodeID, string(state.Role)} {
		if err := writeString(value); err != nil {
			return nil, err
		}
	}
	for _, value := range []uint64{state.Term, state.PromisedTerm, state.DurableIndex, state.AppliedIndex} {
		writeUint64(value)
	}
	buffer.Write(state.OrderingHeadMAC[:])
	for _, value := range []uint64{state.Projection.Index, state.Projection.MetadataEpoch, state.Projection.MetadataSequence} {
		writeUint64(value)
	}
	writeUint16(uint16(len(state.Peers)))
	for _, peer := range state.Peers {
		for _, value := range []string{peer.Name, peer.Address, peer.SPKISHA256} {
			if err := writeString(value); err != nil {
				return nil, err
			}
		}
	}
	return buffer.Bytes(), nil
}

func digestText(value [sha256.Size]byte) string {
	return "sha256:" + hex.EncodeToString(value[:])
}

func parseDigestText(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") || value != strings.ToLower(value) {
		return digest, errors.New("must be sha256: followed by 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	if err != nil {
		return digest, errors.New("must be sha256: followed by 64 lowercase hexadecimal characters")
	}
	copy(digest[:], decoded)
	return digest, nil
}

func validStatePin(value string) bool {
	_, err := parseDigestText(value)
	return err == nil
}
