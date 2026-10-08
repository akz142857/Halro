package replication

import (
	"errors"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/replicationrange"
)

// One default cursor set serves reconciliation/diagnostic ranges. Each of the
// two possible Replica windows owns one extra set; the pool never grows with
// requests, reconnects, history length or segment count.
const maxNativeRangeReaders = replicationlimits.NativeReaderSets - 1

// A nonzero-sized token gives every explicit source mutation a distinct
// identity, without a wrapping counter or a retained history of revisions.
type sourceRevision struct{ marker byte }

var errSourceRevisionChanged = errors.New("native source revision changed")

type sourceRevisionGuard interface {
	sourceRevision() (*sourceRevision, error)
	deliverAtRevision(*sourceRevision, func() error) error
}

func (r *nativeSourceRangeReader) sourceRevision() (*sourceRevision, error) {
	r.parent.mu.RLock()
	defer r.parent.mu.RUnlock()
	if r.parent.closed || r.closed || r.parent.retirement.Load() != nil {
		return nil, errSourceRevisionChanged
	}
	return r.parent.revision, nil
}

// Keep mutation/close excluded through synchronous queue copy. A cached
// candidate may not cross a known source repair, close or lease replacement.
func (r *nativeSourceRangeReader) deliverAtRevision(revision *sourceRevision, send func() error) error {
	r.parent.mu.RLock()
	defer r.parent.mu.RUnlock()
	if r.parent.closed || r.closed || r.parent.retirement.Load() != nil || revision == nil || revision != r.parent.revision {
		return errSourceRevisionChanged
	}
	return send()
}

type nativeRangeReaders struct {
	auditReader, governanceReader *replicationrange.Reader
	metadataReader                *metadatajournal.ReplicationRangeReader
	metadataSuccessor             *metadatajournal.ReplicationRangeReader
	metadataBoundary              *MetadataBaselineAuthority
	ledgerReader                  *ledger.ReplicationRangeReader
}

func newNativeRangeReaders(options NativeSourceOptions) (readers nativeRangeReaders, err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, readers.close())
		}
	}()
	if readers.auditReader, err = audit.NewReplicationRangeReader(options.AuditPath, options.AuditKey, MaxPayloadBytes); err != nil {
		return readers, err
	}
	if readers.governanceReader, err = governance.NewReplicationRangeReader(options.GovernancePath, options.GovernanceKey, MaxPayloadBytes); err != nil {
		return readers, err
	}
	if options.MetadataAuthority != nil {
		readers.metadataReader, err = metadatajournal.NewBoundReplicationRangeReader(options.MetadataPath, options.MetadataKey, MaxPayloadBytes, *options.MetadataAuthority)
	} else {
		readers.metadataReader, err = metadatajournal.NewReplicationRangeReader(options.MetadataPath, options.MetadataKey, MaxPayloadBytes)
	}
	if err != nil {
		return readers, err
	}
	if p := options.MetadataBaseline; p != nil {
		readers.metadataBoundary = p.Authority
		readers.metadataSuccessor, err = metadatajournal.NewBoundReplicationRangeReader(p.Path, p.Key, MaxPayloadBytes, p.Authority.claims.New)
		if err != nil {
			return readers, err
		}
	}
	readers.ledgerReader, err = ledger.NewReplicationRangeReaderWithIndexLimit(options.LedgerPath, options.LedgerKey, MaxPayloadBytes, options.LedgerDirectoryIndexMaxBytes)
	return readers, err
}

func (r *nativeRangeReaders) read(store Store, generation, first, last uint64) ([]byte, error) {
	switch store {
	case StoreLedger:
		return r.ledgerReader.Read(generation, first, last)
	case StoreAudit:
		if generation != 1 {
			return nil, errors.New("audit replication generation must be 1")
		}
		return r.auditReader.Read(first, last)
	case StoreGovernance:
		if generation != 1 {
			return nil, errors.New("governance replication generation must be 1")
		}
		return r.governanceReader.Read(first, last)
	case StoreMetadata:
		if r.metadataBoundary != nil && generation == r.metadataBoundary.claims.New.Baseline.Epoch {
			return r.metadataSuccessor.Read(generation, first, last)
		}
		return r.metadataReader.Read(generation, first, last)
	default:
		return nil, errors.New("native replication store is invalid")
	}
}

func (r *nativeRangeReaders) reset(store Store) error {
	switch store {
	case StoreLedger:
		return r.ledgerReader.Reset()
	case StoreAudit:
		r.auditReader.Reset()
	case StoreGovernance:
		r.governanceReader.Reset()
	case StoreMetadata:
		r.metadataReader.Reset()
		if r.metadataSuccessor != nil {
			r.metadataSuccessor.Reset()
		}
	}
	return nil
}

func (r *nativeRangeReaders) close() error {
	var err error
	if r.ledgerReader != nil {
		err = errors.Join(err, r.ledgerReader.Close())
	}
	if r.auditReader != nil {
		err = errors.Join(err, r.auditReader.Close())
	}
	if r.governanceReader != nil {
		err = errors.Join(err, r.governanceReader.Close())
	}
	if r.metadataSuccessor != nil {
		err = errors.Join(err, r.metadataSuccessor.Close())
	}
	if r.metadataReader != nil {
		err = errors.Join(err, r.metadataReader.Close())
	}
	return err
}

// nativeSourceRangeReader has no repair/truncate API. Parent mutation and
// close exclude all reads under the same lifetime lock and revoke every set's
// prefix authority, including mutations that return an error.
type nativeSourceRangeReader struct {
	parent   *NativeSource
	readers  nativeRangeReaders
	slot     int
	closed   bool // protected by parent.mu
	closeErr error
}

func (s *NativeSource) acquireRangeReader() (FrameSource, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.retirement.Load() != nil {
		return nil, nil, replicationrange.ErrClosed
	}
	for slot, current := range s.rangeReaders {
		if current != nil {
			continue
		}
		readers, err := newNativeRangeReaders(s.options)
		if err != nil {
			return nil, nil, err
		}
		reader := &nativeSourceRangeReader{parent: s, readers: readers, slot: slot}
		s.rangeReaders[slot] = reader
		return reader, reader.close, nil
	}
	return nil, nil, errors.New("native source has no free Replica range-reader slot")
}

func (r *nativeSourceRangeReader) Read(store Store, generation, first, last uint64) ([]byte, error) {
	r.parent.mu.RLock()
	defer r.parent.mu.RUnlock()
	if r.parent.closed || r.closed || r.parent.retirement.Load() != nil {
		return nil, replicationrange.ErrClosed
	}
	if store == StoreMetadata {
		if err := r.parent.checkMetadataRead(generation, first, last); err != nil {
			return nil, err
		}
	}
	data, err := r.readers.read(store, generation, first, last)
	r.parent.recordRetirement(err)
	return data, err
}

func (r *nativeSourceRangeReader) ReadProviderObjectChunk(metadata ProviderObjectMetadata) ([]byte, error) {
	r.parent.mu.RLock()
	defer r.parent.mu.RUnlock()
	if r.parent.closed || r.closed || r.parent.retirement.Load() != nil {
		return nil, replicationrange.ErrClosed
	}
	return r.parent.readProviderObjectChunk(metadata)
}

func (r *nativeSourceRangeReader) close() {
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.closeLocked()
}

func (r *nativeSourceRangeReader) closeLocked() error {
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	r.closeErr = r.readers.close()
	r.parent.recordRetirement(r.closeErr)
	if r.closeErr != nil {
		return r.closeErr
	}
	r.readers = nativeRangeReaders{}
	if r.parent.rangeReaders[r.slot] == r {
		r.parent.rangeReaders[r.slot] = nil
	}
	return nil
}

func (s *NativeSource) resetRangesLocked(store Store) error {
	s.revision = &sourceRevision{}
	err := s.nativeRangeReaders.reset(store)
	for _, reader := range s.rangeReaders {
		if reader != nil {
			err = errors.Join(err, reader.readers.reset(store))
		}
	}
	s.recordRetirement(err)
	return err
}

func (r *nativeSourceRangeReader) ValidateMetadataBaseline(record OrderingRecord) error {
	return r.parent.ValidateMetadataBaseline(record)
}
