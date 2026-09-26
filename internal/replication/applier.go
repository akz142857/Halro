package replication

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// ReplicaProjection applies already-durable native prefixes. Calls may batch
// several replication indexes, but must return only after the derived state is
// durable/visible locally.
type ReplicaProjection interface {
	ApplyLedgerThrough(context.Context, uint64, uint64) error
	ApplyMetadataThrough(context.Context, uint64, uint64) error
}

type schemaBoundaryProjection interface {
	ValidateSchemaBoundary(context.Context, uint32, uint32) error
}

type ReplicaApplier struct {
	mu         sync.Mutex
	receiver   *ReplicaReceiver
	journal    *OrderingJournal
	projection ReplicaProjection
}

func NewReplicaApplier(receiver *ReplicaReceiver, journal *OrderingJournal, projection ReplicaProjection) (*ReplicaApplier, error) {
	if receiver == nil || journal == nil || projection == nil {
		return nil, errors.New("replica applier requires receiver, ordering journal and projection")
	}
	return &ReplicaApplier{receiver: receiver, journal: journal, projection: projection}, nil
}

// ApplyConfirmed batches the complete newly-confirmed prefix. It never reads
// durable_index as an apply limit: frames may be safely on disk and still be
// ineligible because no caller was allowed to observe them as committed.
func (a *ReplicaApplier) ApplyConfirmed(ctx context.Context) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, confirmed, applied := a.receiver.Progress()
	projectionState := a.receiver.ProjectionProgress()
	if confirmed == applied {
		return applied, nil
	}
	if confirmed < applied {
		return applied, errors.New("replica confirmed index regressed behind applied index")
	}
	var ledgerCursor, metadataCursor StoreCursor
	var firstSchema, lastSchema uint32
	for index := applied + 1; index <= confirmed; index++ {
		if err := ctx.Err(); err != nil {
			return applied, err
		}
		record, err := a.journal.Record(index)
		if err != nil {
			return applied, err
		}
		switch record.Kind {
		case KindData:
			switch record.Store {
			case StoreLedger:
				ledgerCursor = StoreCursor{Generation: record.StoreGeneration, Sequence: record.StoreSequenceLast}
			case StoreMetadata:
				metadataCursor = StoreCursor{Generation: record.StoreGeneration, Sequence: record.StoreSequenceLast}
			}
		case KindLeadershipEstablished:
		case KindLedgerRoll:
			// The structural sink already published the verified generation
			// before its durable ACK. There is no semantic Ledger record to apply.
		case KindProviderObject:
			// The sink publishes an authenticated object before acknowledging its
			// final chunk. Metadata that names it is ordered after this record.
		case KindSchemaBoundary:
			boundary, err := DecodeSchemaBoundaryMetadata(record.Metadata)
			if err != nil {
				return applied, err
			}
			if firstSchema == 0 {
				firstSchema = boundary.From
			} else if lastSchema != boundary.From {
				return applied, errors.New("replica schema boundaries are not contiguous")
			}
			lastSchema = boundary.To
		default:
			return applied, fmt.Errorf("replica apply found unknown ordering kind %d", record.Kind)
		}
	}
	// Metadata and Ledger are the two stores with derived in-memory/on-disk
	// projections. Audit and Governance frames are already usable in place.
	if metadataCursor.Generation != 0 {
		if err := a.projection.ApplyMetadataThrough(ctx, metadataCursor.Generation, metadataCursor.Sequence); err != nil {
			return applied, fmt.Errorf("apply confirmed metadata prefix: %w", err)
		}
		projectionState.MetadataEpoch = metadataCursor.Generation
		projectionState.MetadataSequence = metadataCursor.Sequence
	}
	if ledgerCursor.Generation != 0 {
		if err := a.projection.ApplyLedgerThrough(ctx, ledgerCursor.Generation, ledgerCursor.Sequence); err != nil {
			return applied, fmt.Errorf("apply confirmed Ledger prefix: %w", err)
		}
	}
	if firstSchema != 0 {
		validator, ok := a.projection.(schemaBoundaryProjection)
		if !ok {
			return applied, errors.New("replica projection cannot validate a schema boundary")
		}
		if err := validator.ValidateSchemaBoundary(ctx, firstSchema, lastSchema); err != nil {
			return applied, fmt.Errorf("validate confirmed schema boundary: %w", err)
		}
	}
	projectionState.Index = confirmed
	if err := a.receiver.AdvanceAppliedWithProjection(confirmed, projectionState); err != nil {
		return applied, err
	}
	return confirmed, nil
}

type NativeProjection struct {
	ledgerLog   *ledger.Log
	ledgerState *ledger.State
	metadata    *boltstore.Store
}

func NewNativeProjection(ledgerLog *ledger.Log, ledgerState *ledger.State, metadata *boltstore.Store) (*NativeProjection, error) {
	if ledgerLog == nil || ledgerState == nil || metadata == nil {
		return nil, errors.New("native replica projection requires Ledger log/state and metadata store")
	}
	return &NativeProjection{ledgerLog: ledgerLog, ledgerState: ledgerState, metadata: metadata}, nil
}

func (p *NativeProjection) ApplyMetadataThrough(ctx context.Context, epoch, sequence uint64) error {
	_, err := p.metadata.ApplyReplicaMetadataThrough(ctx, epoch, sequence)
	return err
}

func (p *NativeProjection) ApplyLedgerThrough(ctx context.Context, generation, sequence uint64) error {
	from := p.ledgerState.Watermark()
	if sequence < from.Sequence {
		return errors.New("replica Ledger apply target regressed")
	}
	if sequence == from.Sequence {
		return nil
	}
	_, err := p.ledgerLog.Replay(from, func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.Sequence <= sequence {
			return p.ledgerState.Apply(record)
		}
		return nil
	})
	if err != nil {
		return err
	}
	head := p.ledgerState.Watermark()
	if head.Generation != generation || head.Sequence != sequence {
		return fmt.Errorf("replica Ledger projection ended at %d/%d, want %d/%d", head.Generation, head.Sequence, generation, sequence)
	}
	return nil
}

func (p *NativeProjection) ValidateSchemaBoundary(_ context.Context, from, to uint32) error {
	version, err := p.metadata.SchemaVersion()
	if err != nil {
		return err
	}
	if uint64(to) != version || to <= from {
		return fmt.Errorf("metadata projection schema is %d after boundary %d->%d", version, from, to)
	}
	return nil
}
