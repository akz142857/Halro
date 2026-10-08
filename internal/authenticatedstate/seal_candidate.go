package authenticatedstate

// This file defines the v2 cross-component seal envelope candidate from
// docs/architecture/memory-state-authenticated-root-protocol.zh-CN.md. It is
// deliberately not wired into Runtime: decoding authenticates a publisher's
// claim, but does not establish role eligibility, source completeness, or
// business readiness.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
)

const (
	candidateSealMaxBytes = 16 << 10
	candidateSealMACBytes = sha256.Size
	candidateSealVersion  = 2
	maxSealPositions      = 8
	sealRequiredPositions = 5 // Ledger, Metadata, Governance, Audit, and Usage cuts.
	maxSealRoots          = 64
)

var ErrSealIneligible = errors.New("authenticated seal does not match independently validated startup authority")

const (
	SealComponentLedger uint8 = iota + 1
	SealComponentMetadata
	SealComponentGovernance
	SealComponentAudit
	SealComponentUsage
	SealComponentLogDirectory
)

type SealRole uint8

const (
	SealStandalone SealRole = 1
	SealPrimary    SealRole = 2
	SealReplica    SealRole = 3
)

type SealCoverage uint8

const (
	SealCoverageAuthenticated SealCoverage = 1
	SealCoverageMixed         SealCoverage = 2
	SealCoverageLegacy        SealCoverage = 3
)

// SealPosition is one authority's exact native cut. MetadataBaseline binds
// the journal position to a complete authenticated content baseline.
type SealPosition struct {
	Component        uint8
	Epoch            uint64
	Revision         uint64
	Generation       uint64
	Sequence         uint64
	Offset           uint64
	Hash             [32]byte
	MetadataBaseline [32]byte
}

// SealRoot commits one complete logical component index. FileID is a stable
// pack identity, never a caller-controlled path. Empty roots still require a
// nonzero protocol-defined digest.
const (
	SealRootNative uint8 = iota
	SealRootState
)

// StateRootCommitment authenticates the full immutable COW root record,
// including its extent and empty-pack identity. It permits selecting an older
// independently eligible root when a component publication led the common seal.
type StateRootCommitment struct {
	Scope, Cut, Digest [32]byte
	PackID             [16]byte
	Revision, Extent   uint64
	Height             uint16
}

type SealRoot struct {
	Kind      uint8
	State     StateRootCommitment
	Component uint8
	IndexID   uint16
	Codec     uint16
	Rows      uint64
	Empty     bool
	FileID    [16]byte
	Offset    uint64
	Length    uint32
	Digest    [32]byte
}

// CandidateSeal contains only fixed-size fields and bounded sorted entries.
// Limits are claims constrained by DecodeSealCandidate's independently
// supplied hard limits; they cannot raise the reader's hard ceiling.
type CandidateSeal struct {
	SchemaVersion  uint16
	ApplyVersion   uint16
	IndexVersion   uint16
	KeyDerivation  uint16
	Instance       [32]byte
	Cluster        [32]byte
	Member         [32]byte
	Incarnation    [16]byte
	Generation     uint64
	Revision       uint64
	Role           SealRole
	Term           uint64
	OrderingIndex  uint64
	OrderingOffset uint64
	OrderingMAC    [32]byte
	OrderingTerm   uint64
	Coverage       SealCoverage
	MaxPageBytes   uint32
	MaxValueBytes  uint32
	MaxTreeHeight  uint16
	PositionCount  uint8
	Positions      [maxSealPositions]SealPosition
	RootCount      uint8
	Roots          [maxSealRoots]SealRoot
}

type SealHardLimits struct {
	MaxPageBytes  uint32
	MaxValueBytes uint32
	MaxTreeHeight uint16
	// RequiredRoots is the caller's versioned schema inventory. Decode refuses
	// a well-MACed seal that omits or substitutes any required logical index.
	RequiredRoots []SealRootSchema
}

// SealEligibility is assembled from independently validated current startup
// authority: stable identity, exact runtime role/term, the role-permitted
// ordering cut, and exact native component positions. It deliberately carries
// no filesystem paths or caller-selected fallbacks. Matching this value does
// not authenticate its source; callers must derive it from the existing
// identity, member-state, ordering, and native-log verifiers.
type SealEligibility struct {
	SchemaVersion  uint16
	ApplyVersion   uint16
	IndexVersion   uint16
	KeyDerivation  uint16
	Instance       [32]byte
	Cluster        [32]byte
	Member         [32]byte
	Incarnation    [16]byte
	Role           SealRole
	Term           uint64
	OrderingIndex  uint64
	OrderingOffset uint64
	OrderingMAC    [32]byte
	Positions      [sealRequiredPositions]SealPosition
	Coverage       SealCoverage
}

// ValidateSealEligibility only selects a seal whose complete identity,
// versions, role/term, ordering evidence, coverage and five native positions
// exactly match independently established startup authority. Exact matching
// is intentionally conservative: an older seal requires an explicit recovery
// path that proves and replays its suffix before it can be selected.
func ValidateSealEligibility(s CandidateSeal, expected SealEligibility) error {
	if expected.SchemaVersion == 0 || expected.ApplyVersion == 0 || expected.IndexVersion == 0 || expected.KeyDerivation == 0 ||
		expected.Instance == ([32]byte{}) || expected.Incarnation == ([16]byte{}) || expected.Role < SealStandalone || expected.Role > SealReplica ||
		expected.Coverage < SealCoverageAuthenticated || expected.Coverage > SealCoverageLegacy {
		return ErrSealIneligible
	}
	if expected.Role == SealStandalone {
		if expected.Cluster != ([32]byte{}) || expected.Member != ([32]byte{}) || expected.Term != 0 || expected.OrderingIndex != 0 || expected.OrderingOffset != 0 || expected.OrderingMAC != ([32]byte{}) {
			return ErrSealIneligible
		}
	} else if expected.Cluster == ([32]byte{}) || expected.Member == ([32]byte{}) || expected.Term == 0 || expected.OrderingIndex == 0 || expected.OrderingOffset > math.MaxInt64 || expected.OrderingMAC == ([32]byte{}) {
		return ErrSealIneligible
	}
	if s.SchemaVersion != expected.SchemaVersion || s.ApplyVersion != expected.ApplyVersion || s.IndexVersion != expected.IndexVersion || s.KeyDerivation != expected.KeyDerivation ||
		s.Instance != expected.Instance || s.Cluster != expected.Cluster || s.Member != expected.Member || s.Incarnation != expected.Incarnation ||
		s.Role != expected.Role || s.Term != expected.Term || s.OrderingIndex != expected.OrderingIndex || s.OrderingOffset != expected.OrderingOffset ||
		s.OrderingMAC != expected.OrderingMAC || s.OrderingTerm != expected.Term || s.Coverage != expected.Coverage || s.PositionCount != sealRequiredPositions {
		return ErrSealIneligible
	}
	for i := range expected.Positions {
		if expected.Positions[i].Component != uint8(i+1) || s.Positions[i] != expected.Positions[i] {
			return ErrSealIneligible
		}
	}
	return nil
}

// AssembleSealCandidate combines component receipts with independently
// established startup authority. It copies the exact five native positions
// from authority and accepts roots only in the caller's versioned schema
// order. This prevents a caller from accidentally omitting a component root
// or pairing roots with positions from different cuts. It does not establish
// that authority is trustworthy, freeze writers, or grant Runtime restore
// permission; callers must obtain authority and receipts under their own
// lifecycle barrier before publishing the result.
type SealComponentReference struct {
	Root     SealRoot
	Position SealPosition
}

func AssembleSealCandidate(base CandidateSeal, authority SealEligibility, references []SealComponentReference, hard SealHardLimits) (CandidateSeal, error) {
	if len(references) != len(hard.RequiredRoots) || len(references) == 0 || len(references) > maxSealRoots {
		return CandidateSeal{}, ErrSealIneligible
	}
	s := CandidateSeal{
		SchemaVersion:  authority.SchemaVersion,
		ApplyVersion:   authority.ApplyVersion,
		IndexVersion:   authority.IndexVersion,
		KeyDerivation:  authority.KeyDerivation,
		Instance:       authority.Instance,
		Cluster:        authority.Cluster,
		Member:         authority.Member,
		Incarnation:    authority.Incarnation,
		Generation:     base.Generation,
		Revision:       base.Revision,
		Role:           authority.Role,
		Term:           authority.Term,
		OrderingIndex:  authority.OrderingIndex,
		OrderingOffset: authority.OrderingOffset,
		OrderingMAC:    authority.OrderingMAC,
		OrderingTerm:   authority.Term,
		Coverage:       authority.Coverage,
		MaxPageBytes:   base.MaxPageBytes,
		MaxValueBytes:  base.MaxValueBytes,
		MaxTreeHeight:  base.MaxTreeHeight,
		PositionCount:  sealRequiredPositions,
		RootCount:      uint8(len(references)),
	}
	copy(s.Positions[:], authority.Positions[:])
	for i, reference := range references {
		component := reference.Root.Component
		positionComponent := component
		if component == SealComponentLogDirectory {
			// The segment directory is an additional Ledger index, not a sixth
			// native authority; bind it to the exact Ledger cut.
			positionComponent = SealComponentLedger
		}
		if component == 0 || component > SealComponentLogDirectory ||
			reference.Position.Component != positionComponent ||
			reference.Position != authority.Positions[positionComponent-1] {
			return CandidateSeal{}, ErrSealIneligible
		}
		s.Roots[i] = reference.Root
	}
	if err := ValidateSealEligibility(s, authority); err != nil {
		return CandidateSeal{}, err
	}
	if !s.valid(hard) {
		return CandidateSeal{}, ErrSealIneligible
	}
	return s, nil
}

type SealRootSchema struct {
	Kind      uint8
	Component uint8
	IndexID   uint16
	Codec     uint16
}

func (s CandidateSeal) valid(h SealHardLimits) bool {
	if h.MaxPageBytes == 0 || h.MaxValueBytes == 0 || h.MaxTreeHeight == 0 ||
		len(h.RequiredRoots) == 0 || len(h.RequiredRoots) > maxSealRoots ||
		s.SchemaVersion == 0 || s.ApplyVersion == 0 || s.IndexVersion == 0 || s.KeyDerivation == 0 ||
		s.Instance == ([32]byte{}) || s.Incarnation == ([16]byte{}) || s.Generation == 0 || s.Revision == 0 ||
		s.Role < SealStandalone || s.Role > SealReplica || s.Coverage < SealCoverageAuthenticated || s.Coverage > SealCoverageLegacy ||
		s.MaxPageBytes == 0 || s.MaxPageBytes > h.MaxPageBytes || s.MaxValueBytes == 0 || s.MaxValueBytes > h.MaxValueBytes ||
		s.MaxTreeHeight == 0 || s.MaxTreeHeight > h.MaxTreeHeight ||
		s.PositionCount != sealRequiredPositions || s.RootCount == 0 || s.RootCount > maxSealRoots {
		return false
	}
	if s.Role == SealStandalone {
		if s.Cluster != ([32]byte{}) || s.Member != ([32]byte{}) || s.Term != 0 || s.OrderingIndex != 0 || s.OrderingOffset != 0 || s.OrderingMAC != ([32]byte{}) || s.OrderingTerm != 0 {
			return false
		}
	} else if s.Cluster == ([32]byte{}) || s.Member == ([32]byte{}) || s.Term == 0 || s.OrderingIndex == 0 || s.OrderingOffset > math.MaxInt64 || s.OrderingMAC == ([32]byte{}) || s.OrderingTerm != s.Term {
		return false
	}
	for i := 0; i < int(s.PositionCount); i++ {
		p := s.Positions[i]
		if p.Component != uint8(i+1) || p.Offset > math.MaxInt64 {
			return false
		}
		if p.Component == SealComponentMetadata {
			if p.Epoch == 0 || p.Revision == 0 || p.MetadataBaseline == ([32]byte{}) {
				return false
			}
		} else if p.MetadataBaseline != ([32]byte{}) {
			return false
		}
	}
	for i := 0; i < int(s.RootCount); i++ {
		r := s.Roots[i]
		if !validSealRootReference(r) || !validRootComponent(r.Component) || r.IndexID == 0 || r.Codec == 0 || r.Digest == ([32]byte{}) ||
			i > 0 && (s.Roots[i-1].Component > r.Component || s.Roots[i-1].Component == r.Component && s.Roots[i-1].IndexID >= r.IndexID) {
			return false
		}
		if i >= len(h.RequiredRoots) || h.RequiredRoots[i] != (SealRootSchema{Kind: r.Kind, Component: r.Component, IndexID: r.IndexID, Codec: r.Codec}) {
			return false
		}
		if r.Kind == SealRootState && r.State.Height > s.MaxTreeHeight {
			return false
		}
		if r.Empty {
			if r.Rows != 0 || r.FileID != ([16]byte{}) || r.Offset != 0 || r.Length != 0 {
				return false
			}
		} else if r.Rows == 0 || r.FileID == ([16]byte{}) || r.Length == 0 || r.Length > h.MaxPageBytes || r.Length > s.MaxPageBytes || r.Offset > math.MaxInt64-uint64(r.Length) {
			return false
		}
	}
	return int(s.RootCount) == len(h.RequiredRoots)
}

func validSealComponent(c uint8) bool { return c >= SealComponentLedger && c <= SealComponentUsage }
func validRootComponent(c uint8) bool {
	return c >= SealComponentLedger && c <= SealComponentLogDirectory
}

// EncodeSealCandidate returns canonical big-endian bytes followed by a
// domain-separated HMAC-SHA256. Callers must still perform source, role, and
// lifecycle validation before treating the seal as eligible.
func EncodeSealCandidate(s CandidateSeal, key []byte, hard SealHardLimits) ([]byte, error) {
	if len(key) != sha256.Size || !s.valid(hard) {
		return nil, ErrLimit
	}
	b := make([]byte, 0, candidateSealMaxBytes)
	b = append(b, []byte("HLSEAL01")...)
	b = binary.BigEndian.AppendUint16(b, candidateSealVersion)
	b = binary.BigEndian.AppendUint16(b, s.SchemaVersion)
	b = binary.BigEndian.AppendUint16(b, s.ApplyVersion)
	b = binary.BigEndian.AppendUint16(b, s.IndexVersion)
	b = binary.BigEndian.AppendUint16(b, s.KeyDerivation)
	b = append(b, s.Instance[:]...)
	b = append(b, s.Cluster[:]...)
	b = append(b, s.Member[:]...)
	b = append(b, s.Incarnation[:]...)
	for _, n := range []uint64{s.Generation, s.Revision} {
		b = binary.BigEndian.AppendUint64(b, n)
	}
	b = append(b, byte(s.Role))
	for _, n := range []uint64{s.Term, s.OrderingIndex, s.OrderingOffset} {
		b = binary.BigEndian.AppendUint64(b, n)
	}
	b = append(b, s.OrderingMAC[:]...)
	b = binary.BigEndian.AppendUint64(b, s.OrderingTerm)
	b = append(b, byte(s.Coverage))
	b = binary.BigEndian.AppendUint32(b, s.MaxPageBytes)
	b = binary.BigEndian.AppendUint32(b, s.MaxValueBytes)
	b = binary.BigEndian.AppendUint16(b, s.MaxTreeHeight)
	b = append(b, s.PositionCount)
	for i := 0; i < int(s.PositionCount); i++ {
		p := s.Positions[i]
		b = append(b, p.Component)
		for _, n := range []uint64{p.Epoch, p.Revision, p.Generation, p.Sequence, p.Offset} {
			b = binary.BigEndian.AppendUint64(b, n)
		}
		b = append(b, p.Hash[:]...)
		b = append(b, p.MetadataBaseline[:]...)
	}
	b = append(b, s.RootCount)
	for i := 0; i < int(s.RootCount); i++ {
		r := s.Roots[i]
		b = append(b, r.Component, r.Kind)
		b = binary.BigEndian.AppendUint16(b, r.IndexID)
		b = binary.BigEndian.AppendUint16(b, r.Codec)
		b = binary.BigEndian.AppendUint64(b, r.Rows)
		if r.Empty {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
		b = append(b, r.FileID[:]...)
		b = binary.BigEndian.AppendUint64(b, r.Offset)
		b = binary.BigEndian.AppendUint32(b, r.Length)
		b = append(b, r.Digest[:]...)
		for _, v := range [][32]byte{r.State.Scope, r.State.Cut, r.State.Digest} {
			b = append(b, v[:]...)
		}
		b = append(b, r.State.PackID[:]...)
		b = binary.BigEndian.AppendUint64(b, r.State.Revision)
		b = binary.BigEndian.AppendUint64(b, r.State.Extent)
		b = binary.BigEndian.AppendUint16(b, r.State.Height)
	}
	if len(b)+candidateSealMACBytes > candidateSealMaxBytes {
		return nil, ErrLimit
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("halro:cross-component-seal:candidate:v2\x00"))
	_, _ = mac.Write(b)
	return append(b, mac.Sum(nil)...), nil
}

// DecodeSealCandidate validates the size before parsing, authenticates the
// exact canonical prefix, rejects unknown/trailing fields, and applies caller
// supplied hard limits. It does not authorize resume or readiness.
func DecodeSealCandidate(raw, key []byte, hard SealHardLimits) (CandidateSeal, error) {
	var s CandidateSeal
	if len(key) != sha256.Size || len(raw) > candidateSealMaxBytes || len(raw) < 8+2+candidateSealMACBytes || !bytes.Equal(raw[:8], []byte("HLSEAL01")) {
		return s, ErrCorrupt
	}
	body, tag := raw[:len(raw)-candidateSealMACBytes], raw[len(raw)-candidateSealMACBytes:]
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("halro:cross-component-seal:candidate:v2\x00"))
	_, _ = mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), tag) {
		return s, ErrCorrupt
	}
	pos := 8
	take := func(n int) ([]byte, bool) {
		if n < 0 || n > len(body)-pos {
			return nil, false
		}
		v := body[pos : pos+n]
		pos += n
		return v, true
	}
	u16 := func() (uint16, bool) {
		b, ok := take(2)
		if !ok {
			return 0, false
		}
		return binary.BigEndian.Uint16(b), true
	}
	u32 := func() (uint32, bool) {
		b, ok := take(4)
		if !ok {
			return 0, false
		}
		return binary.BigEndian.Uint32(b), true
	}
	u64 := func() (uint64, bool) {
		b, ok := take(8)
		if !ok {
			return 0, false
		}
		return binary.BigEndian.Uint64(b), true
	}
	version, ok := u16()
	if !ok || version != candidateSealVersion {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.SchemaVersion, ok = u16(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.ApplyVersion, ok = u16(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.IndexVersion, ok = u16(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.KeyDerivation, ok = u16(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	for _, dst := range [][]byte{s.Instance[:], s.Cluster[:], s.Member[:], s.Incarnation[:]} {
		b, ok := take(len(dst))
		if !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		copy(dst, b)
	}
	if s.Generation, ok = u64(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.Revision, ok = u64(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	bRole, ok := take(1)
	if !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	s.Role = SealRole(bRole[0])
	if s.Term, ok = u64(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.OrderingIndex, ok = u64(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.OrderingOffset, ok = u64(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	b, ok := take(32)
	if !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	copy(s.OrderingMAC[:], b)
	if s.OrderingTerm, ok = u64(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	b, ok = take(1)
	if !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	s.Coverage = SealCoverage(b[0])
	if s.MaxPageBytes, ok = u32(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.MaxValueBytes, ok = u32(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	if s.MaxTreeHeight, ok = u16(); !ok {
		return CandidateSeal{}, ErrCorrupt
	}
	b, ok = take(1)
	if !ok || b[0] != sealRequiredPositions {
		return CandidateSeal{}, ErrCorrupt
	}
	s.PositionCount = b[0]
	for i := 0; i < int(s.PositionCount); i++ {
		p := &s.Positions[i]
		b, ok = take(1)
		if !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		p.Component = b[0]
		if p.Epoch, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if p.Revision, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if p.Generation, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if p.Sequence, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if p.Offset, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		b, ok = take(32)
		if !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		copy(p.Hash[:], b)
		b, ok = take(32)
		if !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		copy(p.MetadataBaseline[:], b)
	}
	b, ok = take(1)
	if !ok || b[0] == 0 || b[0] > maxSealRoots {
		return CandidateSeal{}, ErrCorrupt
	}
	s.RootCount = b[0]
	for i := 0; i < int(s.RootCount); i++ {
		r := &s.Roots[i]
		b, ok = take(1)
		if !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		r.Component = b[0]
		b, ok = take(1)
		if !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		r.Kind = b[0]
		if r.IndexID, ok = u16(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if r.Codec, ok = u16(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if r.Rows, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		b, ok = take(1)
		if !ok || b[0] > 1 {
			return CandidateSeal{}, ErrCorrupt
		}
		r.Empty = b[0] == 1
		b, ok = take(16)
		if !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		copy(r.FileID[:], b)
		if r.Offset, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if r.Length, ok = u32(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		b, ok = take(32)
		if !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		copy(r.Digest[:], b)
		for _, dst := range [][]byte{r.State.Scope[:], r.State.Cut[:], r.State.Digest[:], r.State.PackID[:]} {
			b, ok = take(len(dst))
			if !ok {
				return CandidateSeal{}, ErrCorrupt
			}
			copy(dst, b)
		}
		if r.State.Revision, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if r.State.Extent, ok = u64(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
		if r.State.Height, ok = u16(); !ok {
			return CandidateSeal{}, ErrCorrupt
		}
	}
	if pos != len(body) || !s.valid(hard) {
		return CandidateSeal{}, ErrCorrupt
	}
	return s, nil
}
