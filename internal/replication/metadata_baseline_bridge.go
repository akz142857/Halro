package replication

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

type metadataBaselineSource interface {
	ValidateMetadataBaseline(OrderingRecord) error
	ValidateMetadataBaselineOrdering(*OrderingJournal, OrderingRecord) error
	ActivateMetadataBaseline(*OrderingJournal, uint64) error
}

// ConfigureMetadataBaseline binds a prepared physical successor to the actual
// Replica sink. It does not activate it; Receiver commits ordering first.
func (s *NativeSink) ConfigureMetadataBaseline(a *MetadataBaselineAuthority, source *NativeSource, successor *metadatajournal.Log) error {
	if a == nil || source == nil || successor == nil || s.metadataBaseline != nil {
		return errors.New("Replica Metadata baseline owner is missing or already installed")
	}
	head := successor.Head()
	if head.FormatVersion != 2 || head.Epoch != a.claims.New.Baseline.Epoch || head.Baseline != a.claims.New.Baseline || head.FileRevision != a.claims.New.FileRevision {
		return errors.New("Replica successor differs from independent file permission")
	}
	if _, err := successor.ReplicationVersion(); err != nil {
		return err
	}
	s.metadataBaseline = &nativeMetadataBaselineLanding{authority: a, source: source, successor: successor}
	source.mu.RLock()
	active := source.metadataBaselineActive
	source.mu.RUnlock()
	if active {
		s.metadata = successor
	}
	return nil
}

type nativeMetadataBaselineLanding struct {
	authority *MetadataBaselineAuthority
	source    *NativeSource
	successor *metadatajournal.Log
}

func (s *NativeSink) persistMetadataBaseline(frame Frame) error {
	b := s.metadataBaseline
	if b == nil {
		return errors.New("Replica Metadata baseline has no independent preparation owner")
	}
	return b.source.ValidateMetadataBaseline(OrderingRecord{Kind: frame.Kind, Index: frame.Index, Term: frame.Term, ConfirmedIndex: frame.ConfirmedIndex, Metadata: frame.Metadata})
}
func (s *NativeSink) OrderingCommitted(journal *OrderingJournal, record OrderingRecord) error {
	if record.Kind != KindMetadataBaseline {
		return nil
	}
	if s.metadataBaseline == nil {
		return errors.New("Replica Metadata baseline has no independent preparation owner")
	}
	if err := s.metadataBaseline.source.ActivateMetadataBaseline(journal, record.Index); err != nil {
		return err
	}
	s.metadata = s.metadataBaseline.successor
	return nil
}

// RecordMetadataBaseline confirms the existing cut before ordering its one
// authorized successor. No term, incarnation or global index is reinitialized.
func (c *PrimaryCoordinator) RecordMetadataBaseline(ctx context.Context, a *MetadataBaselineAuthority) (LocalCommit, error) {
	if a == nil {
		return LocalCommit{}, errors.New("Primary Metadata baseline requires independently qualified authority")
	}
	if err := c.WaitConfirmed(ctx, a.claims.Index); err != nil {
		return LocalCommit{}, err
	}
	metadata, err := a.Descriptor().MarshalBinary()
	if err != nil {
		return LocalCommit{}, err
	}
	commit, err := c.RecordDurable(Frame{Kind: KindMetadataBaseline, Metadata: metadata})
	if err != nil {
		return LocalCommit{}, err
	}
	if commit.Index != a.claims.Index+1 {
		return LocalCommit{}, errors.New("Primary Metadata boundary index differs from authorized old cut")
	}
	if err := c.WaitConfirmed(ctx, commit.Index); err != nil {
		return commit, fmt.Errorf("confirm Metadata baseline boundary: %w", err)
	}
	return commit, nil
}

func (s *NativeSink) ValidateMetadataBaselineOrdering(journal *OrderingJournal, frame Frame) error {
	if s.metadataBaseline == nil {
		return errors.New("Metadata baseline has no independent Replica landing owner")
	}
	return s.metadataBaseline.source.ValidateMetadataBaselineOrdering(journal, OrderingRecord{Kind: frame.Kind, Index: frame.Index, Term: frame.Term, ConfirmedIndex: frame.ConfirmedIndex, Metadata: frame.Metadata})
}
