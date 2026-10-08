package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

type haSelectedPromotionAudit struct {
	auditCapture      *audit.StartupCache
	governanceCapture *governance.StartupCache
	caches            statecut.StartupCacheCandidates
}

func (o *haSelectedPromotionAudit) Close() error {
	if o == nil {
		return nil
	}
	return errors.Join(o.caches.Audit.Close(), o.caches.Governance.Close(), o.auditCapture.Close(), o.governanceCapture.Close())
}

func haSelectedPromotionAuditEvents(authority *replication.HAPromotionAuthority) ([]audit.Event, error) {
	c := authority.Claims()
	if !c.Fulfilled || c.OccurredAtMicros <= 0 {
		return nil, replication.ErrMemberAuthority
	}
	encoded, err := json.Marshal(c.Promises)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	carrierDigest := authority.Digest()
	nodes := make([]string, 0, len(c.Promises))
	for _, p := range c.Promises {
		nodes = append(nodes, p.NodeID)
	}
	metadata := map[string]any{"cluster_id": c.Before.ClusterID, "incarnation": c.Before.Incarnation, "node_id": c.Before.NodeID, "term": c.Proposal.Term, "expect_term": c.Proposal.ExpectedTerm, "expect_index": c.Proposal.ExpectedAppliedIndex, "old_primary": c.Proposal.OldPrimaryNodeID, "fenced_by": c.Proposal.FencedBy, "promise_nodes": nodes, "promise_digest": "sha256:" + hex.EncodeToString(digest[:]), "self": c.Proposal.Self, "no_peer_promise": c.NoPeerPromise, "promotion_authority": "sha256:" + hex.EncodeToString(carrierDigest[:])}
	base := audit.Event{EventID: "evt_promote_" + hex.EncodeToString(carrierDigest[:]), OccurredAt: time.UnixMicro(c.OccurredAtMicros).UTC(), ActorType: "admin", ActorID: c.Proposal.ActorID, Action: "cluster.promote", TargetType: "cluster_member", TargetID: c.Before.NodeID, Outcome: "success", Metadata: metadata}
	if !c.Proposal.PlannedStepdown {
		return []audit.Event{base}, base.Validate()
	}
	metadata["from_node"], metadata["to_node"], metadata["leadership_index"] = c.Proposal.OldPrimaryNodeID, c.Before.NodeID, c.Before.AppliedIndex+1
	requested := base
	requested.EventID += "_requested"
	requested.Action = "cluster.stepdown.requested"
	base.Action = "cluster.stepdown.completed"
	return []audit.Event{requested, base}, errors.Join(requested.Validate(), base.Validate())
}

// The real stopped command owns these sources. Audit has an actual admission
// before native fsync and original Ordering hooks; it is never a served writer.
func (f *haSelectedRuntimeFoundation) appendSelectedPromotionAudit(ctx context.Context, authority *replication.HAPromotionAuthority, step func(string) error) (resultErr error) {
	work, err := f.owners.Components.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: audit.PhysicalBatchRAMBytes})
	if err != nil {
		return err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			f.fees = append(f.fees, work)
			return
		}
		if err := work.Release(); err != nil {
			f.fees = append(f.fees, work)
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
		}
	}()
	events, err := haSelectedPromotionAuditEvents(authority)
	if err != nil {
		return err
	}
	c := authority.Claims()
	state := f.replication.publisher.Snapshot()
	if state.Role != replication.RolePrimary || state.Term != c.Proposal.Term || state.DurableIndex < c.Before.AppliedIndex+1 || state.DurableIndex > c.Before.AppliedIndex+2 || state.ConfirmedIndex != c.Before.AppliedIndex || state.AppliedIndex != c.Before.AppliedIndex {
		return replication.ErrMemberAuthority
	}
	peers := make([]string, 0, len(state.Peers))
	for _, p := range state.Peers {
		peers = append(peers, p.Name)
	}
	outbound, err := replication.NewConnectionOutbound(peers, offlinePeerSender{})
	if err != nil {
		return err
	}
	limits := f.cfg.ResourceBudget
	window := replication.DefaultPrimaryWindowLimits()
	window.CandidateBudget, window.CandidateRAMBytes = f.owners.Components.Budget, limits.ReplicationCandidateRAMBytes
	if limits.TotalUnconfirmedBytes > 0 {
		window.UnconfirmedBudget, window.MaxUnconfirmedEntries, window.UnconfirmedMetadataRAMBytes, window.RecoveryReserveUnconfirmedBytes = f.owners.Components.Budget, limits.UnconfirmedMaxEntries, replicationlimits.PrimaryUnconfirmedTableRAMBytes, limits.RecoveryReserveUnconfirmedBytes
	}
	coordinator, err := replication.NewPrimaryCoordinatorFromSource(state.ClusterID, state.Incarnation, state.NodeID, state.Term, state.ConfirmedIndex, peers, f.replication.journal, outbound, f.replication.publisher.PublishPrimary, f.replication.source, window)
	if err != nil {
		return err
	}
	if prior := f.replication.coordinator; prior != nil {
		prior.Stop()
		f.promotionPriorCoordinator = prior
	}
	f.replication.coordinator = coordinator
	old := f.owners.Components.Audit
	if err := old.Close(); err != nil {
		f.closeErr = errors.Join(f.closeErr, err)
		return err
	}
	log, err := openConfiguredAuditLog(f.cfg, f.bootstrap.AuditPath, f.bootstrap.AuditKey, audit.Options{RequireExisting: true, BeforeDurable: func(batch audit.DurableBatch) (audit.AppendAdmission, error) {
		return coordinator.ReserveNativeSourceUnconfirmed(len(batch.Frames))
	}, AfterDurable: func(batch audit.DurableBatch) error {
		if err := step("audit-native"); err != nil {
			return err
		}
		_, err := coordinator.RecordAuditBatch(batch)
		return err
	}}, f.owners.Components.Budget)
	if log != nil {
		f.owners.Components.Audit, f.reopened.Owners.Components.Audit = log, log
	}
	if err != nil {
		return err
	}
	// Fully authenticate every native record. An orphan suffix is repairable
	// only if it is this signed transition's exact deterministic event batch.
	first := c.Cursors[1].Sequence + 1
	count := 0
	head, err := log.ReplayContext(ctx, func(record audit.Record) error {
		if record.Sequence < first {
			return nil
		}
		if count >= len(events) || record.Sequence != first+uint64(count) {
			return replication.ErrMemberAuthority
		}
		actual, aerr := json.Marshal(record.Event)
		expected, eerr := json.Marshal(events[count])
		if aerr != nil || eerr != nil || !bytes.Equal(actual, expected) {
			return errors.Join(replication.ErrMemberAuthority, aerr, eerr)
		}
		count++
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	if count != 0 && count != len(events) {
		return replication.ErrMemberAuthority
	}
	if count == 0 {
		if state.DurableIndex != c.Before.AppliedIndex+1 {
			return replication.ErrMemberAuthority
		}
		if _, err := log.AppendBatch(ctx, events); err != nil {
			return err
		}
	} else if state.DurableIndex == c.Before.AppliedIndex+1 {
		frames, err := f.replication.source.Read(replication.StoreAudit, 1, first, head.Records)
		if err != nil {
			return err
		}
		admission, err := coordinator.ReserveNativeSourceUnconfirmed(len(frames))
		if err != nil {
			return err
		}
		if _, err = coordinator.RecordAuditBatch(audit.DurableBatch{FirstSequence: first, LastSequence: head.Records, Frames: frames, Admission: admission}); err != nil {
			return err
		}
	}
	record, err := f.replication.journal.Record(c.Before.AppliedIndex + 2)
	if err != nil || record.Kind != replication.KindData || record.Store != replication.StoreAudit || record.Term != c.Proposal.Term || record.StoreGeneration != 1 || record.StoreSequenceFirst != first || record.StoreSequenceLast != first+uint64(len(events))-1 {
		return errors.Join(replication.ErrMemberAuthority, err)
	}
	return step("audit-ordered")
}

func (f *haSelectedRuntimeFoundation) preparePromotionStartupCaches(ctx context.Context, step func(string) error) (statecut.StartupCacheCandidates, error) {
	if f.promotionAudit == nil {
		f.promotionAudit = &haSelectedPromotionAudit{}
	}
	o := f.promotionAudit
	slot := -1
	for i := range haSelectedCacheSlots {
		_, ae := os.Lstat(haSelectedCachePath(f.cfg.Storage.DataDir, i, false))
		_, ge := os.Lstat(haSelectedCachePath(f.cfg.Storage.DataDir, i, true))
		if errors.Is(ae, os.ErrNotExist) && errors.Is(ge, os.ErrNotExist) {
			slot = i
			break
		}
		if ae != nil && !errors.Is(ae, os.ErrNotExist) {
			return o.caches, ae
		}
		if ge != nil && !errors.Is(ge, os.ErrNotExist) {
			return o.caches, ge
		}
	}
	if slot < 0 {
		return o.caches, fmt.Errorf("promotion immutable capsule slots exhausted: %w", authenticatedstate.ErrLimit)
	}
	c := f.reopened.Owners.Components
	var err error
	o.auditCapture, err = c.Audit.CaptureHAStartupCache(ctx, c.Budget, f.bootstrap.Evidence.Identity, c.Audit.Summary())
	if err != nil {
		return o.caches, err
	}
	o.governanceCapture, err = c.NativeGovernance.CaptureHAStartupCache(ctx, c.Budget, f.bootstrap.Evidence.Identity, c.NativeGovernance.Summary())
	if err != nil {
		return o.caches, err
	}
	o.caches.Audit, err = o.auditCapture.PersistNew(ctx, haSelectedCachePath(f.cfg.Storage.DataDir, slot, false), audit.StartupCachePersistOptions{Guard: c.Guard, Budget: c.Budget, DiskReservation: f.cacheDisks[slot][0], Identity: f.bootstrap.Evidence.Identity, Head: o.auditCapture.Head()})
	if err != nil {
		return o.caches, err
	}
	if err = step("audit-capsule"); err != nil {
		return o.caches, err
	}
	o.caches.Governance, err = o.governanceCapture.PersistNew(ctx, haSelectedCachePath(f.cfg.Storage.DataDir, slot, true), governance.StartupCachePersistOptions{Guard: c.Guard, Budget: c.Budget, DiskReservation: f.cacheDisks[slot][1], Identity: f.bootstrap.Evidence.Identity, Head: o.governanceCapture.Head()})
	if err == nil {
		err = step("governance-capsule")
	}
	return o.caches, err
}
