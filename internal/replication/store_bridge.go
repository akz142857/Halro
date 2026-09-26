package replication

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func (c *PrimaryCoordinator) RecordLedgerBatch(batch ledger.DurableBatch) (LocalCommit, error) {
	return c.RecordDurable(Frame{
		Kind: KindData, Store: StoreLedger,
		StoreGeneration:    batch.Generation,
		StoreSequenceFirst: batch.FirstSequence, StoreSequenceLast: batch.LastSequence,
		Payload: batch.Frames,
	})
}

func (c *PrimaryCoordinator) RecordAuditBatch(batch audit.DurableBatch) (LocalCommit, error) {
	return c.RecordDurable(Frame{
		Kind: KindData, Store: StoreAudit,
		StoreGeneration:    1,
		StoreSequenceFirst: batch.FirstSequence, StoreSequenceLast: batch.LastSequence,
		Payload: batch.Frames,
	})
}

func (c *PrimaryCoordinator) RecordGovernanceBatch(batch governance.DurableBatch) (LocalCommit, error) {
	return c.RecordDurable(Frame{
		Kind: KindData, Store: StoreGovernance,
		StoreGeneration:    1,
		StoreSequenceFirst: batch.FirstSequence, StoreSequenceLast: batch.LastSequence,
		Payload: batch.Frames,
	})
}

func (c *PrimaryCoordinator) RecordMetadataBatch(batch metadatajournal.DurableBatch) (LocalCommit, error) {
	return c.RecordDurable(Frame{
		Kind: KindData, Store: StoreMetadata,
		StoreGeneration:    batch.Epoch,
		StoreSequenceFirst: batch.FirstSequence, StoreSequenceLast: batch.LastSequence,
		Payload: batch.Frames,
	})
}

func (c *PrimaryCoordinator) RecordLedgerRoll(segment ledger.Segment) (LocalCommit, error) {
	metadata, err := ledgerRollMetadata(segment)
	if err != nil {
		return LocalCommit{}, err
	}
	encoded, err := metadata.MarshalBinary()
	if err != nil {
		return LocalCommit{}, err
	}
	return c.RecordDurable(Frame{Kind: KindLedgerRoll, Store: StoreNone, Metadata: encoded})
}

func ledgerRollMetadata(segment ledger.Segment) (LedgerRollMetadata, error) {
	decode := func(name, value string) ([32]byte, error) {
		var result [32]byte
		if value == "" {
			return result, nil
		}
		raw, err := hex.DecodeString(value)
		if err != nil || len(raw) != len(result) {
			return result, errors.New("ledger Segment " + name + " is not a SHA-256 digest")
		}
		copy(result[:], raw)
		return result, nil
	}
	start, err := decode("start_hash", segment.StartHash)
	if err != nil {
		return LedgerRollMetadata{}, err
	}
	end, err := decode("end_hash", segment.EndHash)
	if err != nil {
		return LedgerRollMetadata{}, err
	}
	checksum, err := decode("plain_checksum", segment.PlainChecksum)
	if err != nil {
		return LedgerRollMetadata{}, err
	}
	return LedgerRollMetadata{
		Generation: segment.Generation, FirstSequence: segment.FirstSequence, LastSequence: segment.LastSequence,
		Length: uint64(segment.Length), EndEpoch: uint64(segment.EndEpoch), StartHash: start, EndHash: end,
		PlainChecksum: checksum, SealedAtUnix: segment.SealedAt.UnixNano(),
	}, nil
}

func ledgerSegmentFromMetadata(metadata LedgerRollMetadata) ledger.Segment {
	encodeHash := func(value [32]byte) string {
		if value == ([32]byte{}) {
			return ""
		}
		return hex.EncodeToString(value[:])
	}
	return ledger.Segment{
		Generation: metadata.Generation, File: fmt.Sprintf("ledger-%d.wal", metadata.Generation),
		FirstSequence: metadata.FirstSequence, LastSequence: metadata.LastSequence,
		Length: int64(metadata.Length), StoredLength: int64(metadata.Length),
		StartHash: encodeHash(metadata.StartHash), EndHash: encodeHash(metadata.EndHash),
		EndEpoch: uint8(metadata.EndEpoch), PlainChecksum: hex.EncodeToString(metadata.PlainChecksum[:]),
		StoredChecksum: hex.EncodeToString(metadata.PlainChecksum[:]), SealedAt: time.Unix(0, metadata.SealedAtUnix).UTC(),
	}
}
