package authenticatedstate

import (
	"bytes"
	"testing"
)

func sealCandidateFixture() CandidateSeal {
	var s CandidateSeal
	s.SchemaVersion, s.ApplyVersion, s.IndexVersion, s.KeyDerivation = 1, 1, 1, 1
	s.Instance[0], s.Incarnation[0] = 1, 1
	s.Generation, s.Revision = 1, 1
	s.Role, s.Coverage = SealStandalone, SealCoverageAuthenticated
	s.MaxPageBytes, s.MaxValueBytes, s.MaxTreeHeight = 512<<10, 3<<20, 32
	s.PositionCount = sealRequiredPositions
	s.Positions[0] = SealPosition{Component: SealComponentLedger, Generation: 1, Hash: [32]byte{1}}
	s.Positions[1] = SealPosition{Component: SealComponentMetadata, Epoch: 1, Revision: 1, MetadataBaseline: [32]byte{2}}
	s.Positions[2] = SealPosition{Component: SealComponentGovernance}
	s.Positions[3] = SealPosition{Component: SealComponentAudit}
	s.Positions[4] = SealPosition{Component: SealComponentUsage}
	s.RootCount = 1
	s.Roots[0] = SealRoot{Component: SealComponentLedger, IndexID: 1, Codec: 1, Rows: 1, FileID: [16]byte{1}, Length: 160, Digest: [32]byte{1}}
	return s
}

func sealHardFor(s CandidateSeal) SealHardLimits {
	h := SealHardLimits{MaxPageBytes: 512 << 10, MaxValueBytes: 3 << 20, MaxTreeHeight: 32}
	for i := 0; i < int(s.RootCount); i++ {
		r := s.Roots[i]
		h.RequiredRoots = append(h.RequiredRoots, SealRootSchema{Kind: r.Kind, Component: r.Component, IndexID: r.IndexID, Codec: r.Codec})
	}
	return h
}

func sealEligibilityFor(s CandidateSeal) SealEligibility {
	var positions [sealRequiredPositions]SealPosition
	copy(positions[:], s.Positions[:sealRequiredPositions])
	return SealEligibility{
		SchemaVersion: s.SchemaVersion, ApplyVersion: s.ApplyVersion, IndexVersion: s.IndexVersion, KeyDerivation: s.KeyDerivation,
		Instance: s.Instance, Cluster: s.Cluster, Member: s.Member, Incarnation: s.Incarnation,
		Role: s.Role, Term: s.Term, OrderingIndex: s.OrderingIndex, OrderingOffset: s.OrderingOffset,
		OrderingMAC: s.OrderingMAC, Positions: positions, Coverage: s.Coverage,
	}
}

func TestCandidateSealCanonicalRoundTripAndHardLimits(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	want := sealCandidateFixture()
	want.RootCount = 2
	want.Roots[1] = want.Roots[0]
	want.Roots[1].IndexID, want.Roots[1].FileID[0], want.Roots[1].Offset = 2, 2, 160
	hard := sealHardFor(want)
	raw, err := EncodeSealCandidate(want, key, hard)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSealCandidate(raw, key, hard)
	if err != nil || got != want {
		t.Fatalf("round trip = %#v, %v", got, err)
	}
	raw2, err := EncodeSealCandidate(got, key, hard)
	if err != nil || !bytes.Equal(raw, raw2) {
		t.Fatalf("encoding is not canonical: %v", err)
	}
	if _, err := DecodeSealCandidate(raw, key, SealHardLimits{MaxPageBytes: 4096, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, RequiredRoots: hard.RequiredRoots}); err == nil {
		t.Fatal("seal raised the reader's independent page limit")
	}
	if _, err := DecodeSealCandidate(raw, key, SealHardLimits{MaxPageBytes: hard.MaxPageBytes, MaxValueBytes: hard.MaxValueBytes, MaxTreeHeight: hard.MaxTreeHeight, RequiredRoots: hard.RequiredRoots[:1]}); err == nil {
		t.Fatal("seal's incomplete root inventory was accepted")
	}
}

func TestAssembleSealCandidateRequiresExactComponentReferencesAndSchemaOrder(t *testing.T) {
	base := sealCandidateFixture()
	authority := sealEligibilityFor(base)
	references := []SealComponentReference{{Root: base.Roots[0], Position: authority.Positions[0]}}
	hard := sealHardFor(base)

	got, err := AssembleSealCandidate(base, authority, references, hard)
	if err != nil {
		t.Fatal(err)
	}
	if got.PositionCount != sealRequiredPositions || got.RootCount != 1 || got.Positions != base.Positions || got.Roots[0] != base.Roots[0] {
		t.Fatalf("assembled candidate differs from exact authority/reference: %#v", got)
	}
	if err := ValidateSealEligibility(got, authority); err != nil {
		t.Fatalf("assembled candidate lost startup eligibility: %v", err)
	}

	wrongCut := references[0]
	wrongCut.Position.Sequence++
	if _, err := AssembleSealCandidate(base, authority, []SealComponentReference{wrongCut}, hard); err == nil {
		t.Fatal("assembled candidate accepted a component root from another cut")
	}
	if _, err := AssembleSealCandidate(base, authority, nil, hard); err == nil {
		t.Fatal("assembled candidate accepted an omitted required root")
	}

	second := SealRoot{Component: SealComponentMetadata, IndexID: 1, Codec: 1, Rows: 1, FileID: [16]byte{2}, Length: 160, Digest: [32]byte{2}}
	metadataRef := SealComponentReference{Root: second, Position: authority.Positions[1]}
	orderedHard := SealHardLimits{
		MaxPageBytes: hard.MaxPageBytes, MaxValueBytes: hard.MaxValueBytes, MaxTreeHeight: hard.MaxTreeHeight,
		RequiredRoots: []SealRootSchema{hard.RequiredRoots[0], {Component: SealComponentMetadata, IndexID: 1, Codec: 1}},
	}
	if _, err := AssembleSealCandidate(base, authority, []SealComponentReference{metadataRef, references[0]}, orderedHard); err == nil {
		t.Fatal("assembled candidate accepted roots outside the canonical schema order")
	}

	logDirectory := SealRoot{Component: SealComponentLogDirectory, IndexID: 1, Codec: 1, Rows: 1, FileID: [16]byte{3}, Length: 160, Digest: [32]byte{3}}
	ledgerAndDirectory := []SealComponentReference{
		references[0],
		{Root: logDirectory, Position: authority.Positions[0]},
	}
	ledgerDirectoryHard := SealHardLimits{
		MaxPageBytes: hard.MaxPageBytes, MaxValueBytes: hard.MaxValueBytes, MaxTreeHeight: hard.MaxTreeHeight,
		RequiredRoots: []SealRootSchema{
			hard.RequiredRoots[0], {Component: SealComponentLogDirectory, IndexID: 1, Codec: 1},
		},
	}
	if _, err := AssembleSealCandidate(base, authority, ledgerAndDirectory, ledgerDirectoryHard); err != nil {
		t.Fatalf("Ledger directory root did not bind to the Ledger cut: %v", err)
	}
}

func TestCandidateSealRejectsTamperingMalformedAndUnsortedEntries(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	want := sealCandidateFixture()
	hard := sealHardFor(want)
	raw, err := EncodeSealCandidate(want, key, hard)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Clone(raw)
	changed[20] ^= 1
	if _, err := DecodeSealCandidate(changed, key, hard); err == nil {
		t.Fatal("tampered seal accepted")
	}
	if _, err := DecodeSealCandidate(raw[:len(raw)-1], key, hard); err == nil {
		t.Fatal("truncated seal accepted")
	}
	if _, err := DecodeSealCandidate(append(bytes.Clone(raw), 0), key, hard); err == nil {
		t.Fatal("trailing byte accepted")
	}
	want.Positions[4].Component = SealComponentAudit
	if _, err := EncodeSealCandidate(want, key, hard); err == nil {
		t.Fatal("incomplete/duplicate component position accepted")
	}
	want = sealCandidateFixture()
	want.RootCount = 2
	want.Roots[1] = want.Roots[0]
	hard = sealHardFor(want)
	if _, err := EncodeSealCandidate(want, key, hard); err == nil {
		t.Fatal("duplicate logical index root accepted")
	}
}

func TestCandidateSealRoleBindingIsFailClosed(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	s := sealCandidateFixture()
	hard := sealHardFor(s)
	s.Role = SealPrimary
	if _, err := EncodeSealCandidate(s, key, hard); err == nil {
		t.Fatal("Primary seal without cluster/order proof accepted")
	}
	s.Cluster, s.Member = [32]byte{1}, [32]byte{2}
	s.Term, s.OrderingTerm, s.OrderingIndex = 4, 4, 9
	s.OrderingMAC = [32]byte{1}
	if _, err := EncodeSealCandidate(s, key, hard); err != nil {
		t.Fatal(err)
	}
	s.OrderingTerm--
	if _, err := EncodeSealCandidate(s, key, hard); err == nil {
		t.Fatal("mismatched ordering term accepted")
	}
}

func TestCandidateSealExplicitEmptyRoot(t *testing.T) {
	key := bytes.Repeat([]byte{5}, 32)
	s := sealCandidateFixture()
	hard := sealHardFor(s)
	s.Roots[0] = SealRoot{Component: SealComponentLedger, IndexID: 1, Codec: 1, Empty: true, Digest: [32]byte{4}}
	raw, err := EncodeSealCandidate(s, key, hard)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSealCandidate(raw, key, hard)
	if err != nil || got != s {
		t.Fatalf("explicit empty root = %#v, %v", got, err)
	}
	s.Roots[0].FileID = [16]byte{1}
	if _, err := EncodeSealCandidate(s, key, hard); err == nil {
		t.Fatal("empty root with a page location accepted")
	}
}

func TestCandidateSealRequiresExactIndependentStartupEligibility(t *testing.T) {
	seal := sealCandidateFixture()
	expected := sealEligibilityFor(seal)
	if err := ValidateSealEligibility(seal, expected); err != nil {
		t.Fatalf("matching independently derived authority rejected: %v", err)
	}
	tests := []struct {
		name string
		edit func(*SealEligibility)
	}{
		{"identity", func(e *SealEligibility) { e.Instance[0]++ }},
		{"incarnation", func(e *SealEligibility) { e.Incarnation[0]++ }},
		{"schema", func(e *SealEligibility) { e.SchemaVersion++ }},
		{"role", func(e *SealEligibility) { e.Role = SealPrimary }},
		{"term", func(e *SealEligibility) { e.Term++ }},
		{"coverage", func(e *SealEligibility) { e.Coverage = SealCoverageMixed }},
		{"native-position", func(e *SealEligibility) { e.Positions[0].Sequence++ }},
		{"incomplete-positions", func(e *SealEligibility) { e.Positions[4].Component = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrong := expected
			tt.edit(&wrong)
			if err := ValidateSealEligibility(seal, wrong); err == nil {
				t.Fatal("ineligible seal accepted")
			}
		})
	}
	badHA := expected
	badHA.Cluster = [32]byte{1}
	if err := ValidateSealEligibility(seal, badHA); err == nil {
		t.Fatal("Standalone expectation with fabricated HA identity accepted")
	}
}
