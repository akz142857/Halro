package metadatastate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// PrepareNativeRotationArchive captures this actual independently reopened
// native epoch. The successor baseline never grants permission to delete it.
func (b *Baseline) PrepareNativeRotationArchive(expected metadatajournal.Head, destination string, guard *lock.Lock) (*metadatajournal.NativeArchive, error) {
	if b == nil || !b.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if (!b.ready.Load() && !(b.readOnly && b.linked && b.ctx.Err() == nil)) || b.position != expected || b.journal == nil {
		return nil, ErrSource
	}
	return b.journal.PrepareNativeArchive(expected, destination, guard)
}

func (b *Baseline) CaptureRotationRetirementLock() error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if (!b.ready.Load() && !(b.readOnly && b.linked && b.ctx.Err() == nil)) || b.store == nil {
		return ErrSource
	}
	return b.store.CaptureRotationRetirementLock()
}

func (b *Baseline) RetireClosedRotationRoot(ctx context.Context, successor *Baseline, previous AuthenticatedMetadataSealReference, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, authority authenticatedstate.SealEligibility, reservation resourcebudget.Permit) error {
	if b == nil || successor == nil || b == successor {
		return ErrSource
	}
	if !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if b.ctx.Err() == nil || !b.borrowedClosed || !b.borrowedClean || b.closeErr != nil || b.position != metadataHeadForRotation(previous, b.position) || previous.Schema != (authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: AuthenticatedMetadataSealIndexID, Codec: AuthenticatedMetadataSealCodec}) {
		return errors.Join(ErrSource, b.closeErr)
	}
	if !successor.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer successor.mu.Unlock()
	if !successor.ready.Load() || successor.budget != b.budget || successor.evidence.Identity != b.evidence.Identity || successor.evidence.KeyGeneration != b.evidence.KeyGeneration || successor.evidence.InstanceID != b.evidence.InstanceID {
		return ErrSource
	}
	return b.store.RetireClosedRotationRoot(ctx, successor.store, previous.Root, publisher, guard, authority, reservation)
}
func metadataHeadForRotation(ref AuthenticatedMetadataSealReference, head metadatajournal.Head) metadatajournal.Head {
	p := ref.Position
	if p.Component != authenticatedstate.SealComponentMetadata || p.Epoch != head.Epoch || p.Revision != head.FileRevision || p.Sequence != head.Sequence || p.Offset != uint64(head.Offset) || p.Hash != head.Hash || p.MetadataBaseline != head.Baseline.Cut {
		return metadatajournal.Head{}
	}
	return head
}
