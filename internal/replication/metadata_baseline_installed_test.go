package replication

import (
	"crypto/sha256"
	"path/filepath"
	"testing"
)

func TestActivatedMetadataBaselineOrderingRequiresRealSignedPrefixAndBoundary(t *testing.T) {
	source, journal, a, _, _, _ := metadataBoundaryFixture(t)
	if err := source.ValidateActivatedMetadataBaselineOrdering(journal); err == nil {
		t.Fatal("unactivated candidate acquired installed boundary qualification")
	}
	metadata, err := a.Descriptor().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	n := a.Claims().Index
	boundary, err := journal.Append(OrderingRecord{Kind: KindMetadataBaseline, Index: n + 1, Term: a.Claims().Term, ConfirmedIndex: n, Metadata: metadata, FrameDigest: sha256.Sum256(metadata)})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.ActivateMetadataBaseline(journal, n+1); err != nil {
		t.Fatal(err)
	}
	if err := source.ValidateActivatedMetadataBaselineOrdering(journal); err != nil {
		t.Fatal("genuine installed boundary refused", err)
	}
	next := testOrderingRecord(n+2, a.Claims().Term, "continuous later record")
	next.Store = StoreMetadata
	next.StoreGeneration = a.Claims().New.Baseline.Epoch
	next.StoreSequenceFirst, next.StoreSequenceLast = 1, 1
	if _, err := journal.Append(next); err != nil {
		t.Fatal(err)
	}
	if err := source.ValidateActivatedMetadataBaselineOrdering(journal); err != nil {
		t.Fatal("continuous later suffix invalidated original boundary", err)
	}
	if err := source.ValidateMetadataBaselineOrdering(journal, boundary); err == nil {
		t.Fatal("old append API admitted a second transition after N+1")
	}
	foreign, err := OpenOrderingJournal(filepath.Join(t.TempDir(), "ordering.journal"), []byte("0123456789abcdef0123456789abcdef"), OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if err := source.ValidateActivatedMetadataBaselineOrdering(foreign); err == nil {
		t.Fatal("foreign ordering without original signed MAC prefix accepted")
	}
}
