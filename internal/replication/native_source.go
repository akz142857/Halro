package replication

import (
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

// NativeSourceOptions names the four authoritative files and the native keys
// needed to authenticate their frames during startup reconciliation.
type NativeSourceOptions struct {
	LedgerPath, AuditPath, GovernancePath, MetadataPath string
	LedgerKey, AuditKey, GovernanceKey, MetadataKey     []byte
}

type NativeSource struct {
	options NativeSourceOptions
}

func NewNativeSource(options NativeSourceOptions) (*NativeSource, error) {
	if options.LedgerPath == "" || options.AuditPath == "" || options.GovernancePath == "" || options.MetadataPath == "" {
		return nil, errors.New("native replication source requires all four store paths")
	}
	for name, key := range map[string][]byte{
		"ledger": options.LedgerKey, "audit": options.AuditKey,
		"governance": options.GovernanceKey, "metadata": options.MetadataKey,
	} {
		if len(key) != 32 {
			return nil, fmt.Errorf("native replication source %s key must be 32 bytes", name)
		}
	}
	options.LedgerKey = append([]byte(nil), options.LedgerKey...)
	options.AuditKey = append([]byte(nil), options.AuditKey...)
	options.GovernanceKey = append([]byte(nil), options.GovernanceKey...)
	options.MetadataKey = append([]byte(nil), options.MetadataKey...)
	return &NativeSource{options: options}, nil
}

func (s *NativeSource) Cursor(store Store) (StoreCursor, error) {
	var generation, sequence uint64
	var err error
	switch store {
	case StoreLedger:
		generation, sequence, err = ledger.ReplicationCursor(s.options.LedgerPath, s.options.LedgerKey)
	case StoreAudit:
		generation, sequence, err = audit.ReplicationCursor(s.options.AuditPath, s.options.AuditKey)
	case StoreGovernance:
		generation, sequence, err = governance.ReplicationCursor(s.options.GovernancePath, s.options.GovernanceKey)
	case StoreMetadata:
		generation, sequence, err = metadatajournal.ReplicationCursor(s.options.MetadataPath, s.options.MetadataKey)
	default:
		return StoreCursor{}, fmt.Errorf("native replication store %d is invalid", store)
	}
	return StoreCursor{Generation: generation, Sequence: sequence}, err
}

func (s *NativeSource) HeadAt(store Store, cursor StoreCursor) ([32]byte, error) {
	switch store {
	case StoreLedger:
		return ledger.ReplicationHeadAt(s.options.LedgerPath, s.options.LedgerKey, cursor.Generation, cursor.Sequence)
	case StoreAudit:
		return audit.ReplicationHeadAt(s.options.AuditPath, s.options.AuditKey, cursor.Generation, cursor.Sequence)
	case StoreGovernance:
		return governance.ReplicationHeadAt(s.options.GovernancePath, s.options.GovernanceKey, cursor.Generation, cursor.Sequence)
	case StoreMetadata:
		return metadatajournal.ReplicationHeadAt(s.options.MetadataPath, s.options.MetadataKey, cursor.Generation, cursor.Sequence)
	default:
		return [32]byte{}, fmt.Errorf("native replication store %d is invalid", store)
	}
}

func (s *NativeSource) Read(store Store, generation, firstSequence, lastSequence uint64) ([]byte, error) {
	switch store {
	case StoreLedger:
		return ledger.ReadReplicationFrames(s.options.LedgerPath, s.options.LedgerKey, generation, firstSequence, lastSequence)
	case StoreAudit:
		if generation != 1 {
			return nil, errors.New("audit replication generation must be 1")
		}
		return audit.ReadReplicationFrames(s.options.AuditPath, s.options.AuditKey, firstSequence, lastSequence)
	case StoreGovernance:
		if generation != 1 {
			return nil, errors.New("governance replication generation must be 1")
		}
		return governance.ReadReplicationFrames(s.options.GovernancePath, s.options.GovernanceKey, firstSequence, lastSequence)
	case StoreMetadata:
		return metadatajournal.ReadReplicationFrames(s.options.MetadataPath, s.options.MetadataKey, generation, firstSequence, lastSequence)
	default:
		return nil, fmt.Errorf("native replication store %d is invalid", store)
	}
}

func (s *NativeSource) Truncate(store Store, cursor StoreCursor) error {
	switch store {
	case StoreLedger:
		return ledger.TruncateReplicationTail(s.options.LedgerPath, s.options.LedgerKey, cursor.Generation, cursor.Sequence)
	case StoreAudit:
		if cursor.Generation != 1 {
			return errors.New("audit replication generation must be 1")
		}
		return audit.TruncateReplicationTail(s.options.AuditPath, s.options.AuditKey, cursor.Sequence)
	case StoreGovernance:
		if cursor.Generation != 1 {
			return errors.New("governance replication generation must be 1")
		}
		return governance.TruncateReplicationTail(s.options.GovernancePath, s.options.GovernanceKey, cursor.Sequence)
	case StoreMetadata:
		return metadatajournal.TruncateReplicationTail(s.options.MetadataPath, s.options.MetadataKey, cursor.Generation, cursor.Sequence)
	default:
		return fmt.Errorf("native replication store %d is invalid", store)
	}
}

// PendingLedgerRoll returns the authenticated structural metadata left by a
// native Roll whose ordering record was not made durable before a crash.
func (s *NativeSource) PendingLedgerRoll(generation uint64) ([]byte, error) {
	segment, err := ledger.ReplicationSegment(s.options.LedgerPath, s.options.LedgerKey, generation)
	if err != nil {
		return nil, err
	}
	metadata, err := ledgerRollMetadata(segment)
	if err != nil {
		return nil, err
	}
	return metadata.MarshalBinary()
}

func (s *NativeSource) Close() {
	clear(s.options.LedgerKey)
	clear(s.options.AuditKey)
	clear(s.options.GovernanceKey)
	clear(s.options.MetadataKey)
}
