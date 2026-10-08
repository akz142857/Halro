package app

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

// This profile funds real session queues, incoming frame reconstructions,
// Primary candidates and the original native reader. Selected COW roots and
// their scoped IO are separately paid by their actual owners; no ordinary DB,
// cold scratch rebuild or backup is created by this transport constructor.
func validateSelectedHASessionBudget(b config.ResourceBudget) error {
	n, q, r, d := b.ReplicationCandidateRAMBytes, b.ReplicationQueueRAMBytes, b.ReplicationReadRAMBytes, b.ReplicationReaderRAMBytes
	if n < replicationlimits.MinCandidateRAMBytes || q < replicationlimits.MinQueueRAMBytes || r < replicationlimits.MinReadRAMBytes || d < replicationlimits.MinNativeReaderRAMBytes {
		return errors.New("selected HA sessions require calibrated candidate, queue, incoming frame and native reader envelopes")
	}
	control := int64(12) * resourcebudget.ClaimRAMBytes
	if d > math.MaxInt64-control-replication.MetadataBaselineReaderRAMBytes {
		return resourcebudget.ErrInvalid
	}
	base := d + replication.MetadataBaselineReaderRAMBytes + control
	for _, v := range []int64{n, q, r} {
		if v > (math.MaxInt64-base)/replicationlimits.MaxBudgetedPeerSessions {
			return resourcebudget.ErrInvalid
		}
		base += v * replicationlimits.MaxBudgetedPeerSessions
	}
	if b.RecoveryClaims < 12 || b.RecoveryRAMBytes < base || b.RecoveryReserveRAMBytes < base {
		return fmt.Errorf("selected HA session recovery requires %d bytes and 12 claims", base)
	}
	return nil
}

// Only this actual selected factory reaches the reduced session profile. A cfg
// flag, copied candidate seal, or absent facade cannot supply the qualification.
// prepareSelectedTransport holds f.mu, before exposing any transport producer.
func (f *haSelectedRuntimeFoundation) selectedReplicationSessionOptions(ctx context.Context) (replication.PeerSessionOptions, error) {
	var options replication.PeerSessionOptions
	if ctx == nil || f == nil || f.ctx == nil || f.ctx.Err() != nil || f.reopened == nil || f.replication == nil || f.replication.manager != nil || f.metadataStore == nil || f.cfg.ResourceBudget == nil || f.owners.Components.Budget == nil {
		return options, authenticatedstate.ErrSealIneligible
	}
	c := f.reopened.Owners.Components
	if c.NativeLedger == nil || c.Ledger == nil || c.Usage == nil || c.Directory == nil || c.Metadata == nil || c.Governance == nil || c.Audit == nil || c.NativeGovernance == nil || c.Publisher == nil || c.Guard == nil || f.owners.Member == nil || f.owners.Ordering == nil || f.owners.Source == nil || f.owners.Baseline == nil {
		return options, authenticatedstate.ErrSealIneligible
	}
	if c.Budget != f.owners.Components.Budget || c.NativeLedger != f.owners.Components.NativeLedger || c.Publisher != f.owners.Components.Publisher || c.Guard != f.owners.Components.Guard || f.reopened.Owners.Member != f.owners.Member || f.reopened.Owners.Ordering != f.owners.Ordering || f.reopened.Owners.Source != f.owners.Source || f.reopened.Owners.Baseline != f.owners.Baseline {
		return options, authenticatedstate.ErrSealIneligible
	}
	if err := f.reopened.ValidateHAMetadataStoreOwner(f.metadataStore, c.Budget); err != nil {
		return options, err
	}
	if err := f.qualifySelectedSessionRoots(ctx); err != nil {
		return options, fmt.Errorf("selected session all-root qualification: %w", err)
	}
	// Exercise the original issued facade's authenticated read lane as well as
	// its identity. A previously closed DAO cannot qualify a new session owner.
	if _, err := f.metadataStore.AdminBootstrapCompletion(ctx); err != nil {
		return options, fmt.Errorf("selected session issued metadata facade: %w", err)
	}
	if err := validateSelectedHASessionBudget(*f.cfg.ResourceBudget); err != nil {
		return options, err
	}
	if err := errors.Join(ctx.Err(), f.ctx.Err()); err != nil {
		return options, err
	}
	queue, err := replication.NewPeerSessionBudget(c.Budget, f.cfg.ResourceBudget.ReplicationQueueRAMBytes, f.cfg.ResourceBudget.ReplicationReadRAMBytes, replicationlimits.MaxBudgetedPeerSessions)
	if err != nil {
		return options, err
	}
	options.QueueBudget = queue
	return options, nil
}

// These actual owners are still private: transport producers are installed
// only after this function returns. Read each authenticated current root in its
// real read lane, then independently freeze Member/Ordering/four sources and
// compare the complete positions. Live owners cannot borrow a closed startup
// lease, and their read scopes must join before the whole-source freeze begins.
func (f *haSelectedRuntimeFoundation) qualifySelectedSessionRoots(ctx context.Context) error {
	c := f.reopened.Owners.Components
	if _, err := c.Lease.ValidateOwner(c.NativeLedger); err == nil {
		return f.validateSelectedStartupRoots(ctx)
	}
	lr, err := c.Ledger.SealReference(ctx)
	if err != nil {
		return fmt.Errorf("Ledger root: %w", err)
	}
	ur, err := c.Usage.TypedSealReference(ctx)
	if err != nil {
		return fmt.Errorf("Usage root: %w", err)
	}
	gr, err := c.Governance.TypedSealReference(ctx)
	if err != nil {
		return fmt.Errorf("Governance root: %w", err)
	}
	mr, err := c.Metadata.SealReference(ctx)
	if err != nil {
		return fmt.Errorf("Metadata root: %w", err)
	}
	dr, err := c.Directory.SealReference(ctx)
	if err != nil {
		return fmt.Errorf("Directory root: %w", err)
	}
	return f.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
		if lr.Anchor.Head.Generation != cut.Allowed[0].Generation || lr.Anchor.Head.Sequence != cut.Allowed[0].Sequence || lr.Anchor.Hash != cut.Heads[0] || ur.Ledger != lr || gr.Journal.Position.Sequence != cut.Allowed[2].Sequence || gr.Journal.Position.Hash != cut.Heads[2] || gr.Projection.Position != gr.Journal.Position || mr.Position.Epoch != cut.Allowed[3].Generation || mr.Position.Sequence != cut.Allowed[3].Sequence || mr.Position.Hash != cut.Heads[3] || dr.Position.Generation != cut.Allowed[0].Generation || dr.Position.Sequence != cut.Allowed[0].Sequence || dr.Position.Hash != cut.Heads[0] {
			return authenticatedstate.ErrSealIneligible
		}
		return ctx.Err()
	})
}
