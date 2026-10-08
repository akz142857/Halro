package metadatastate

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/store/lock"
)

// ResumeHASelectedRoot joins the actual selected read child before returning
// its authenticated historical root to the original mutable source owner. It
// preserves the source's actual flock and complete physical COW/native tails.
// The caller must hold independent Member/Ordering permission for authority;
// this handoff neither reduces a suffix nor grants business readiness.
func (b *Baseline) ResumeHASelectedRoot(ctx context.Context, selected *Baseline, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, authority authenticatedstate.SealEligibility) (resultErr error) {
	if b == nil || selected == nil || b == selected || publisher == nil || guard == nil || !b.mu.TryLock() {
		return ErrSource
	}
	defer b.mu.Unlock()
	if b.readOnly || !b.ready.Load() || !b.linked || b.store == nil || b.journal == nil || b.selectedStartup || selected.store == nil || !selected.readOnly || b.output != selected.output || b.journalPath != selected.journalPath || b.evidence != selected.evidence || b.budget != selected.budget || authority.Instance != b.evidence.Identity {
		return ErrSource
	}
	switch authority.Role {
	case authenticatedstate.SealPrimary:
		if b.haPrimaryPrepared == nil {
			return ErrSource
		}
	case authenticatedstate.SealReplica:
		if b.haReplicaPrepared == nil {
			return ErrSource
		}
	default:
		return ErrSource
	}
	if err := publisher.ValidateOwner(guard, b.budget); err != nil {
		return err
	}
	common, found, err := publisher.Select(authority)
	if err != nil || !found {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if !selected.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	ref, err := selected.sealReferenceLocked(ctx)
	saved := selected.position
	selected.mu.Unlock()
	if err != nil || ref.Position != authority.Positions[1] || metadataSealPosition(saved) != ref.Position {
		return errors.Join(ErrSource, err)
	}
	matched := false
	for i := uint8(0); i < common.RootCount; i++ {
		if common.Roots[i].Component == authenticatedstate.SealComponentMetadata {
			matched = common.Roots[i] == ref.Root && (authenticatedstate.SealRootSchema{Kind: ref.Root.Kind, Component: ref.Root.Component, IndexID: ref.Root.IndexID, Codec: ref.Root.Codec}) == ref.Schema
		}
	}
	if !matched {
		return authenticatedstate.ErrSealIneligible
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	b.ready.Store(false)
	resultErr = b.journal.WithFrozenHead(opCtx, b.budget, func(ctx context.Context, head metadatajournal.Head) error {
		if head.Baseline != saved.Baseline || head.FileRevision != saved.FileRevision || head.Epoch != saved.Epoch || head.Sequence < saved.Sequence || head.Offset < saved.Offset || saved.Sequence == head.Sequence && (saved.Hash != head.Hash || saved.Offset != head.Offset) {
			return ErrSource
		}
		if err := b.store.JoinBorrowedReadComponent(selected.store); err != nil {
			return err
		}
		if err := b.store.AdoptClosedSelectedReadRoot(ctx, selected.store, ref.Root, ref.Schema, ref.Root.State.Cut); err != nil {
			return err
		}
		b.position = saved
		actual, err := b.sealReferenceLockedMode(ctx, false)
		if err != nil || actual != ref {
			return errors.Join(ErrSource, err)
		}
		current, found, err := publisher.Select(authority)
		if err != nil || !found || current != common {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		return ctx.Err()
	})
	if resultErr == nil {
		b.ready.Store(true)
	}
	return resultErr
}
