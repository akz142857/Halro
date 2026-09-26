package replication

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

// NativeSourceOptions names the four authoritative files and the native keys
// needed to authenticate their frames during startup reconciliation.
type NativeSourceOptions struct {
	LedgerPath, AuditPath, GovernancePath, MetadataPath string
	ProviderObjectDir                                   string
	LedgerKey, AuditKey, GovernanceKey, MetadataKey     []byte
}

// PersistProviderObjectSource keeps encrypted bytes needed to reconstruct an
// authenticated ordering record after the live object is no longer named.
// The spool contains ciphertext only and is pruned with ordering retention.
func PersistProviderObjectSource(directory, name string, sealed []byte) error {
	if err := validateProviderObjectName(name); err != nil {
		return err
	}
	digest := sha256.Sum256(sealed)
	if !filepath.IsAbs(directory) {
		return errors.New("provider-object source directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, name)
	if existing, err := os.ReadFile(path); err == nil {
		if sha256.Sum256(existing) != digest {
			return errors.New("provider-object source already exists with different bytes")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".object-source-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(sealed); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	return durable.SyncDirectory(directory)
}

func (s *NativeSource) ReadProviderObjectChunk(metadata ProviderObjectMetadata) ([]byte, error) {
	if s.options.ProviderObjectDir == "" {
		return nil, errors.New("provider-object source directory is unavailable")
	}
	if err := metadata.Validate(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.options.ProviderObjectDir, metadata.Name)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || uint64(info.Size()) != metadata.TotalLength {
		return nil, errors.New("provider-object source size does not match ordering metadata")
	}
	payload := make([]byte, int(metadata.ChunkLength))
	if len(payload) > 0 {
		if _, err := file.ReadAt(payload, int64(metadata.Offset)); err != nil {
			return nil, err
		}
	}
	if metadata.Final {
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if sha256.Sum256(contents) != metadata.Digest {
			return nil, errors.New("provider-object source digest does not match ordering metadata")
		}
	}
	return payload, nil
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

// RepairReplicaTail is the explicit bootstrap repair step. The expected cursor
// must come from the authenticated ordering prefix; individual Replica open
// paths never infer permission to truncate from their own partial bytes.
func (s *NativeSource) RepairReplicaTail(store Store, cursor StoreCursor) error {
	switch store {
	case StoreLedger:
		return ledger.RepairReplicaTail(s.options.LedgerPath, s.options.LedgerKey, cursor.Generation, cursor.Sequence)
	case StoreAudit:
		if cursor.Generation != 1 {
			return errors.New("audit replication generation must be 1")
		}
		return audit.RepairReplicaTail(s.options.AuditPath, s.options.AuditKey, cursor.Sequence)
	case StoreGovernance:
		if cursor.Generation != 1 {
			return errors.New("governance replication generation must be 1")
		}
		return governance.RepairReplicaTail(s.options.GovernancePath, s.options.GovernanceKey, cursor.Sequence)
	case StoreMetadata:
		return metadatajournal.RepairReplicaTail(s.options.MetadataPath, s.options.MetadataKey, cursor.Generation, cursor.Sequence)
	default:
		return fmt.Errorf("native replication store %d is invalid", store)
	}
}

func (s *NativeSource) RepairReplicaLedgerRoll(cursor StoreCursor) error {
	return ledger.RepairReplicaRoll(s.options.LedgerPath, s.options.LedgerKey, cursor.Generation, cursor.Sequence)
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
