package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

// Fixed protocol vectors use their canonical bounded encoding, rather than
// JSON's expansion of all unused root-array slots. A seal here records the
// captured observation only; even a correctly MACed copy grants no new source,
// Member, role or publication permission.
type haGroupRotationSourceWire struct {
	Member             replication.MemberState
	Selected, Observed []byte
	BaselineDigest     [32]byte
	Cursors            [4]replication.StoreCursor
	Heads              [4][32]byte
}
type haGroupRotationMemberWire struct {
	DataDir, KeyFile string
	Source           haGroupRotationSourceWire
	Bootstrap        haMemoryStateBootstrap
	VaultVersion     uint64
	SourceUnlock     []byte
	SourceHistory    []byte
	History          [haGroupRotationIntentSlots]haGroupHistoricalKey
}
type haGroupRotationIntentWire struct {
	Mode                                        string
	TargetDescriptor                            *masterkey.KeySlotDescriptor
	TargetVaultCheck                            []byte
	Version, Count                              uint8
	OperationID, OldFingerprint, NewFingerprint string
	OldKey, NewKey                              []byte
	ClusterKey                                  [32]byte
	Members                                     [3]haGroupRotationMemberWire
}

func (i haGroupRotationIntent) MarshalJSON() ([]byte, error) {
	w := haGroupRotationIntentWire{Mode: i.Mode, TargetDescriptor: i.TargetDescriptor, TargetVaultCheck: i.TargetVaultCheck, Version: i.Version, Count: i.Count, OperationID: i.OperationID, OldFingerprint: i.OldFingerprint, NewFingerprint: i.NewFingerprint, OldKey: i.OldKey, NewKey: i.NewKey, ClusterKey: i.ClusterKey}
	if i.Count > 3 {
		return nil, authenticatedstate.ErrLimit
	}
	for n := 0; n < int(i.Count); n++ {
		e := i.Members[n]
		s := haGroupRotationSourceWire{Member: e.Source.Member, BaselineDigest: e.Source.BaselineDigest, Cursors: e.Source.Cursors, Heads: e.Source.Heads}
		var err error
		s.Selected, err = authenticatedstate.EncodeSealCandidate(e.Source.Selected, i.ClusterKey[:], statecut.HardLimits())
		if err != nil {
			return nil, err
		}
		s.Observed, err = authenticatedstate.EncodeSealCandidate(e.Source.Observed, i.ClusterKey[:], statecut.HardLimits())
		if err != nil {
			return nil, err
		}
		w.Members[n] = haGroupRotationMemberWire{DataDir: e.DataDir, KeyFile: e.KeyFile, Source: s, Bootstrap: e.Bootstrap, VaultVersion: e.VaultVersion, SourceUnlock: e.SourceUnlock, SourceHistory: e.SourceHistory, History: e.History}
	}
	return json.Marshal(w)
}

func (i *haGroupRotationIntent) UnmarshalJSON(raw []byte) error {
	var w haGroupRotationIntentWire
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&w); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return authenticatedstate.ErrCorrupt
	}
	*i = haGroupRotationIntent{Mode: w.Mode, TargetDescriptor: w.TargetDescriptor, TargetVaultCheck: w.TargetVaultCheck, Version: w.Version, Count: w.Count, OperationID: w.OperationID, OldFingerprint: w.OldFingerprint, NewFingerprint: w.NewFingerprint, OldKey: w.OldKey, NewKey: w.NewKey, ClusterKey: w.ClusterKey}
	if i.Count < 2 || i.Count > 3 {
		return authenticatedstate.ErrLimit
	}
	for n := 0; n < int(i.Count); n++ {
		e := w.Members[n]
		s := statecut.HAKeyRotationSource{Member: e.Source.Member, BaselineDigest: e.Source.BaselineDigest, Cursors: e.Source.Cursors, Heads: e.Source.Heads}
		var err error
		s.Selected, err = authenticatedstate.DecodeSealCandidate(e.Source.Selected, i.ClusterKey[:], statecut.HardLimits())
		if err != nil {
			return err
		}
		s.Observed, err = authenticatedstate.DecodeSealCandidate(e.Source.Observed, i.ClusterKey[:], statecut.HardLimits())
		if err != nil {
			return err
		}
		i.Members[n] = haGroupRotationMemberIntent{DataDir: e.DataDir, KeyFile: e.KeyFile, Source: s, Bootstrap: e.Bootstrap, VaultVersion: e.VaultVersion, SourceUnlock: e.SourceUnlock, SourceHistory: e.SourceHistory, History: e.History}
	}
	for n := int(i.Count); n < 3; n++ {
		if !emptyHAGroupMemberWire(w.Members[n]) {
			return authenticatedstate.ErrCorrupt
		}
	}
	return nil
}

func emptyHAGroupMemberWire(e haGroupRotationMemberWire) bool {
	// The bounded last slot must have the exact all-zero encoding, including
	// bootstrap key slices. It cannot hide additional members or secret material.
	a, _ := json.Marshal(e)
	b, _ := json.Marshal(haGroupRotationMemberWire{})
	return bytes.Equal(a, b)
}

func validateHAGroupRotationCapturedEntry(e haGroupRotationMemberIntent) error {
	m, b := e.Source.Member, e.Bootstrap
	if e.VaultVersion < 2 {
		return authenticatedstate.ErrSealIneligible
	}
	if b.Version != 2 || b.ClusterID != m.ClusterID || b.Incarnation != m.Incarnation || b.NodeID != m.NodeID || b.SourceAuthorityDigest != e.Source.BaselineDigest || b.Evidence.Version != 1 || b.Evidence.Identity == ([32]byte{}) || b.Evidence.KeyGeneration == 0 || b.Evidence.InstanceID == "" || b.Binding.Version != 1 || b.Binding.Identity != b.Evidence.Identity || b.Binding.KeyGeneration != b.Evidence.KeyGeneration || b.Binding.Epoch == 0 || b.Binding.RootDigest == ([32]byte{}) || b.Binding.RootID == ([16]byte{}) || len(b.LedgerKey) != 32 || len(b.AuditKey) != 32 || len(b.GovernanceKey) != 32 || len(b.LegacyMetadataKey) != 32 {
		return authenticatedstate.ErrSealIneligible
	}
	for _, path := range []string{b.LedgerPath, b.AuditPath, b.GovernancePath, b.OrderingPath, b.MetadataRoot, b.MetadataJournal} {
		if len(path) < 1 || len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return authenticatedstate.ErrLimit
		}
	}
	role := authenticatedstate.SealReplica
	if m.Role == replication.RolePrimary {
		role = authenticatedstate.SealPrimary
	}
	inc := sha256.Sum256([]byte(m.Incarnation))
	var incarnation [16]byte
	copy(incarnation[:], inc[:16])
	for _, s := range []authenticatedstate.CandidateSeal{e.Source.Selected, e.Source.Observed} {
		if s.RootCount != 7 || s.PositionCount != 5 || s.Role != role || s.Term != m.Term || s.OrderingTerm != m.Term || s.Instance != b.Evidence.Identity || s.Cluster != sha256.Sum256([]byte(m.ClusterID)) || s.Member != sha256.Sum256([]byte(m.NodeID)) || s.Incarnation != incarnation || s.OrderingIndex > m.AppliedIndex || s.OrderingIndex == 0 || s.Coverage != authenticatedstate.SealCoverageAuthenticated || s.Positions[1].MetadataBaseline != b.Binding.Cut || s.Positions[1].Epoch != b.Binding.Epoch {
			return authenticatedstate.ErrSealIneligible
		}
	}
	s := e.Source.Observed
	if s.OrderingIndex != m.AppliedIndex || s.OrderingMAC != m.OrderingHeadMAC || s.Positions[1].Sequence != m.Projection.MetadataSequence || s.Positions[1].Epoch != m.Projection.MetadataEpoch {
		return authenticatedstate.ErrSealIneligible
	}
	for n, pos := range []int{0, 3, 2, 1} {
		p, c := s.Positions[pos], e.Source.Cursors[n]
		generation := p.Generation
		if n == 3 {
			generation = p.Epoch
		}
		if p.Sequence != c.Sequence || (n == 0 || n == 3) && generation != c.Generation || p.Hash != e.Source.Heads[n] {
			return errors.New("group rotation captured native cut differs from the actual member observation")
		}
	}
	return nil
}
