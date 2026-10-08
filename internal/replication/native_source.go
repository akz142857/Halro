package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/replicationrange"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// NativeSourceOptions names the four authoritative files and the native keys
// needed to authenticate their frames during startup reconciliation.
type NativeSourceOptions struct {
	ReserveLedgerFileDisk func(int64) (ledger.NativeFileDiskReservation, error)
	// One aggregate lifetime envelope reserves all fixed reader sets, including
	// idle peer slots and Ledger's retained gzip/descriptor state. Payload/decode
	// temporaries and retained domain projections require separate owners.
	ReaderBudget                                        *resourcebudget.Manager
	ReaderRAMBytes                                      int64
	LedgerDirectoryIndexMaxBytes                        int64
	LedgerPath, AuditPath, GovernancePath, MetadataPath string
	ProviderObjectDir                                   string
	LedgerKey, AuditKey, GovernanceKey, MetadataKey     []byte
	// Independent group baseline qualification. Nil retains format1 only;
	// candidate format2 control fields never manufacture this permission.
	MetadataAuthority *metadatajournal.BoundReplicationAuthority
	MetadataBaseline  *MetadataBaselineSourceOptions
}

// PersistProviderObjectSource keeps encrypted bytes needed to reconstruct an
// authenticated ordering record after the live object is no longer named.
// The spool contains ciphertext only and is pruned with ordering retention.
func PersistProviderObjectSource(directory, name string, sealed []byte) error {
	return persistProviderObjectSource(directory, name, bytes.NewReader(sealed), uint64(len(sealed)), sha256.Sum256(sealed))
}

// Input stays borrowed through synchronous copy/fsync. Both new and existing
// spool files are fully authenticated without a whole-object allocation.
func persistProviderObjectSource(directory, name string, input io.Reader, length uint64, digest [sha256.Size]byte) error {
	if err := validateProviderObjectName(name); err != nil {
		return err
	}
	if !filepath.IsAbs(directory) {
		return errors.New("provider-object source directory must be absolute")
	}
	if err := ensureProviderObjectSourceDirectory(directory); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, name)
	if existing, err := os.Open(path); err == nil {
		verifyErr := verifyProviderObjectFile(existing, length, digest)
		if verifyErr != nil {
			return errors.Join(fmt.Errorf("provider-object source already exists with different bytes: %w", verifyErr), existing.Close())
		}
		syncErr := existing.Sync()
		closeErr := existing.Close()
		return errors.Join(syncErr, closeErr, syncProviderObjectDirectory(directory))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".object-source-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	closeWith := func(cause error) error { return errors.Join(cause, temporary.Close()) }
	if err := temporary.Chmod(0o600); err != nil {
		return closeWith(err)
	}
	if err := copyProviderObject(temporary, input, length, digest); err != nil {
		return closeWith(err)
	}
	if err := temporary.Sync(); err != nil {
		return closeWith(err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	return syncProviderObjectDirectory(directory)
}

// PersistProviderObjectChunkSource makes every durably received chunk
// reconstructable after this Replica is promoted. The source may be a prefix;
// a final chunk authenticates the complete object before returning.
func PersistProviderObjectChunkSource(directory string, metadata ProviderObjectMetadata, payload []byte) error {
	if err := metadata.Validate(); err != nil {
		return err
	}
	if uint64(len(payload)) != metadata.ChunkLength || !filepath.IsAbs(directory) {
		return errors.New("provider-object chunk source is invalid")
	}
	if err := ensureProviderObjectSourceDirectory(directory); err != nil {
		return err
	}
	path := filepath.Join(directory, metadata.Name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	closeWith := func(cause error) error { return errors.Join(cause, file.Close()) }
	if err := file.Chmod(0o600); err != nil {
		return closeWith(err)
	}
	info, err := file.Stat()
	if err != nil {
		return closeWith(err)
	}
	end := metadata.Offset + metadata.ChunkLength
	if uint64(info.Size()) < metadata.Offset {
		return closeWith(errors.New("provider-object source chunk has a prefix gap"))
	}
	if uint64(info.Size()) >= end {
		existing := make([]byte, len(payload))
		if len(existing) > 0 {
			if _, err := file.ReadAt(existing, int64(metadata.Offset)); err != nil {
				return closeWith(err)
			}
		}
		if !equalBytes(existing, payload) {
			return closeWith(errors.New("provider-object source chunk already exists with different bytes"))
		}
	} else {
		// Source chunk fsync precedes ordering append. A retransmitted current
		// chunk may repair only its partial, unauthorised suffix; preserve the
		// earlier prefix and never truncate a complete later chunk.
		if uint64(info.Size()) != metadata.Offset {
			if err := file.Truncate(int64(metadata.Offset)); err != nil {
				return closeWith(err)
			}
			if err := file.Sync(); err != nil {
				return closeWith(err)
			}
		}
		if _, err := file.WriteAt(payload, int64(metadata.Offset)); err != nil {
			return closeWith(err)
		}
	}
	if err := file.Sync(); err != nil {
		return closeWith(err)
	}
	if metadata.Final {
		if err := verifyProviderObjectFile(file, metadata.TotalLength, metadata.Digest); err != nil {
			return closeWith(fmt.Errorf("provider-object source digest does not match final chunk: %w", err))
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncProviderObjectDirectory(directory)
}

var syncProviderObjectDirectory = durable.SyncDirectory

func ensureProviderObjectSourceDirectory(directory string) error {
	if !filepath.IsAbs(directory) {
		return errors.New("provider-object source directory must be absolute")
	}
	info, statErr := os.Lstat(directory)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return statErr
	}
	if statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return errors.New("provider-object source path must be a real directory")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	// Repeat the parent barrier even when the directory already exists. A
	// previous attempt can have created it and then failed its parent fsync;
	// existence alone is therefore not evidence that the name is durable.
	if err := syncProviderObjectDirectory(filepath.Dir(directory)); err != nil {
		return err
	}
	return nil
}

func (s *NativeSource) ReadProviderObjectChunk(metadata ProviderObjectMetadata) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.retirement.Load() != nil {
		return nil, replicationrange.ErrClosed
	}
	return s.readProviderObjectChunk(metadata)
}

func (s *NativeSource) readProviderObjectChunk(metadata ProviderObjectMetadata) (payload []byte, resultErr error) {
	if s.options.ProviderObjectDir == "" {
		return nil, errors.New("provider-object source directory is unavailable")
	}
	if err := metadata.Validate(); err != nil {
		return nil, err
	}
	slot := -1
	if s.providerSlots != nil {
		select {
		case slot = <-s.providerSlots:
		default:
			return nil, errors.New("native provider source has no free reader slot")
		}
	}
	retired := true
	defer func() {
		if retired && slot >= 0 {
			s.providerSlots <- slot
		}
	}()
	path := filepath.Join(s.options.ProviderObjectDir, metadata.Name)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeFile := s.closeProviderFile
		if closeFile == nil {
			closeFile = (*os.File).Close
		}
		if err := closeFile(file); err != nil {
			err = errors.Join(ownedfile.ErrRetirement, err)
			s.recordRetirement(err)
			if slot >= 0 {
				s.retiredProviderFiles[slot] = file
			}
			retired = false
			resultErr = errors.Join(resultErr, err)
		}
		if resultErr != nil {
			payload = nil
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	end := metadata.Offset + metadata.ChunkLength
	if !info.Mode().IsRegular() || uint64(info.Size()) < end || metadata.Final && uint64(info.Size()) != metadata.TotalLength {
		return nil, errors.New("provider-object source size does not match ordering metadata")
	}
	payload = make([]byte, int(metadata.ChunkLength))
	if len(payload) > 0 {
		if _, err := file.ReadAt(payload, int64(metadata.Offset)); err != nil {
			return nil, err
		}
	}
	if metadata.Final {
		if err := verifyProviderObjectFile(file, metadata.TotalLength, metadata.Digest); err != nil {
			return nil, fmt.Errorf("provider-object source digest does not match ordering metadata: %w", err)
		}
	}
	return payload, nil
}

type NativeSource struct {
	mu      sync.RWMutex
	options NativeSourceOptions
	nativeRangeReaders
	rangeReaders           [maxNativeRangeReaders]*nativeSourceRangeReader
	closed                 bool
	metadataBaselineActive bool
	revision               *sourceRevision
	readerPermit           resourcebudget.Permit
	retirement             atomic.Pointer[nativeRetirement]
	closeErr               error
	providerSlots          chan int
	retiredProviderFiles   [replicationlimits.NativeReaderSets]*os.File
	closeProviderFile      func(*os.File) error
}

type NativeSourceConstructionFailure struct {
	Owner *NativeSource
	Cause error
}

func (e *NativeSourceConstructionFailure) Error() string { return e.Cause.Error() }
func (e *NativeSourceConstructionFailure) Unwrap() error { return e.Cause }

func NewNativeSource(options NativeSourceOptions) (*NativeSource, error) {
	if options.MetadataBaseline != nil {
		p := *options.MetadataBaseline
		if p.Authority == nil || !filepath.IsAbs(p.Path) || filepath.Clean(p.Path) == filepath.Clean(options.MetadataPath) || len(p.Key) != 32 || options.MetadataAuthority != nil {
			return nil, errors.New("Metadata baseline source requires separate old history and independent successor")
		}

		options.MetadataBaseline = &p
	}
	if options.MetadataAuthority != nil {
		if err := options.MetadataAuthority.Validate(); err != nil {
			return nil, err
		}
		a := *options.MetadataAuthority
		options.MetadataAuthority = &a
	}
	if options.LedgerPath == "" || options.AuditPath == "" || options.GovernancePath == "" || options.MetadataPath == "" {
		return nil, errors.New("native replication source requires all four store paths")
	}
	if options.LedgerDirectoryIndexMaxBytes < 0 {
		return nil, errors.New("native Ledger directory index disk limit cannot be negative")
	}
	for name, key := range map[string][]byte{
		"ledger": options.LedgerKey, "audit": options.AuditKey,
		"governance": options.GovernanceKey, "metadata": options.MetadataKey,
	} {
		if len(key) != 32 {
			return nil, fmt.Errorf("native replication source %s key must be 32 bytes", name)
		}
	}
	var permit resourcebudget.Permit
	if options.ReaderBudget != nil {
		requiredRAM := replicationlimits.MinNativeReaderRAMBytes
		if options.MetadataBaseline != nil {
			requiredRAM += MetadataBaselineReaderRAMBytes
		}
		if options.ReaderRAMBytes < requiredRAM {
			return nil, errors.New("native reader RAM requires a calibrated envelope covering all three reader sets")
		}
		var err error
		permit, err = options.ReaderBudget.TryAcquireRecovery(resourcebudget.Cost{RAM: options.ReaderRAMBytes})
		if err != nil {
			return nil, fmt.Errorf("native reader RAM admission: %w", err)
		}
	} else if options.ReaderRAMBytes != 0 {
		return nil, errors.New("native reader RAM requires a shared manager")
	}
	options.LedgerKey = append([]byte(nil), options.LedgerKey...)
	options.AuditKey = append([]byte(nil), options.AuditKey...)
	options.GovernanceKey = append([]byte(nil), options.GovernanceKey...)
	options.MetadataKey = append([]byte(nil), options.MetadataKey...)
	if options.MetadataBaseline != nil {
		options.MetadataBaseline.Key = append([]byte(nil), options.MetadataBaseline.Key...)
	}
	readers, err := newNativeRangeReaders(options)
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ledger.ErrSegmentRetirement) {
			owner := &NativeSource{options: options, nativeRangeReaders: readers, readerPermit: permit, closed: true, closeErr: err}
			owner.recordRetirement(err)
			return owner, &NativeSourceConstructionFailure{Owner: owner, Cause: err}
		}
		clear(options.LedgerKey)
		clear(options.AuditKey)
		clear(options.GovernanceKey)
		clear(options.MetadataKey)
		if options.MetadataBaseline != nil {
			clear(options.MetadataBaseline.Key)
		}
		options.LedgerKey, options.AuditKey, options.GovernanceKey, options.MetadataKey = nil, nil, nil, nil
		readers = nativeRangeReaders{}
		if options.ReaderBudget != nil && !errors.Is(err, ownedfile.ErrRetirement) && !errors.Is(err, ledger.ErrSegmentRetirement) {
			err = errors.Join(err, permit.Release())
		}
		return nil, err
	}
	source := &NativeSource{options: options, nativeRangeReaders: readers, revision: &sourceRevision{}, readerPermit: permit, providerSlots: make(chan int, replicationlimits.NativeReaderSets)}
	for slot := range replicationlimits.NativeReaderSets {
		source.providerSlots <- slot
	}
	return source, nil
}

func (s *NativeSource) Cursor(store Store) (StoreCursor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.retirement.Load() != nil {
		return StoreCursor{}, replicationrange.ErrClosed
	}
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
		reader := s.metadataReader
		if s.metadataBaselineActive {
			reader = s.metadataSuccessor
		}
		generation, sequence, err = reader.CursorContext(context.Background())
	default:
		return StoreCursor{}, fmt.Errorf("native replication store %d is invalid", store)
	}
	s.recordRetirement(err)
	return StoreCursor{Generation: generation, Sequence: sequence}, err
}

func (s *NativeSource) HeadAt(store Store, cursor StoreCursor) ([32]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.retirement.Load() != nil {
		return [32]byte{}, replicationrange.ErrClosed
	}
	switch store {
	case StoreLedger:
		return ledger.ReplicationHeadAt(s.options.LedgerPath, s.options.LedgerKey, cursor.Generation, cursor.Sequence)
	case StoreAudit:
		return audit.ReplicationHeadAt(s.options.AuditPath, s.options.AuditKey, cursor.Generation, cursor.Sequence)
	case StoreGovernance:
		return governance.ReplicationHeadAt(s.options.GovernancePath, s.options.GovernanceKey, cursor.Generation, cursor.Sequence)
	case StoreMetadata:
		reader, err := s.metadataReaderFor(cursor.Generation)
		if err != nil {
			return [32]byte{}, err
		}
		digest, err := reader.HeadAtContext(context.Background(), cursor.Generation, cursor.Sequence)
		s.recordRetirement(err)
		return digest, err
	default:
		return [32]byte{}, fmt.Errorf("native replication store %d is invalid", store)
	}
}

func (s *NativeSource) Read(store Store, generation, firstSequence, lastSequence uint64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.retirement.Load() != nil {
		return nil, replicationrange.ErrClosed
	}
	if store == StoreMetadata {
		if err := s.checkMetadataRead(generation, firstSequence, lastSequence); err != nil {
			return nil, err
		}
	}
	data, err := s.nativeRangeReaders.read(store, generation, firstSequence, lastSequence)
	s.recordRetirement(err)
	return data, err
}

func (s *NativeSource) Truncate(store Store, cursor StoreCursor) (resultErr error) {
	// Reconciliation owns this mutation before range consumers start. Never
	// reuse prefix authority across a successful or uncertain tail truncation.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.retirement.Load() != nil {
		return replicationrange.ErrClosed
	}
	if err := s.resetRangesLocked(store); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.resetRangesLocked(store)) }()
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
		if p := s.options.MetadataBaseline; p != nil {
			if !s.metadataBaselineActive {
				old := p.Authority.claims.Cursors[StoreMetadata-StoreLedger]
				if cursor.Generation != old.Generation || cursor.Sequence > old.Sequence {
					return errors.New("Metadata repair exceeds independently authorized old migration cut")
				}
				return metadatajournal.TruncateReplicationTail(s.options.MetadataPath, s.options.MetadataKey, cursor.Generation, cursor.Sequence)
			}
			if cursor.Generation != p.Authority.claims.New.Baseline.Epoch {
				return errors.New("old Metadata migration history is read-only")
			}
			return metadatajournal.TruncateBoundReplicationTail(context.Background(), p.Path, p.Key, p.Authority.claims.New, cursor.Generation, cursor.Sequence)
		}
		if s.options.MetadataAuthority != nil {
			return metadatajournal.TruncateBoundReplicationTail(context.Background(), s.options.MetadataPath, s.options.MetadataKey, *s.options.MetadataAuthority, cursor.Generation, cursor.Sequence)
		}
		return metadatajournal.TruncateReplicationTail(s.options.MetadataPath, s.options.MetadataKey, cursor.Generation, cursor.Sequence)
	default:
		return fmt.Errorf("native replication store %d is invalid", store)
	}
}

// RepairReplicaTail is the explicit bootstrap repair step. The expected cursor
// must come from the authenticated ordering prefix; individual Replica open
// paths never infer permission to truncate from their own partial bytes.
func (s *NativeSource) RepairReplicaTail(store Store, cursor StoreCursor) (resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.retirement.Load() != nil {
		return replicationrange.ErrClosed
	}
	if err := s.resetRangesLocked(store); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.resetRangesLocked(store)) }()
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
		if p := s.options.MetadataBaseline; p != nil {
			if !s.metadataBaselineActive {
				old := p.Authority.claims.Cursors[StoreMetadata-StoreLedger]
				if cursor.Generation != old.Generation || cursor.Sequence > old.Sequence {
					return errors.New("Metadata repair exceeds independently authorized old migration cut")
				}
				return metadatajournal.RepairReplicaTail(s.options.MetadataPath, s.options.MetadataKey, cursor.Generation, cursor.Sequence)
			}
			if cursor.Generation != p.Authority.claims.New.Baseline.Epoch {
				return errors.New("old Metadata migration history is read-only")
			}
			return metadatajournal.RepairBoundReplicaTail(context.Background(), p.Path, p.Key, p.Authority.claims.New, cursor.Generation, cursor.Sequence)
		}
		if s.options.MetadataAuthority != nil {
			return metadatajournal.RepairBoundReplicaTail(context.Background(), s.options.MetadataPath, s.options.MetadataKey, *s.options.MetadataAuthority, cursor.Generation, cursor.Sequence)
		}
		return metadatajournal.RepairReplicaTail(s.options.MetadataPath, s.options.MetadataKey, cursor.Generation, cursor.Sequence)
	default:
		return fmt.Errorf("native replication store %d is invalid", store)
	}
}

func (s *NativeSource) RepairReplicaLedgerRoll(cursor StoreCursor) (resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.retirement.Load() != nil {
		return replicationrange.ErrClosed
	}
	if err := s.resetRangesLocked(StoreLedger); err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, s.resetRangesLocked(StoreLedger))
		s.recordRetirement(resultErr)
	}()
	return ledger.RepairReplicaRollWithDisk(s.options.LedgerPath, s.options.LedgerKey, cursor.Generation, cursor.Sequence, s.options.ReserveLedgerFileDisk)
}

// PendingLedgerRoll returns the authenticated structural metadata left by a
// native Roll whose ordering record was not made durable before a crash.
func (s *NativeSource) PendingLedgerRoll(generation uint64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.retirement.Load() != nil {
		return nil, replicationrange.ErrClosed
	}
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

type nativeRetirement struct{ err error }

// The first actual IO failure withdraws all three source reader sets. This
// fixed latch is safe under concurrent admitted reads without upgrading mu.
func (s *NativeSource) recordRetirement(err error) {
	if !errors.Is(err, ownedfile.ErrRetirement) && !errors.Is(err, ledger.ErrSegmentRetirement) {
		return
	}
	if s.retirement.Load() == nil {
		s.retirement.CompareAndSwap(nil, &nativeRetirement{errors.Join(ownedfile.ErrRetirement, err)})
	}
}

func (s *NativeSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if debt := s.retirement.Load(); debt != nil {
		s.closeErr = debt.err
	}
	s.closeErr = errors.Join(s.closeErr, s.nativeRangeReaders.close())
	for _, reader := range s.rangeReaders {
		if reader != nil {
			s.closeErr = errors.Join(s.closeErr, reader.closeLocked())
		}
	}
	if s.closeErr != nil {
		s.recordRetirement(s.closeErr)
		return s.closeErr
	}
	clear(s.options.LedgerKey)
	clear(s.options.AuditKey)
	clear(s.options.GovernanceKey)
	clear(s.options.MetadataKey)
	if s.options.MetadataBaseline != nil {
		clear(s.options.MetadataBaseline.Key)
		s.options.MetadataBaseline.Key = nil
	}
	s.options.LedgerKey, s.options.AuditKey, s.options.GovernanceKey, s.options.MetadataKey = nil, nil, nil, nil
	s.nativeRangeReaders = nativeRangeReaders{}
	s.providerSlots = nil
	s.revision = nil
	if s.options.ReaderBudget != nil {
		s.closeErr = s.readerPermit.Release()
		if s.closeErr == nil {
			s.readerPermit = resourcebudget.Permit{}
			s.options.ReaderBudget = nil
		}
	}
	return s.closeErr
}
