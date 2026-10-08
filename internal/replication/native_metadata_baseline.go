package replication

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replicationlimits"
)

// MetadataBaselineSourceOptions names an independently prepared successor.
// The old file remains read-only reconstruction evidence after activation.
// Exactly one transition is retained; repeated migrations require a new owner.
type MetadataBaselineSourceOptions struct {
	Authority *MetadataBaselineAuthority
	Path      string
	Key       []byte
}

// The successor adds one native decoder and retained frame buffer per fixed
// reader slot. It is charged before opening any of these readers.
const MetadataBaselineReaderRAMBytes int64 = replicationlimits.NativeReaderSets * (MaxPayloadBytes + (1 << 20))

func (s *NativeSource) metadataReaderFor(epoch uint64) (*metadatajournal.ReplicationRangeReader, error) {
	if plan := s.options.MetadataBaseline; plan != nil {
		old := plan.Authority.claims.Cursors[StoreMetadata-StoreLedger]
		if epoch == old.Generation {
			return s.metadataReader, nil
		}
		if epoch == plan.Authority.claims.New.Baseline.Epoch && s.metadataBaselineActive {
			return s.metadataSuccessor, nil
		}
		return nil, errors.New("Metadata source epoch has no activated ordering baseline")
	}
	return s.metadataReader, nil
}
func (s *NativeSource) checkMetadataRead(generation, first, last uint64) error {
	if plan := s.options.MetadataBaseline; plan != nil {
		old := plan.Authority.claims.Cursors[StoreMetadata-StoreLedger]
		if generation == old.Generation {
			if last > old.Sequence {
				return errors.New("old Metadata history exceeds frozen migration cut")
			}
			return nil
		}
		if generation != plan.Authority.claims.New.Baseline.Epoch || !s.metadataBaselineActive {
			return errors.New("new Metadata source has no durable baseline ordering permission")
		}
	}
	return nil
}

func (s *NativeSource) validateMetadataBaselineLocked(record OrderingRecord) error {
	plan := s.options.MetadataBaseline
	if plan == nil {
		return errors.New("Metadata baseline source lacks independent group authority")
	}
	if err := plan.Authority.validateRecord(record); err != nil {
		return err
	}
	c := plan.Authority.claims
	for store := StoreLedger; store <= StoreMetadata; store++ {
		cursor := c.Cursors[store-StoreLedger]
		var head [32]byte
		var err error
		switch store {
		case StoreLedger:
			head, err = ledger.ReplicationHeadAt(s.options.LedgerPath, s.options.LedgerKey, cursor.Generation, cursor.Sequence)
		case StoreAudit:
			head, err = audit.ReplicationHeadAt(s.options.AuditPath, s.options.AuditKey, cursor.Generation, cursor.Sequence)
		case StoreGovernance:
			head, err = governance.ReplicationHeadAt(s.options.GovernancePath, s.options.GovernanceKey, cursor.Generation, cursor.Sequence)
		case StoreMetadata:
			head, err = s.metadataReader.HeadAtContext(context.Background(), cursor.Generation, cursor.Sequence)
		}
		if err != nil || head != c.Heads[store-StoreLedger] {
			return errors.Join(fmt.Errorf("Metadata baseline old source %d differs", store), err)
		}
	}
	generation, sequence, err := s.metadataReader.CursorContext(context.Background())
	if err != nil || (StoreCursor{Generation: generation, Sequence: sequence}) != c.Cursors[StoreMetadata-StoreLedger] {
		return errors.Join(errors.New("old Metadata native file is not frozen at the migration cut"), err)
	}
	newHead, err := s.metadataSuccessor.HeadAtContext(context.Background(), c.New.Baseline.Epoch, 0)
	if err != nil || newHead != c.HeaderHash {
		return errors.Join(errors.New("Metadata successor does not match independently prepared header"), err)
	}
	return nil
}

// ValidateMetadataBaseline authenticates physical old and new evidence before
// reconstruction, local ordering append, or Replica durability can use it.
func (s *NativeSource) ValidateMetadataBaseline(record OrderingRecord) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.retirement.Load() != nil {
		return errSourceRevisionChanged
	}
	err := s.validateMetadataBaselineLocked(record)
	s.recordRetirement(err)
	return err
}

// ActivateMetadataBaseline requires the actual authenticated journal record.
// File presence and a source-fsynced successor cannot advance the epoch.
func (s *NativeSource) ActivateMetadataBaseline(journal *OrderingJournal, index uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.retirement.Load() != nil {
		return errSourceRevisionChanged
	}
	plan := s.options.MetadataBaseline
	if plan == nil {
		return errors.New("Metadata baseline source lacks independent group authority")
	}
	if err := plan.Authority.validateOldCut(journal); err != nil {
		return err
	}
	record, err := journal.Record(index)
	if err != nil {
		return err
	}
	if err = s.validateMetadataBaselineLocked(record); err != nil {
		s.recordRetirement(err)
		return err
	}
	if !s.metadataBaselineActive {
		if err := s.resetRangesLocked(StoreMetadata); err != nil {
			return err
		}
		s.metadataBaselineActive = true
	}
	return nil
}

// ValidateMetadataBaselineOrdering checks the real old authenticated ordering
// prefix before any new record can be appended by Primary or Replica.
func (s *NativeSource) ValidateMetadataBaselineOrdering(journal *OrderingJournal, record OrderingRecord) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.retirement.Load() != nil {
		return errSourceRevisionChanged
	}
	plan := s.options.MetadataBaseline
	if plan == nil {
		return errors.New("Metadata baseline has no independent group permission")
	}
	if err := plan.Authority.validateOldCut(journal); err != nil {
		return err
	}
	head, term, mac := journal.Head()
	if head != plan.Authority.claims.Index || term != plan.Authority.claims.Term || mac != plan.Authority.claims.OrderingHead {
		return errors.New("Metadata baseline must append at its exact authorized continuous old ordering head")
	}
	return s.validateMetadataBaselineLocked(record)
}

// RecoverMetadataBaseline repairs only the independently granted successor at
// the real authenticated ordering cursor. A crash before N+1 leaves old epoch
// active and cannot grant repair/use of the private candidate.
func (s *NativeSource) RecoverMetadataBaseline(journal *OrderingJournal) (resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.retirement.Load() != nil {
		return errSourceRevisionChanged
	}
	plan := s.options.MetadataBaseline
	if plan == nil {
		return nil
	}
	head, _, _ := journal.Head()
	if head <= plan.Authority.claims.Index {
		return nil
	}
	if err := plan.Authority.validateOldCut(journal); err != nil {
		return err
	}
	record, err := journal.Record(plan.Authority.claims.Index + 1)
	if err != nil {
		return err
	}
	if err := plan.Authority.validateRecord(record); err != nil {
		return err
	}
	expected, err := journal.StoreCursors()
	if err != nil {
		return err
	}
	cursor := expected[StoreMetadata-StoreLedger]
	if cursor.Generation != plan.Authority.claims.New.Baseline.Epoch {
		return errors.New("successor repair ordering cursor has another Metadata epoch")
	}
	if err := s.resetRangesLocked(StoreMetadata); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.resetRangesLocked(StoreMetadata)) }()
	generation, sequence, cursorErr := metadatajournal.BoundReplicationCursor(context.Background(), plan.Path, plan.Key, plan.Authority.claims.New)
	if cursorErr != nil {
		return metadatajournal.RepairBoundReplicaTail(context.Background(), plan.Path, plan.Key, plan.Authority.claims.New, cursor.Generation, cursor.Sequence)
	}
	if generation != cursor.Generation || sequence < cursor.Sequence {
		return errors.New("successor physical prefix is behind authenticated ordering")
	}
	if sequence > cursor.Sequence {
		return metadatajournal.TruncateBoundReplicationTail(context.Background(), plan.Path, plan.Key, plan.Authority.claims.New, cursor.Generation, cursor.Sequence)
	}
	return nil
}
