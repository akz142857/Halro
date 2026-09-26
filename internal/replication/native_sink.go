package replication

import (
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

// NativeSink lands physical source frames into Replica-owned native logs.
// Each log validates its own MAC/chain before any byte is written and fsyncs
// before returning, so ReplicaReceiver may safely emit the durable ACK next.
type NativeSink struct {
	ledger     *ledger.Log
	audit      *audit.Log
	governance *governance.Log
	metadata   *metadatajournal.Log
}

func NewNativeSink(ledgerLog *ledger.Log, auditLog *audit.Log, governanceLog *governance.Log, metadataLog *metadatajournal.Log) (*NativeSink, error) {
	if ledgerLog == nil || auditLog == nil || governanceLog == nil || metadataLog == nil {
		return nil, errors.New("native replication sink requires all four replica logs")
	}
	return &NativeSink{ledger: ledgerLog, audit: auditLog, governance: governanceLog, metadata: metadataLog}, nil
}

func (s *NativeSink) Persist(frame Frame) error {
	switch frame.Kind {
	case KindLeadershipEstablished, KindSchemaBoundary:
		return nil
	case KindLedgerRoll:
		metadata, err := DecodeLedgerRollMetadata(frame.Metadata)
		if err != nil {
			return err
		}
		_, err = s.ledger.ApplyReplicatedRoll(ledgerSegmentFromMetadata(metadata))
		return err
	case KindData:
	default:
		return fmt.Errorf("native sink does not recognize frame kind %d", frame.Kind)
	}
	switch frame.Store {
	case StoreLedger:
		return s.ledger.AppendReplicated(frame.StoreGeneration, frame.StoreSequenceFirst, frame.StoreSequenceLast, frame.Payload)
	case StoreAudit:
		return s.audit.AppendReplicated(frame.StoreSequenceFirst, frame.StoreSequenceLast, frame.Payload)
	case StoreGovernance:
		return s.governance.AppendReplicated(frame.StoreSequenceFirst, frame.StoreSequenceLast, frame.Payload)
	case StoreMetadata:
		return s.metadata.AppendReplicated(frame.StoreGeneration, frame.StoreSequenceFirst, frame.StoreSequenceLast, frame.Payload)
	default:
		return fmt.Errorf("native sink does not recognize store %d", frame.Store)
	}
}
