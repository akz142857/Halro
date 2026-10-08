package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

// The original receiver and applier mint the recovery job before any mutable
// native/root factory exists. No transport or ordinary query can reach this
// receiver until that same applier has durably published the full Applied cut.
func (f *haSelectedRuntimeFoundation) restoreConfirmedSelectedStartup(ctx context.Context, member replication.MemberState) error {
	r := f.replication
	var err error
	r.receiver, err = replication.NewReplicaStartupReceiver(member.ClusterID, member.Incarnation, member.NodeID, f.owners.Baseline.CurrentPrimary(), member.Term, member.PromisedTerm, member.ConfirmedIndex, member.AppliedIndex, r.journal, replication.ReplicaReceiverOptions{PersistState: r.publisher.PublishReplica, Projection: member.Projection})
	if err != nil {
		return err
	}
	r.applier, err = replication.NewReplicaApplier(r.receiver, r.journal, &haSelectedStartupProjection{owner: f, member: member})
	if err != nil {
		return err
	}
	applied, err := r.applier.ApplyConfirmed(ctx)
	if err != nil {
		return fmt.Errorf("HA original confirmed startup applier: %w", err)
	}
	if applied != member.ConfirmedIndex {
		return authenticatedstate.ErrSealIneligible
	}
	return f.validateSelectedStartupRoots(ctx)
}

type haSelectedStartupProjection struct {
	owner  *haSelectedRuntimeFoundation
	member replication.MemberState
}

func (*haSelectedStartupProjection) ApplyLedgerThrough(context.Context, uint64, uint64) error {
	return authenticatedstate.ErrSealIneligible
}
func (*haSelectedStartupProjection) ApplyMetadataThrough(context.Context, uint64, uint64) error {
	return authenticatedstate.ErrSealIneligible
}
func (p *haSelectedStartupProjection) ApplyConfirmedPrefix(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) (replication.ProjectionState, error) {
	f := p.owner
	if f == nil || f.reopened != nil || f.owners.Components.NativeLedger != nil {
		return replication.ProjectionState{}, authenticatedstate.ErrSealIneligible
	}
	ctx = context.WithValue(ctx, haSelectedConfirmedJobKey{}, &haSelectedConfirmedJob{owner: f, prefix: prefix})
	var initial statecut.HASelectedCut
	if err := f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error { initial = cut; return ctx.Err() }); err != nil {
		return replication.ProjectionState{}, err
	}
	if err := f.openSelectedNativeOwners(ctx, p.member, initial); err != nil {
		return replication.ProjectionState{}, err
	}
	if err := f.prepareSelectedRoots(ctx); err != nil {
		return replication.ProjectionState{}, err
	}
	if err := f.validateSelectedStartupRoots(ctx); err != nil {
		return replication.ProjectionState{}, err
	}
	return replication.ProjectionState{Index: initial.AllowedIndex, MetadataEpoch: initial.Allowed[3].Generation, MetadataSequence: initial.Allowed[3].Sequence}, nil
}

// Requalification uses the current independent Member/source cut, including
// genuine startup confirmed permission only while its original job is active.
// The maintenance lease remains exclusive; this grants no business read lane.
func (f *haSelectedRuntimeFoundation) validateSelectedStartupRoots(ctx context.Context) error {
	if f == nil || f.reopened == nil {
		return authenticatedstate.ErrSealIneligible
	}
	c := f.reopened.Owners.Components
	return c.NativeLedger.WithHASelectedSealAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, permitted ledger.HASelectedLedgerPosition) error {
		return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
			if a != cut.Authority || permitted.Generation != cut.Allowed[0].Generation || permitted.Sequence != cut.Allowed[0].Sequence || permitted.Hash != cut.Heads[0] {
				return authenticatedstate.ErrSealIneligible
			}
			lr, err := c.Ledger.SealReference(ctx)
			if err != nil || lr.Anchor.Head.Generation != permitted.Generation || lr.Anchor.Head.Sequence != permitted.Sequence || lr.Anchor.Hash != permitted.Hash {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			ur, err := c.Usage.TypedSealReference(ctx)
			if err != nil || ur.Ledger != lr {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			gr, err := c.Governance.TypedSealReference(ctx)
			if err != nil || gr.Journal.Position.Sequence != cut.Allowed[2].Sequence || gr.Journal.Position.Hash != cut.Heads[2] || gr.Projection.Position != gr.Journal.Position {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			mr, err := c.Metadata.SealReference(ctx)
			if err != nil || mr.Position.Epoch != cut.Allowed[3].Generation || mr.Position.Sequence != cut.Allowed[3].Sequence || mr.Position.Hash != cut.Heads[3] {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			dr, err := c.Directory.SealReference(ctx)
			if err != nil || dr.Position.Generation != permitted.Generation || dr.Position.Sequence != permitted.Sequence || dr.Position.Hash != permitted.Hash {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			return ctx.Err()
		})
	})
}
