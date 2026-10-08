package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

// This cold duplicate sink cannot write any native frame. The original
// Receiver must recognize the exact fully durable Ordering frame before it
// can publish its original confirmation tuple through the real publisher.
type receivedReplicaDuplicateSink struct{}

func (receivedReplicaDuplicateSink) Persist(replication.Frame) error {
	return replication.ErrMemberAuthority
}

// recoverReceivedOrdering runs before any mutable selected native factory or
// startup publication. A receipt is qualified against the original Member,
// whole native source and Ordering descriptors, and the old seven roots are
// independently selected before any progress change. Applied remains old.
func (f *haSelectedRuntimeFoundation) recoverReceivedOrdering(ctx context.Context) (member replication.MemberState, resultErr error) {
	if f == nil || f.replication == nil || f.owners.Baseline == nil || f.cfg.ResourceBudget == nil || f.promotionRecovery != nil || f.peerPromotionRecovery != nil {
		return member, authenticatedstate.ErrSealIneligible
	}
	r, c := f.replication, f.owners.Components
	var witness *replication.ReplicaReceivedOrderingAuthority
	var err error
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		witness, err = replication.QualifyReplicaReceivedOrdering(ctx, r.publisher, r.journal, r.source, f.owners.Baseline, c.Budget)
	} else {
		witness, err = replication.QualifyPortableReplicaReceivedOrdering(ctx, r.publisher, r.journal, r.source, f.owners.Baseline, c.Budget)
	}
	if err != nil {
		return member, fmt.Errorf("HA actual received pre-IO receipt/native/Ordering qualification: %w", err)
	}
	defer func() {
		if resultErr != nil && errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &replication.ReplicaReceivedOrderingFailure{Owner: witness, Cause: resultErr}
			return
		}
		if resultErr != nil {
			resultErr = errors.Join(resultErr, witness.Close())
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				resultErr = &replication.ReplicaReceivedOrderingFailure{Owner: witness, Cause: resultErr}
			}
		}
	}()
	if err = statecut.WithHAReceivedSelectedCut(ctx, f.owners, witness, func(ctx context.Context, _ statecut.HASelectedCut) error { return ctx.Err() }); err != nil {
		return member, err
	}
	bankBytes := replication.PhysicalPublicationDiskBytes
	if !f.cfg.ResourceBudget.PhysicalTerminalReserve {
		bankBytes = replication.PortableReceivedBankDiskBytes
	}
	bank, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: bankBytes})
	if err != nil {
		return member, err
	}
	f.fees = append(f.fees, bank)
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		f.physicalPublication, err = r.publisher.PreparePhysicalPublicationForReceived(ctx, r.journal, c.Budget, replication.PhysicalPublicationOptions{Records: resourcebudget.MaxCompletionBatch, OrderingDisk: f.orderingDisk, BankDisk: bank, Restart: true}, witness)
	} else {
		_, err = r.publisher.PreparePortableReceivedPublicationForReceived(ctx, r.journal, r.source, f.owners.Baseline, c.Budget, bank, witness)
	}
	if err != nil {
		return member, fmt.Errorf("HA received original Member/Ordering receipt restoration: %w", err)
	}
	old := r.publisher.Snapshot()
	receiver, err := replication.NewReplicaReceiver(old.ClusterID, old.Incarnation, old.NodeID, f.owners.Baseline.CurrentPrimary(), old.Term, old.PromisedTerm, old.ConfirmedIndex, old.AppliedIndex, r.journal, receivedReplicaDuplicateSink{}, replication.ReplicaReceiverOptions{PersistState: r.publisher.PublishReplica, Projection: old.Projection})
	if err != nil {
		return member, err
	}
	if err = witness.Recover(ctx, receiver); err != nil {
		return member, fmt.Errorf("HA original accepted Receiver confirmation recovery: %w", err)
	}
	return r.publisher.Snapshot(), nil
}
