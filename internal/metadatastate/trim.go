package metadatastate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// TrimReceipt preserves the exact content root and source provenance. The
// source head is not role permission; caller separately accepts allowed cuts.
type TrimReceipt struct {
	Source, NativeHead, AppliedHead metadatajournal.Head
	State                           metadatajournal.TrimStateBinding
}

// TrimAtApplied rewrites only through the current authenticated applied root,
// never through an unapplied durable tail. It retains actual rewrite Disk/RAM
// fees and owns native exclusion/replacement retirement. It does not reclaim
// old content pages or establish global Seal/HA/production migration authority.
func (b *Baseline) TrimAtApplied(ctx context.Context, expected metadatajournal.Head) (TrimReceipt, error) {
	return b.trimAtApplied(ctx, expected, nil)
}
func (b *Baseline) trimAtApplied(ctx context.Context, expected metadatajournal.Head, phase func(string)) (result TrimReceipt, resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return result, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if b.haPeerTransition != nil || !b.ready.Load() || !b.linked || b.journal == nil {
		return result, authenticatedstate.ErrClosed
	}
	if !b.journal.SelectedStartupStructuralReady() {
		return result, metadatajournal.ErrStartupAuditPending
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if expected != b.journal.Head() || expected.Epoch != b.position.Epoch || expected.FileRevision != b.position.FileRevision || expected.Baseline != b.position.Baseline || b.position.Sequence < expected.TrimmedThrough || b.position.Sequence > expected.Sequence {
		return result, ErrSource
	}
	rewrite := b.position.Sequence != expected.TrimmedThrough
	cost, err := b.journalDisk.Cost()
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	rewriteBytes := int64(0)
	if rewrite {
		rewriteBytes = cost.Disk
	}
	permit, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: DeltaRAMBytes, Disk: rewriteBytes})
	if err != nil {
		return result, err
	}
	succeeded := false
	b.ready.Store(false)
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			b.retirementErr = errors.Join(b.retirementErr, resultErr)
			b.retiredRewrite = permit
		} else {
			resultErr = errors.Join(resultErr, permit.Release())
		}
		b.ready.Store(succeeded && resultErr == nil)
		if resultErr != nil {
			result = TrimReceipt{}
		}
	}()
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		return result, err
	}
	if err := checkSavedRows(ctx, b.store, OpenOptions{BaselineOptions: BaselineOptions{InstanceID: b.evidence.InstanceID}, Evidence: b.evidence, SavedHead: b.position}); err != nil {
		return result, err
	}
	state := metadatajournal.TrimStateBinding{Version: 1, Root: metadatajournal.RootCommitment(root), Sequence: b.position.Sequence, SourceRevision: expected.FileRevision, SourceEndOffset: b.position.Offset, SourceHash: b.position.Hash}
	seen := false
	_, err = b.journal.InspectBoundContext(ctx, func(record metadatajournal.Record) error {
		if record.Kind == metadatajournal.KindTrim {
			if record.Trim.State == nil || *record.Trim.State != b.trimState {
				return ErrSource
			}
		} else if record.Kind == metadatajournal.KindEpoch && record.Header.PreviousEpoch != b.evidence.Metadata.MetadataJournalEpoch {
			return ErrSource
		}
		if record.Kind == metadatajournal.KindOperations {
			if err := validateDelta(record.Ops); err != nil {
				return err
			}
			if record.Sequence == state.Sequence && record.Hash == state.SourceHash {
				state.SourceFrameOffset = record.Offset
				seen = true
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return result, err
	}
	if !rewrite {
		succeeded = true
		return TrimReceipt{Source: expected, NativeHead: expected, AppliedHead: b.position, State: b.trimState}, nil
	}
	if !seen || root.Cut != deltaCut(expected, metadatajournal.Record{Epoch: expected.Epoch, Sequence: state.Sequence, Offset: state.SourceFrameOffset, Hash: state.SourceHash}) {
		return result, ErrSource
	}
	if phase != nil {
		phase("state-verified")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	head, err := b.journal.TrimBoundContext(ctx, expected, state)
	if err != nil {
		return result, err
	}
	if phase != nil {
		phase("journal-published")
	}
	applied := head
	applied.Sequence, applied.Hash = state.Sequence, state.SourceHash
	first := true
	_, err = b.journal.InspectBoundContext(ctx, func(record metadatajournal.Record) error {
		if first {
			if record.Kind != metadatajournal.KindTrim || record.Trim.State == nil || *record.Trim.State != state {
				return ErrSource
			}
			first = false
			return nil
		}
		if record.Sequence == state.Sequence+1 {
			applied.Offset = record.Offset
		}
		return ctx.Err()
	})
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	b.position, b.trimState = applied, state
	result = TrimReceipt{Source: expected, NativeHead: head, AppliedHead: applied, State: state}
	succeeded = true
	return result, nil
}
