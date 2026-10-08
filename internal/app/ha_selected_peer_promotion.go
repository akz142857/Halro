package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
)

// The selected control caller persists this peer's own root evidence before
// promising. The candidate's proposal never supplies the peer's old roots.
func (f *haSelectedRuntimeFoundation) selectedHandleRecords(data replication.ConnectionRecordHandler) replication.ConnectionRecordHandler {
	ordinary := f.replication.handleRecords(data)
	return func(ctx context.Context, peer replication.Hello, record replication.StreamRecord, send func([]byte) error) error {
		if record.Kind == replication.StreamRecordPhysicalCredit {
			q := f.replication.peerPhysical.Load()
			if q == nil {
				return replication.ErrPhysicalCredit
			}
			return q.handle(ctx, peer, record, send)
		}
		if record.Kind == replication.StreamRecordPromotionProposal {
			return f.handleSelectedPromotionProposal(ctx, peer, record.Encoded, send)
		}
		if record.Kind == replication.StreamRecordHAPeerPromotionRequest {
			return f.handleSelectedPeerBoundaryRequest(ctx, peer, record.Encoded, send)
		}
		return ordinary(ctx, peer, record, send)
	}
}

func (f *haSelectedRuntimeFoundation) handleSelectedPromotionProposal(ctx context.Context, peer replication.Hello, encoded []byte, send func([]byte) error) error {
	p, err := replication.UnmarshalPromotionProposal(encoded)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.replication
	if r == nil || f.reopened == nil || f.ctx.Err() != nil || send == nil || f.cfg.ResourceBudget.PhysicalTerminalReserve || f.promotionRecovery != nil {
		return authenticatedstate.ErrSealIneligible
	}
	c := f.reopened.Owners.Components
	before := r.publisher.Snapshot()
	if peer.Role != replication.RoleAwaitingDecision || peer.NodeID != p.CandidateNodeID || p.ClusterID != before.ClusterID || p.Incarnation != before.Incarnation || validatePromotionCandidateHello(peer, p) != nil || peer.MetadataBaselineAuthority != f.owners.Baseline.Descriptor().AuthorityDigest {
		return replication.ErrMemberAuthority
	}
	key := r.clusterKey[:]
	prepared, err := readHASelectedPeerPromotionCarrier(ctx, haSelectedPeerPromotionPath(f.cfg, false), key, c.Budget)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if prepared != nil && (prepared.Claims().Proposal != p || prepared.ValidateRecoveryMember(before) != nil) {
		return replication.ErrMemberAuthority
	}
	primary := r.coordinator != nil
	if primary {
		if prepared == nil && p.PlannedStepdown {
			if err := r.freezeForPlannedStepdown(p, before); err != nil {
				return err
			}
		} else {
			r.coordinator.MarkUnavailable(errors.New("selected promotion withdraws old Primary admission"))
		}
		r.startupReady.Store(false)
	}
	// Join the actual business owners before the exclusive native source loan.
	// A rejected/partial preparation deliberately keeps admission withdrawn.
	if err := f.business.close(ctx); err != nil {
		return err
	}
	f.business = nil
	if prepared == nil {
		var lease *ledger.MaintenanceLease
		if primary {
			lease, err = c.NativeLedger.BeginMaintenance(ctx)
		} else {
			lease, err = c.NativeLedger.BeginReplicaReadMaintenance(ctx)
		}
		if err != nil {
			return err
		}
		// Retain the actual newly installed lease without mutating the ordinary
		// source inventory that existing read callbacks have captured.
		f.peerMaintenance = lease
		if err := f.reopened.Owners.Components.Usage.BindHASelectedTypedMaintenance(ctx, c.NativeLedger, lease, c.Ledger, c.Budget); err != nil {
			return fmt.Errorf("selected peer Usage maintenance handoff: %w", err)
		}
		captureOwners := f.reopened.Owners
		captureOwners.Components.Lease = lease
		claims, err := statecut.CaptureHAPromotionPeer(ctx, captureOwners, p)
		if err != nil {
			return fmt.Errorf("selected peer complete root capture: %w", err)
		}
		prepared, err = persistHASelectedPeerPromotionCarrier(ctx, f.cfg, claims, key, c.Budget)
		if err != nil {
			return err
		}
	}
	claims := prepared.Claims()
	if before.PromisedTerm == before.Term {
		if primary {
			before, err = r.publisher.Promise(p.Term)
		} else {
			if r.receiver == nil {
				return replication.ErrMemberAuthority
			}
			err = r.receiver.Promise(p.Term, func() error {
				before, err = r.publisher.Promise(p.Term)
				return err
			})
		}
		if err != nil {
			return err
		}
	}
	// Promise is the irreversible boundary, even if the subsequent carrier or
	// response fails. Ordinary source/read entry points now reject this owner.
	if primary {
		defer r.fail(errors.New("selected Primary durably promised a higher term and stepped down"))
	}
	if prepared.ValidateRecoveryMember(before) != nil || before.PromisedTerm != p.Term {
		return replication.ErrMemberAuthority
	}
	_, frameTerm, _ := r.journal.Head()
	promise := replication.PromotionPromise{Version: replication.PromotionProtocolVersion, ClusterID: before.ClusterID, Incarnation: before.Incarnation, NodeID: before.NodeID, Role: before.Role, PreviousRole: claims.Before.Role, Term: before.Term, PromisedTerm: before.PromisedTerm, DurableIndex: before.DurableIndex, AppliedIndex: before.AppliedIndex, LastFrameTerm: frameTerm, PeerPreparationDigest: prepared.Digest()}
	claims.Promise = &promise
	fulfilled, err := persistHASelectedPeerPromotionCarrier(ctx, f.cfg, claims, key, c.Budget)
	if err != nil {
		return fmt.Errorf("selected peer durable promise carrier: %w", err)
	}
	response, err := fulfilled.Claims().Promise.MarshalBinary()
	if err != nil {
		return err
	}
	return send(response)
}
