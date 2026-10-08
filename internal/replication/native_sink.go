package replication

import (
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
	ledger           *ledger.Log
	audit            *audit.Log
	governance       *governance.Log
	metadata         *metadatajournal.Log
	metadataBaseline *nativeMetadataBaselineLanding
	objectDir        string
	objectSourceDir  string
}

func NewNativeSink(ledgerLog *ledger.Log, auditLog *audit.Log, governanceLog *governance.Log, metadataLog *metadatajournal.Log, objectDir ...string) (*NativeSink, error) {
	if ledgerLog == nil || auditLog == nil || governanceLog == nil || metadataLog == nil {
		return nil, errors.New("native replication sink requires all four replica logs")
	}
	if _, err := metadataLog.ReplicationVersion(); err != nil {
		return nil, fmt.Errorf("native metadata sink capability: %w", err)
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
	case KindMetadataBaseline:
		return s.persistMetadataBaseline(frame)
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
	if existing, openErr := os.Open(finalPath); openErr == nil {
		closeWith := func(cause error) error { return errors.Join(cause, existing.Close()) }
		if err := verifyProviderObjectFile(existing, metadata.TotalLength, metadata.Digest); err != nil {
			return closeWith(fmt.Errorf("replicated provider object already exists with different bytes: %w", err))
		}
		// Rename may have succeeded before a failed directory fsync. Existence
		// alone cannot authorize an ACK; repeat the file and name barriers.
		if err := existing.Sync(); err != nil {
			return closeWith(err)
		}
		if s.objectSourceDir != "" {
			if err := persistProviderObjectSource(s.objectSourceDir, metadata.Name, io.NewSectionReader(existing, 0, int64(metadata.TotalLength)), metadata.TotalLength, metadata.Digest); err != nil {
				return closeWith(err)
			}
		}
		if err := existing.Close(); err != nil {
			return err
		}
		return syncProviderObjectLiveDirectory(s.objectDir)
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return openErr
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
	if uint64(info.Size()) < metadata.Offset {
		return closeWith(errors.New("provider-object chunk does not continue its staging prefix"))
	}
	if uint64(info.Size()) > metadata.Offset {
		// The native chunk fsync precedes the ordering append. A crash in that
		// gap leaves an unauthorized suffix which the Primary will retransmit.
		// Remove it before replaying this exact frame.
		if err := file.Truncate(int64(metadata.Offset)); err != nil {
			return closeWith(err)
		}
		if err := file.Sync(); err != nil {
			return closeWith(err)
		}
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
	if s.objectSourceDir != "" {
		if err := PersistProviderObjectChunkSource(s.objectSourceDir, metadata, frame.Payload); err != nil {
			return err
		}
	}
	if !metadata.Final {
		return nil
	}
	staged, err := os.Open(stagingPath)
	if err != nil {
		return err
	}
	verifyErr := verifyProviderObjectFile(staged, metadata.TotalLength, metadata.Digest)
	closeErr := staged.Close()
	if verifyErr != nil || closeErr != nil {
		return errors.Join(verifyErr, closeErr)
	}
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return err
	}
	if err := syncProviderObjectLiveDirectory(s.objectDir); err != nil {
		return err
	}
	return nil
}

// Separate live-name barrier so crash/retry tests can fault after rename.
var syncProviderObjectLiveDirectory = durable.SyncDirectory

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
