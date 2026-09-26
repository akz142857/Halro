package replication

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

// NativeSink lands physical source frames into Replica-owned native logs.
// Each log validates its own MAC/chain before any byte is written and fsyncs
// before returning, so ReplicaReceiver may safely emit the durable ACK next.
type NativeSink struct {
	ledger          *ledger.Log
	audit           *audit.Log
	governance      *governance.Log
	metadata        *metadatajournal.Log
	objectDir       string
	objectSourceDir string
}

func NewNativeSink(ledgerLog *ledger.Log, auditLog *audit.Log, governanceLog *governance.Log, metadataLog *metadatajournal.Log, objectDir ...string) (*NativeSink, error) {
	if ledgerLog == nil || auditLog == nil || governanceLog == nil || metadataLog == nil {
		return nil, errors.New("native replication sink requires all four replica logs")
	}
	if len(objectDir) > 2 {
		return nil, errors.New("native replication sink accepts a live and source provider-object directory")
	}
	directory := ""
	if len(objectDir) >= 1 {
		directory = filepath.Clean(objectDir[0])
		if !filepath.IsAbs(directory) {
			return nil, errors.New("native replication provider-object directory must be absolute")
		}
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create replicated provider-object directory: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, err
		}
	}
	sourceDirectory := ""
	if len(objectDir) == 2 {
		sourceDirectory = filepath.Clean(objectDir[1])
		if !filepath.IsAbs(sourceDirectory) {
			return nil, errors.New("native replication provider-object source directory must be absolute")
		}
	}
	return &NativeSink{
		ledger: ledgerLog, audit: auditLog, governance: governanceLog, metadata: metadataLog,
		objectDir: directory, objectSourceDir: sourceDirectory,
	}, nil
}

func (s *NativeSink) Persist(frame Frame) error {
	switch frame.Kind {
	case KindLeadershipEstablished, KindSchemaBoundary:
		return nil
	case KindProviderObject:
		return s.persistProviderObject(frame)
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

func (s *NativeSink) persistProviderObject(frame Frame) error {
	if s.objectDir == "" {
		return errors.New("replica provider-object directory is unavailable")
	}
	metadata, err := DecodeProviderObjectMetadata(frame.Metadata)
	if err != nil {
		return err
	}
	finalPath := filepath.Join(s.objectDir, metadata.Name)
	if existing, readErr := os.ReadFile(finalPath); readErr == nil {
		if uint64(len(existing)) != metadata.TotalLength || sha256.Sum256(existing) != metadata.Digest {
			return errors.New("replicated provider object already exists with different bytes")
		}
		if s.objectSourceDir != "" {
			return PersistProviderObjectSource(s.objectSourceDir, metadata.Name, existing)
		}
		return nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	stagingPath := filepath.Join(s.objectDir, ".replicating-"+fmt.Sprintf("%x", metadata.Digest[:]))
	flags := os.O_WRONLY | os.O_CREATE
	if metadata.Offset == 0 {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(stagingPath, flags, 0o600)
	if err != nil {
		return err
	}
	closeWith := func(cause error) error { return errors.Join(cause, file.Close()) }
	info, err := file.Stat()
	if err != nil {
		return closeWith(err)
	}
	if uint64(info.Size()) != metadata.Offset {
		return closeWith(errors.New("provider-object chunk does not continue its staging prefix"))
	}
	if _, err := file.Seek(int64(metadata.Offset), io.SeekStart); err != nil {
		return closeWith(err)
	}
	if len(frame.Payload) > 0 {
		if _, err := file.Write(frame.Payload); err != nil {
			return closeWith(err)
		}
	}
	if err := file.Sync(); err != nil {
		return closeWith(err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if !metadata.Final {
		return nil
	}
	staged, err := os.Open(stagingPath)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, staged)
	closeErr := staged.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if uint64(written) != metadata.TotalLength || !equalBytes(hash.Sum(nil), metadata.Digest[:]) {
		return errors.New("replicated provider-object digest does not match its receipt")
	}
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return err
	}
	if err := durable.SyncDirectory(s.objectDir); err != nil {
		return err
	}
	if s.objectSourceDir != "" {
		contents, err := os.ReadFile(finalPath)
		if err != nil {
			return err
		}
		if err := PersistProviderObjectSource(s.objectSourceDir, metadata.Name, contents); err != nil {
			return err
		}
	}
	return nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for i := range left {
		different |= left[i] ^ right[i]
	}
	return different == 0
}
