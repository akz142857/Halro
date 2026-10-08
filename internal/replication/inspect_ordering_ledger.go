package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/akz142857/Halro/internal/ledger"
)

// InspectOrderingLedger binds every ordered Ledger range and roll (including
// excluded tails) to the native bytes, as well as the header's seeded Ledger
// head. Other stores' bodies are outside this Usage-specific inspection. The
// caller owns exclusive directory locking and independently verifies its native
// external anchor and role/member/Metadata evidence. No native file is repaired.
// One finite native reader and one maximum frame are live at a time. Historical
// roll descriptions still incur full authentication scans, not an indexed lookup.
func InspectOrderingLedger(ctx context.Context, path string, key []byte, clusterID, incarnation string, expectedIndex uint64, expectedHead [sha256.Size]byte, through uint64, nativePath string, nativeKey []byte) (OrderingInspection, error) {
	reader, err := ledger.NewReplicationRangeReader(nativePath, nativeKey, MaxPayloadBytes)
	if err != nil {
		return OrderingInspection{}, err
	}
	defer reader.Close()
	checkHeader := func(h OrderingHeader) error {
		baseline := h.StoreCursors[StoreLedger-StoreLedger]
		generation := baseline.Generation
		if baseline.Sequence == 0 {
			generation = 0 // Canonical empty baseline; still authenticate all bytes.
		}
		prefix, _, err := ledger.InspectProjectionCursor(ctx, nativePath, nativeKey, generation, baseline.Sequence)
		if err != nil {
			return err
		}
		if prefix.Hash != h.StoreHeads[StoreLedger-StoreLedger] {
			return fmt.Errorf("ordering Ledger baseline differs from native authenticated head")
		}
		return nil
	}
	checkRecord := func(h OrderingHeader, r OrderingRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.Kind != KindLedgerRoll && !(r.Kind == KindData && r.Store == StoreLedger) {
			return nil
		}
		var payload []byte
		if r.Kind == KindData {
			var err error
			payload, err = reader.Read(r.StoreGeneration, r.StoreSequenceFirst, r.StoreSequenceLast)
			if err != nil {
				return fmt.Errorf("read native Ledger for ordering index %d: %w", r.Index, err)
			}
		} else {
			roll, err := DecodeLedgerRollMetadata(r.Metadata)
			if err != nil {
				return err
			}
			segment, err := ledger.ReplicationSegmentContext(ctx, nativePath, nativeKey, roll.Generation)
			if err != nil {
				return err
			}
			actual, err := ledgerRollMetadata(segment)
			if err != nil {
				return err
			}
			metadata, err := actual.MarshalBinary()
			if err != nil {
				return err
			}
			if !bytes.Equal(metadata, r.Metadata) {
				return fmt.Errorf("native Ledger roll differs from ordering index %d", r.Index)
			}
		}
		frame := Frame{Kind: r.Kind, Store: r.Store, Index: r.Index, Term: r.Term, ConfirmedIndex: r.ConfirmedIndex,
			StoreGeneration: r.StoreGeneration, StoreSequenceFirst: r.StoreSequenceFirst, StoreSequenceLast: r.StoreSequenceLast,
			ClusterID: h.ClusterID, Incarnation: h.Incarnation, Metadata: r.Metadata, Payload: payload}
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			return err
		}
		if digest != r.FrameDigest {
			return fmt.Errorf("native Ledger frame differs from authenticated ordering digest at index %d", r.Index)
		}
		return ctx.Err()
	}
	return inspectOrderingJournal(ctx, path, key, clusterID, incarnation, expectedIndex, expectedHead, through, checkHeader, checkRecord, 0)
}
