package replication

import (
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func TestStoreBridgesPreserveExactDurableBytesAndSequenceRanges(t *testing.T) {
	outbound := &recordingOutbound{}
	primary, journal := newTestPrimary(t, outbound)
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		store      Store
		generation uint64
		call       func() (LocalCommit, error)
		want       []byte
	}{
		{name: "ledger", store: StoreLedger, generation: 7, want: []byte("ledger-frames"), call: func() (LocalCommit, error) {
			return primary.RecordLedgerBatch(ledger.DurableBatch{Generation: 7, FirstSequence: 1, LastSequence: 2, Frames: []byte("ledger-frames")})
		}},
		{name: "audit", store: StoreAudit, generation: 1, want: []byte("audit-frames"), call: func() (LocalCommit, error) {
			return primary.RecordAuditBatch(audit.DurableBatch{FirstSequence: 1, LastSequence: 2, Frames: []byte("audit-frames")})
		}},
		{name: "governance", store: StoreGovernance, generation: 1, want: []byte("governance-frames"), call: func() (LocalCommit, error) {
			return primary.RecordGovernanceBatch(governance.DurableBatch{FirstSequence: 1, LastSequence: 2, Frames: []byte("governance-frames")})
		}},
		{name: "metadata", store: StoreMetadata, generation: 4, want: []byte("metadata-frames"), call: func() (LocalCommit, error) {
			return primary.RecordMetadataBatch(metadatajournal.DurableBatch{Epoch: 4, FirstSequence: 1, LastSequence: 1, Frames: []byte("metadata-frames")})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commit, err := test.call()
			if err != nil {
				t.Fatal(err)
			}
			frame, err := UnmarshalFrame(commit.Encoded)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Store != test.store || frame.StoreGeneration != test.generation || string(frame.Payload) != string(test.want) || frame.StoreSequenceFirst != 1 || frame.StoreSequenceLast == 0 {
				t.Fatalf("frame=%#v", frame)
			}
		})
	}
}

func TestLedgerRollBridgePreservesAuthoritativeSegmentMetadata(t *testing.T) {
	outbound := &recordingOutbound{}
	primary, journal := newTestPrimary(t, outbound)
	defer journal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished, Store: StoreNone}); err != nil {
		t.Fatal(err)
	}
	sealedAt := time.Unix(1_700_000_000, 123).UTC()
	segment := ledger.Segment{
		Generation: 3, FirstSequence: 41, LastSequence: 52, Length: 4096, EndEpoch: 2,
		StartHash: strings.Repeat("11", 32), EndHash: strings.Repeat("22", 32),
		PlainChecksum: strings.Repeat("33", 32), SealedAt: sealedAt,
	}
	commit, err := primary.RecordLedgerRoll(segment)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := UnmarshalFrame(commit.Encoded)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := DecodeLedgerRollMetadata(frame.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	got := ledgerSegmentFromMetadata(metadata)
	if got.Generation != segment.Generation || got.FirstSequence != segment.FirstSequence ||
		got.LastSequence != segment.LastSequence || got.Length != segment.Length ||
		got.EndEpoch != segment.EndEpoch || got.StartHash != segment.StartHash ||
		got.EndHash != segment.EndHash || got.PlainChecksum != segment.PlainChecksum ||
		!got.SealedAt.Equal(segment.SealedAt) {
		t.Fatalf("roll round trip=%#v, want authoritative fields from %#v", got, segment)
	}
}
