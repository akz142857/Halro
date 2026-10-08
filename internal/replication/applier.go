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

type ledgerBatchProjection interface {
	ApplyLedgerBatchThrough(context.Context, uint64, uint64) error
	ValidateLedgerForPublication(context.Context) error
}

type ledgerRollProjection interface {
	ApplyLedgerRoll(context.Context, LedgerRollMetadata) error
}

type schemaBoundaryProjection interface {
	ValidateSchemaBoundary(context.Context, uint32, uint32) error
}

// MetadataBaselineProjection applies one confirmed independently qualified
// successor. Native durability alone does not publish its content root.
type MetadataBaselineProjection interface {
	ApplyMetadataBaseline(context.Context, MetadataBaselineMetadata) error
	ApplyMetadataThrough(context.Context, uint64, uint64) error
	ValidateSchemaBoundary(context.Context, uint32, uint32) error
}

type ReplicaApplier struct {
	mu         sync.Mutex
	receiver   *ReplicaReceiver
	journal    *OrderingJournal
	projection ReplicaProjection
	// Local only: complete Ledger substeps can precede whole-prefix publication.
	// Retrying skips only substeps this applier verified in this lifetime.
	ledgerDone uint64
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
	if owner, ok := a.projection.(ReplicaConfirmedPrefixProjection); ok {
		return a.applyConfirmedPrefix(ctx, owner)
	}
	_, confirmed, applied := a.receiver.Progress()
	projectionState := a.receiver.ProjectionProgress()
	if confirmed == applied {
		return applied, nil
	}
	if confirmed < applied {
		return applied, errors.New("replica confirmed index regressed behind applied index")
	}
	var metadataCursor StoreCursor
	flushMetadata := func() error {
		if metadataCursor.Generation == 0 {
			return nil
		}
		if err := a.projection.ApplyMetadataThrough(ctx, metadataCursor.Generation, metadataCursor.Sequence); err != nil {
			return fmt.Errorf("apply confirmed metadata prefix: %w", err)
		}
		projectionState.MetadataEpoch = metadataCursor.Generation
		projectionState.MetadataSequence = metadataCursor.Sequence
		metadataCursor = StoreCursor{}
		return nil
	}
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
			case StoreMetadata:
				metadataCursor = StoreCursor{Generation: record.StoreGeneration, Sequence: record.StoreSequenceLast}
			}
		case KindMetadataBaseline:
			if err := flushMetadata(); err != nil {
				return applied, err
			}
			boundary, err := DecodeMetadataBaselineMetadata(record.Metadata)
			if err != nil {
				return applied, err
			}
			if projectionState.MetadataEpoch != boundary.PreviousEpoch || projectionState.MetadataSequence != boundary.PreviousSequence {
				return applied, errors.New("confirmed Metadata baseline does not continue applied old projection")
			}
			owner, ok := a.projection.(MetadataBaselineProjection)
			if !ok {
				return applied, errors.New("Replica Metadata baseline has no qualified projection owner")
			}
			if err := owner.ApplyMetadataBaseline(ctx, boundary); err != nil {
				return applied, err
			}
			projectionState.MetadataEpoch = boundary.Epoch
			projectionState.MetadataSequence = 0
		case KindLeadershipEstablished:
		case KindLedgerRoll:
			// The sink owns physical publication. The selected Ledger owner
			// must still advance its cut, in the ordered second pass below.
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
	if err := flushMetadata(); err != nil {
		return applied, err
	}
	if err := a.applyConfirmedLedger(ctx, applied, confirmed); err != nil {
		return applied, err
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
	if owner, ok := a.projection.(ledgerBatchProjection); ok {
		if err := owner.ValidateLedgerForPublication(ctx); err != nil {
			return applied, fmt.Errorf("validate confirmed Ledger publication: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return applied, err
	}
	projectionState.Index = confirmed
	if err := a.receiver.AdvanceAppliedWithProjection(confirmed, projectionState); err != nil {
		return applied, err
	}
	return confirmed, nil
}

// Metadata is materialized first, preserving the existing dependency order.
// A second bounded pass applies Ledger batches separated by structural rolls.
// No slice of the confirmed ordering suffix is constructed. Local progress never
// advances member applied_index and is discarded with this applier/generation.
func (a *ReplicaApplier) applyConfirmedLedger(ctx context.Context, applied, confirmed uint64) error {
	var pending StoreCursor
	var pendingIndex uint64
	flush := func() error {
		if pending.Generation == 0 {
			return nil
		}
		apply := a.projection.ApplyLedgerThrough
		if owner, ok := a.projection.(ledgerBatchProjection); ok {
			apply = owner.ApplyLedgerBatchThrough
		}
		if err := apply(ctx, pending.Generation, pending.Sequence); err != nil {
			return fmt.Errorf("apply confirmed Ledger prefix: %w", err)
		}
		a.ledgerDone = pendingIndex
		pending = StoreCursor{}
		return nil
	}
	for index := max(applied, a.ledgerDone) + 1; index <= confirmed; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := a.journal.Record(index)
		if err != nil {
			return err
		}
		if record.Kind == KindData && record.Store == StoreLedger {
			pending = StoreCursor{Generation: record.StoreGeneration, Sequence: record.StoreSequenceLast}
			pendingIndex = index
		}
		if record.Kind == KindLedgerRoll {
			if err := flush(); err != nil {
				return err
			}
			roll, err := DecodeLedgerRollMetadata(record.Metadata)
			if err != nil {
				return err
			}
			owner, ok := a.projection.(ledgerRollProjection)
			if !ok {
				return errors.New("replica projection cannot apply a confirmed Ledger roll")
			}
			if err := owner.ApplyLedgerRoll(ctx, roll); err != nil {
				return fmt.Errorf("apply confirmed Ledger roll: %w", err)
			}
			a.ledgerDone = index
		}
	}
	return flush()
}

// ReplicaLedgerOwner is the sole selected reducer/read owner of the allowed
// Ledger prefix. Callers construct it from authenticated role evidence, exclude
// concurrent mutations, and keep it alive through all applier calls. Derivative
// callbacks must be idempotent: they run before the owner and can be retried if
// a later callback or local transaction fails. This interface grants no trust
// to an existing derived file and does not publish a business generation.
type ReplicaLedgerOwner interface {
	ReadPosition(context.Context) (ledger.Watermark, [32]byte, error)
	Apply(context.Context, ledger.Record) error
	ReadReplayAnchor(context.Context) (ledger.ReplayAnchor, error)
	ApplyVerifiedRoll(context.Context, ledger.VerifiedProjectionRoll) error
}

// The legacy State stores no hash. This compatibility owner retains one native
// authenticated anchor, derived once from its fresh role-permitted startup cut.
// Every later Apply/roll updates it synchronously under NativeProjection.mu.
// It does not make State snapshots trustworthy across restart.
type replicaStateOwner struct {
	state       *ledger.State
	native      *ledger.Log
	anchor      ledger.ReplayAnchor
	initialized bool
}

func (o *replicaStateOwner) ReadPosition(ctx context.Context) (ledger.Watermark, [32]byte, error) {
	return o.state.ReadPosition(ctx)
}
func (o *replicaStateOwner) ReadReplayAnchor(ctx context.Context) (ledger.ReplayAnchor, error) {
	head, _, err := o.state.ReadPosition(ctx)
	if err != nil {
		return ledger.ReplayAnchor{}, err
	}
	if o.initialized {
		if o.anchor.Head != head {
			return ledger.ReplayAnchor{}, errors.New("legacy Replica State changed outside its selected owner")
		}
		return o.anchor, nil
	}
	anchor, err := o.native.AuthenticateAppliedAnchor(ctx, head)
	if err != nil {
		return ledger.ReplayAnchor{}, err
	}
	after, _, err := o.state.ReadPosition(ctx)
	if err != nil {
		return ledger.ReplayAnchor{}, err
	}
	if after != head {
		return ledger.ReplayAnchor{}, errors.New("legacy Replica State changed during native anchor authentication")
	}
	o.anchor = anchor
	o.initialized = true
	return anchor, nil
}
func (o *replicaStateOwner) ApplyVerifiedRoll(ctx context.Context, roll ledger.VerifiedProjectionRoll) error {
	if err := o.state.ApplyVerifiedRoll(ctx, roll); err != nil {
		return err
	}
	anchor, err := roll.NextAnchor()
	if err != nil {
		return err
	}
	o.anchor = anchor
	o.initialized = true
	return nil
}
func (o *replicaStateOwner) Apply(ctx context.Context, record ledger.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.state.Apply(record); err != nil {
		return err
	}
	o.anchor = ledger.ReplayAnchor{Head: ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}, Hash: record.Hash, Epoch: record.Epoch}
	o.initialized = true
	return nil
}

type NativeProjection struct {
	mu                sync.Mutex
	ledgerLog         *ledger.Log
	ledgerOwner       ReplicaLedgerOwner
	needsValidation   bool
	batchValidation   bool
	metadata          *boltstore.Store
	metadataSuccessor MetadataBaselineProjection
	// ledgerDerived receives the same authenticated prefix as ledgerOwner.
	// Replica-only derivatives such as Usage use this hook so promotion does
	// not pay a cold replay while still keeping the Ledger log authoritative.
	ledgerDerived []func(ledger.Record) error
}

func NewNativeProjection(
	ledgerLog *ledger.Log,
	ledgerState *ledger.State,
	metadata *boltstore.Store,
	ledgerDerived ...func(ledger.Record) error,
) (*NativeProjection, error) {
	if ledgerState == nil {
		return nil, errors.New("native replica projection requires a Ledger state")
	}
	return NewNativeProjectionWithLedgerOwner(ledgerLog, &replicaStateOwner{state: ledgerState, native: ledgerLog}, metadata, ledgerDerived...)
}

// NewNativeProjectionWithLedgerOwner permits a freshly rebuilt disk projection
// without retaining a second historical State. Metadata and derivatives remain
// shared resources owned by the caller; this adapter does not close them.
func NewNativeProjectionWithLedgerOwner(
	ledgerLog *ledger.Log,
	owner ReplicaLedgerOwner,
	metadata *boltstore.Store,
	ledgerDerived ...func(ledger.Record) error,
) (*NativeProjection, error) {
	if ledgerLog == nil || owner == nil || metadata == nil {
		return nil, errors.New("native replica projection requires Ledger log/owner and metadata store")
	}
	for _, apply := range ledgerDerived {
		if apply == nil {
			return nil, errors.New("native replica projection ledger derivative cannot be nil")
		}
	}
	return &NativeProjection{
		ledgerLog: ledgerLog, ledgerOwner: owner, metadata: metadata,
		ledgerDerived: append([]func(ledger.Record) error(nil), ledgerDerived...),
	}, nil
}

func (p *NativeProjection) ApplyMetadataThrough(ctx context.Context, epoch, sequence uint64) error {
	if p.metadataSuccessor != nil {
		return p.metadataSuccessor.ApplyMetadataThrough(ctx, epoch, sequence)
	}
	_, err := p.metadata.ApplyReplicaMetadataThrough(ctx, epoch, sequence)
	return err
}

func (p *NativeProjection) ApplyLedgerThrough(ctx context.Context, generation, sequence uint64) error {
	return p.applyLedgerThrough(ctx, generation, sequence, false)
}

// ApplyLedgerBatchThrough validates the target file but defers the remaining
// native suffix to ValidateLedgerForPublication. It is only a substep and never
// sufficient evidence to advance member applied_index on its own.
func (p *NativeProjection) ApplyLedgerBatchThrough(ctx context.Context, generation, sequence uint64) error {
	return p.applyLedgerThrough(ctx, generation, sequence, true)
}

func (p *NativeProjection) applyLedgerThrough(ctx context.Context, generation, sequence uint64, batch bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ledgerLog.Status().Load() == ledger.AccountingRecoveryRequired {
		return errors.New("replica Ledger owner requires fresh native recovery")
	}
	from, _, err := p.ledgerOwner.ReadPosition(ctx)
	if err != nil {
		return err
	}
	if generation == 0 || generation < from.Generation || sequence < from.Sequence {
		return errors.New("replica Ledger apply target is invalid or regressed")
	}
	if sequence == from.Sequence {
		if generation != from.Generation {
			return errors.New("replica Ledger apply target has the same sequence in a different generation")
		}
		if !p.needsValidation {
			return nil
		}
	}
	// Keep Replay's complete file/trailer authentication. Durable records past
	// the permitted cut are inspected but never reduced or sent to observers.
	consume := func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.Sequence > sequence {
			return nil
		}
		if record.Generation > generation || record.Sequence == sequence && record.Generation != generation {
			return errors.New("replica Ledger target is not a native record in the requested generation")
		}
		for _, apply := range p.ledgerDerived {
			if err := apply(record); err != nil {
				return err
			}
		}
		if err := p.ledgerOwner.Apply(ctx, record); err != nil {
			return err
		}
		return nil
	}
	p.needsValidation = true
	if batch {
		p.batchValidation = true
	}
	anchor, anchorErr := p.ledgerOwner.ReadReplayAnchor(ctx)
	if anchorErr != nil {
		return anchorErr
	}
	if anchor.Head != from {
		return errors.New("replica Ledger owner changed during resume capture")
	}
	if batch {
		_, err = p.ledgerLog.ReplayAuthenticatedGeneration(ctx, anchor, generation, consume)
	} else {
		_, err = p.ledgerLog.ReplayAuthenticated(ctx, anchor, consume)
	}
	if err != nil {
		return err
	}
	head, _, err := p.ledgerOwner.ReadPosition(ctx)
	if err != nil {
		return err
	}
	if head.Generation != generation || head.Sequence != sequence {
		return fmt.Errorf("replica Ledger projection ended at %d/%d, want %d/%d", head.Generation, head.Sequence, generation, sequence)
	}
	p.needsValidation = false
	return nil
}

func (p *NativeProjection) ApplyLedgerRoll(ctx context.Context, metadata LedgerRollMetadata) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := metadata.MarshalBinary(); err != nil {
		return err
	}
	if metadata.Length > uint64(^uint64(0)>>1) || metadata.EndEpoch > 255 {
		return errors.New("replica Ledger roll position/epoch exceeds native representation")
	}
	if p.needsValidation {
		// A previous consumer/cancel failure cannot be erased by a roll. Within
		// a batch, only that file's unfinished check is needed here; the final
		// publication barrier still validates the remaining suffix once.
		anchor, err := p.ledgerOwner.ReadReplayAnchor(ctx)
		if err != nil {
			return err
		}
		var replayErr error
		if p.batchValidation && anchor.Head.Generation != 0 {
			_, replayErr = p.ledgerLog.ReplayAuthenticatedGeneration(ctx, anchor, anchor.Head.Generation, nil)
		} else {
			_, replayErr = p.ledgerLog.ReplayAuthenticated(ctx, anchor, nil)
		}
		if replayErr != nil {
			return replayErr
		}
	}
	// Native proof is checked on every attempt, including an idempotent owner
	// retry. It cannot be replaced by metadata or an already-advanced head.
	p.needsValidation = true
	if err := p.ledgerLog.WithVerifiedProjectionRoll(ctx, ledgerSegmentFromMetadata(metadata), func(roll ledger.VerifiedProjectionRoll) error {
		return p.ledgerOwner.ApplyVerifiedRoll(ctx, roll)
	}); err != nil {
		return err
	}
	p.needsValidation = false
	return nil
}

// This barrier always runs before member applied publication, even when local
// ledgerDone skips every substep on a retry. Closed/errorful owners, native
// integrity failures and an unfinished suffix cannot be bypassed by local progress.
func (p *NativeProjection) ValidateLedgerForPublication(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ledgerLog.Status().Load() == ledger.AccountingRecoveryRequired {
		return errors.New("replica Ledger publication requires fresh native recovery")
	}
	from, _, err := p.ledgerOwner.ReadPosition(ctx)
	if err != nil {
		return err
	}
	anchor, err := p.ledgerOwner.ReadReplayAnchor(ctx)
	if err != nil {
		return err
	}
	if anchor.Head != from {
		return errors.New("replica Ledger owner changed during publication validation")
	}
	if _, err := p.ledgerLog.ReplayAuthenticated(ctx, anchor, nil); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.needsValidation = false
	p.batchValidation = false
	return nil
}

func (p *NativeProjection) ValidateSchemaBoundary(ctx context.Context, from, to uint32) error {
	if p.metadataSuccessor != nil {
		return p.metadataSuccessor.ValidateSchemaBoundary(ctx, from, to)
	}
	version, err := p.metadata.SchemaVersion()
	if err != nil {
		return err
	}
	if uint64(to) != version || to <= from {
		return fmt.Errorf("metadata projection schema is %d after boundary %d->%d", version, from, to)
	}
	return nil
}

// ConfigureMetadataBaseline is called before the maintenance transport starts.
// The supplied owner must retain the actual qualified preparation through all
// apply callbacks; this adapter never manufactures permission from its files.
func (p *NativeProjection) ConfigureMetadataBaseline(owner MetadataBaselineProjection) error {
	if owner == nil || p.metadataSuccessor != nil {
		return errors.New("Metadata baseline projection is missing or already configured")
	}
	p.metadataSuccessor = owner
	return nil
}
func (p *NativeProjection) ApplyMetadataBaseline(ctx context.Context, b MetadataBaselineMetadata) error {
	if p.metadataSuccessor == nil {
		return errors.New("Metadata baseline projection has no qualified owner")
	}
	return p.metadataSuccessor.ApplyMetadataBaseline(ctx, b)
}
