package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
)

type haSelectedConfirmedJobKey struct{}
type haSelectedConfirmedJob struct {
	owner  *haSelectedRuntimeFoundation
	prefix *replication.ReplicaConfirmedPrefix
}

// prepareSelectedServices authenticates historical sources and the actual seven
// selected derived owners. This is an owned startup phase, not an HA Ready grant.
func (f *haSelectedRuntimeFoundation) prepareSelectedRoots(ctx context.Context) error {
	c := f.owners.Components
	if f.reopened != nil {
		return f.validateSelectedStartupRoots(ctx)
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	opts := audit.StartupAuditOptions{Deadline: 10 * time.Minute, ProgressTimeout: 30 * time.Second}
	var err error
	f.audits[0], err = c.NativeLedger.StartSelectedStartupAudit(opCtx, c.Budget, opts)
	if err != nil {
		return err
	}
	f.audits[1], err = c.Audit.StartSelectedStartupAudit(opCtx, c.Budget, opts)
	if err != nil {
		return err
	}
	f.audits[2], err = c.NativeGovernance.StartSelectedStartupAudit(opCtx, c.Budget, opts)
	if err != nil {
		return err
	}
	for _, auditor := range f.audits {
		if err := auditor.Wait(opCtx); err != nil {
			return err
		}
	}
	if err := f.prepareHAPhysicalNativeSources(opCtx); err != nil {
		return fmt.Errorf("HA services physical native sources: %w", err)
	}
	r, err := f.reopenSelectedGeneration(opCtx)
	if err != nil {
		return fmt.Errorf("HA services selected derived reopen: %w", err)
	}
	if f.peerPromotionRecovery != nil {
		return nil
	}
	if err := r.ResumeSelectedMetadata(opCtx); err != nil {
		return fmt.Errorf("HA services Metadata handoff: %w", err)
	}
	if err := r.AdvanceSelectedMetadataForStartup(opCtx); err != nil {
		return fmt.Errorf("HA services Metadata allowed prefix: %w", err)
	}
	return nil
}

func (f *haSelectedRuntimeFoundation) prepareSelectedServices(ctx context.Context) error {
	return f.prepareSelectedServicesWithLease(ctx, false)
}

// Primary startup retains native exclusivity until genuine pending recovery,
// final whole-ordering confirmation and service reconstruction have finished.
// Replica must release its read-only startup lease before the actual applier.
func (f *haSelectedRuntimeFoundation) prepareSelectedServicesWithLease(ctx context.Context, retainPrimaryLease bool) error {
	if f.promotionRecovery != nil || f.peerPromotionRecovery != nil {
		return authenticatedstate.ErrSealIneligible
	}
	if err := f.prepareSelectedRoots(ctx); err != nil {
		return err
	}
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		// No DAO, service or transport producer exists while the complete
		// physical vector acquires its original role-specific targets.
		if err := f.prepareHAPhysicalDerivedVector(ctx); err != nil {
			return fmt.Errorf("HA services complete physical vector: %w", err)
		}
		if f.replication.role == replication.RoleReplica {
			c := f.reopened.Owners.Components
			if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
				return c.Ledger.PrepareHAReplicaPhysicalCatchup(ctx, c.NativeLedger, c.Lease)
			}); err != nil {
				return fmt.Errorf("HA physical Replica receive window: %w", err)
			}
		}
	}
	c, r := f.owners.Components, f.reopened
	var err error
	withPreparation := func(visit func(context.Context) error) error {
		if f.cfg.ResourceBudget.PhysicalTerminalReserve {
			return r.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, visit)
		}
		return visit(ctx)
	}
	err = withPreparation(func(ctx context.Context) error {
		f.metadataStore, err = r.OpenHAMetadataStore(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("HA services Metadata DAO: %w", err)
	}
	if f.replication.role == replication.RolePrimary {
		if err := withPreparation(f.installHASelectedAccountingService); err != nil {
			return fmt.Errorf("HA services accounting factory: %w", err)
		}
	}
	if err := withPreparation(func(ctx context.Context) error {
		return r.Owners.Components.Usage.ActivateHASelectedTypedLive(ctx, c.NativeLedger, c.Budget)
	}); err != nil {
		return fmt.Errorf("HA services Usage live: %w", err)
	}
	if err := f.installHASelectedUsageCollector(ctx); err != nil {
		return fmt.Errorf("HA services Usage Collector: %w", err)
	}
	if err := withPreparation(func(ctx context.Context) error {
		return r.Owners.Components.Ledger.EnableHASelectedBusinessReads(ctx, c.NativeLedger, c.Lease, c.Budget)
	}); err != nil {
		return fmt.Errorf("HA services Ledger business reads: %w", err)
	}
	if retainPrimaryLease {
		if f.replication.role != replication.RolePrimary {
			return authenticatedstate.ErrSealIneligible
		}
		return ctx.Err()
	}
	return c.Lease.Close()
}

// prepareSelectedTransport installs the real TLS source/sink/applier callers.
// It never opens metadata.db or rebuilds ordinary compatibility projections.
func (f *haSelectedRuntimeFoundation) prepareSelectedTransport(logger *slog.Logger) error {
	return f.prepareSelectedTransportMode(logger, false)
}

// Strict wire3 requires original paid physical owners and a genuine producer.
// The ordinary component transport keeps wire2 compatibility.
func (f *haSelectedRuntimeFoundation) prepareSelectedTransportWithPhysicalCredit(logger *slog.Logger) error {
	return f.prepareSelectedTransportMode(logger, true)
}
func (f *haSelectedRuntimeFoundation) prepareSelectedTransportMode(logger *slog.Logger, strict bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, c := f.replication, f.owners.Components
	if f.reopened == nil || f.reopened.Derivative == nil || r == nil || r.manager != nil || logger == nil || f.ctx.Err() != nil {
		return authenticatedstate.ErrSealIneligible
	}
	if strict {
		if err := f.replenishHAPhysicalStartupOrdering(f.ctx); err != nil {
			return fmt.Errorf("HA physical startup Ordering renewal: %w", err)
		}
	}
	state := r.publisher.Snapshot()
	var session replication.PeerSessionOptions
	var err error
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		err = f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(f.ctx, func(ctx context.Context) error {
			session, err = f.selectedReplicationSessionOptions(ctx)
			return err
		})
	} else {
		session, err = f.selectedReplicationSessionOptions(f.ctx)
	}
	if err != nil {
		return err
	}
	endpoints := make([]replication.PeerEndpoint, 0, len(state.Peers))
	for _, peer := range state.Peers {
		endpoints = append(endpoints, replication.PeerEndpoint{NodeID: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	var handle replication.ConnectionRecordHandler
	if state.Role == replication.RolePrimary {
		handler, err := replication.NewPrimaryRecordHandler(r.coordinator)
		if err != nil {
			return err
		}
		handle = handler.Handle
	} else if state.Role == replication.RoleReplica {
		if err := f.preparePortableReceivedPublication(f.ctx); err != nil {
			return err
		}
		sink, err := replication.NewNativeSink(c.NativeLedger, c.Audit, c.NativeGovernance, f.metadataJournal, filepath.Join(f.cfg.Storage.DataDir, "provider-objects"), providerObjectSourceDir(f.cfg))
		if err != nil {
			return err
		}
		if err := sink.ConfigureMetadataBaseline(f.owners.Baseline, r.source, f.metadataJournal); err != nil {
			return err
		}
		if r.receiver != nil {
			if err = r.receiver.InstallStartupNativeSink(sink); err != nil {
				return err
			}
		} else {
			r.receiver, err = replication.NewReplicaReceiver(state.ClusterID, state.Incarnation, state.NodeID, f.owners.Baseline.CurrentPrimary(), state.Term, state.PromisedTerm, state.ConfirmedIndex, state.AppliedIndex, r.journal, sink, replication.ReplicaReceiverOptions{PersistState: r.publisher.PublishReplica, Projection: state.Projection})
			if err != nil {
				return err
			}
		}
		r.applier, err = replication.NewReplicaApplier(r.receiver, r.journal, &haSelectedReplicaProjection{owner: f})
		if err != nil {
			return err
		}
		handler, err := replication.NewReplicaRecordHandler(r.receiver, r.applier)
		if err != nil {
			return err
		}
		handle = handler.Handle
	} else {
		return replication.ErrMemberAuthority
	}
	if strict {
		if !f.cfg.ResourceBudget.PhysicalTerminalReserve || r.peerPhysical.Load() != nil {
			return replication.ErrPhysicalCredit
		}
		var q *haPeerPhysicalController
		if state.Role == replication.RolePrimary {
			q, err = newHAPrimaryPhysicalController(f.ctx, f)
		} else {
			q, err = newHAReplicaPhysicalController(f.ctx, f)
		}
		if q != nil {
			r.peerPhysical.Store(q)
		}
		if err != nil {
			return err
		}
	}
	opts := replication.ConnectionManagerOptions{LocalNode: state.NodeID, TLS: replication.TLSFiles{CAFile: f.cfg.Replication.TLS.CAFile, CertFile: f.cfg.Replication.TLS.CertFile, KeyFile: f.cfg.Replication.TLS.KeyFile}, ClusterKey: r.clusterKey[:], Peers: endpoints, LocalHello: f.selectedHello, OnAuthenticatedHello: f.authorizeSelectedHello, Handle: f.selectedHandleRecords(handle), Session: session, OnSessionError: func(peer string, err error) {
		logger.Warn("HA selected replication session failed", "peer", peer, "error", err)
	}}
	if r.coordinator != nil {
		opts.OnDataSessionReady = func(peer string, hello replication.Hello, send func([]byte) error) error {
			if r.startupCtx != nil {
				return r.catchUpPrimaryPeer(peer, hello, send)
			}
			return r.coordinator.PreparePeer(peer, hello.DurableIndex, hello.AppliedIndex, send)
		}
		opts.OnDataSessionClosed = r.coordinator.ForgetPeer
	} else if r.receiver != nil {
		opts.OnDataSessionReady = func(node string, peer replication.Hello, send func([]byte) error) error {
			if err := f.sendSelectedReplicaReadyAck(node, peer, send); err != nil {
				return err
			}
			if q := r.peerPhysical.Load(); q != nil {
				return q.sendReplicaWindow(f.ctx, send)
			}
			return nil
		}
	}
	r.manager, err = replication.NewConnectionManager(opts)
	if err == nil {
		f.sender.Set(r.manager)
	}
	return err
}

func (f *haSelectedRuntimeFoundation) selectedHello() (replication.Hello, error) {
	hello, err := f.replication.localHello()
	if err != nil {
		return hello, err
	}
	if hello.Metadata.Current != 2 || f.ctx.Err() != nil {
		return replication.Hello{}, authenticatedstate.ErrSealIneligible
	}
	hello.Protocol = replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}
	if f.replication.peerPhysical.Load() != nil {
		hello.Protocol = replication.VersionRange{Current: 3, Minimum: 3, Maximum: 3}
	}
	hello.Metadata = replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}
	hello.MetadataBaselineAuthority = f.owners.Baseline.Descriptor().AuthorityDigest
	return hello, nil
}
func (f *haSelectedRuntimeFoundation) authorizeSelectedHello(ctx context.Context, peer replication.Hello) (bool, error) {
	r := f.replication
	state := r.publisher.Snapshot()
	claims := f.owners.Baseline.Claims()
	if peer.Role == replication.RoleAwaitingDecision {
		if r.coordinator != nil && peer.Term <= state.Term && peer.PromisedTerm == state.Term {
			return f.authorizePeerBoundaryControl(ctx, peer)
		}
		if peer.ClusterID != state.ClusterID || peer.Incarnation != state.Incarnation || peer.Term != state.Term || peer.PromisedTerm < state.PromisedTerm || peer.MetadataBaselineAuthority != f.owners.Baseline.Descriptor().AuthorityDigest || peer.Protocol != (replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}) || peer.Metadata != peer.Protocol || peer.AppliedIndex < claims.Index+1 {
			return false, replication.ErrMemberAuthority
		}
		if peer.PromisedTerm == state.PromisedTerm {
			prepared, err := readHASelectedPeerPromotionCarrier(ctx, haSelectedPeerPromotionPath(f.cfg, false), r.clusterKey[:], f.owners.Components.Budget)
			if err != nil || prepared.ValidateRecoveryMember(state) != nil || prepared.Claims().Proposal.CandidateNodeID != peer.NodeID || prepared.Claims().Proposal.Term != peer.PromisedTerm || prepared.Claims().Before.AppliedIndex != peer.AppliedIndex || peer.DurableIndex != peer.AppliedIndex {
				return false, errors.Join(replication.ErrMemberAuthority, err)
			}
		}
		// Only a control session is admitted; the actual bounded proposal and real
		// publisher Promise still decide whether any durable transition is valid.
		return false, nil
	}
	if r.coordinator != nil && (peer.Term > state.Term || peer.PromisedTerm > state.Term) {
		_, err := r.authorizePrimaryHello(ctx, peer)
		f.cancel()
		return false, err
	}
	protocol := replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}
	if r.peerPhysical.Load() != nil {
		protocol = replication.VersionRange{Current: 3, Minimum: 3, Maximum: 3}
	}
	if peer.ClusterID != state.ClusterID || peer.Incarnation != state.Incarnation || peer.Term != f.owners.Baseline.CurrentTerm() || peer.PromisedTerm != f.owners.Baseline.CurrentTerm() || peer.MetadataBaselineAuthority != f.owners.Baseline.Descriptor().AuthorityDigest || peer.Protocol != protocol || peer.Metadata != (replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}) || peer.DurableIndex < claims.Index+1 || peer.AppliedIndex < claims.Index+1 || f.ctx.Err() != nil {
		return false, replication.ErrMemberAuthority
	}
	if state.Role == replication.RolePrimary && peer.Role != replication.RoleReplica || state.Role == replication.RoleReplica && (peer.Role != replication.RolePrimary || peer.NodeID != f.owners.Baseline.CurrentPrimary()) {
		return false, replication.ErrMemberAuthority
	}
	if r.coordinator != nil && r.startupCtx != nil {
		return r.authorizePrimaryHello(ctx, peer)
	}
	return true, nil
}

func (f *haSelectedRuntimeFoundation) serveSelectedTransport(ctx context.Context) error {
	if f.promotionRecovery != nil || f.peerPromotionRecovery != nil {
		return authenticatedstate.ErrSealIneligible
	}
	if f.replication.manager == nil {
		return authenticatedstate.ErrSealIneligible
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer func() { stop(); cancel() }()
	listener, err := net.Listen("tcp", f.cfg.Replication.Listen)
	if err != nil {
		return err
	}
	return f.replication.serve(opCtx, listener)
}

type haSelectedReplicaProjection struct{ owner *haSelectedRuntimeFoundation }

func (*haSelectedReplicaProjection) ApplyLedgerThrough(context.Context, uint64, uint64) error {
	return authenticatedstate.ErrSealIneligible
}
func (*haSelectedReplicaProjection) ApplyMetadataThrough(context.Context, uint64, uint64) error {
	return authenticatedstate.ErrSealIneligible
}

func (p *haSelectedReplicaProjection) ApplyConfirmedPrefix(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) (result replication.ProjectionState, resultErr error) {
	f := p.owner
	if f == nil || f.reopened == nil || f.ctx.Err() != nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		resultErr = f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			var err error
			result, err = p.applyConfirmedPrefixOwned(ctx, prefix)
			return err
		})
		return
	}
	return p.applyConfirmedPrefixOwned(ctx, prefix)
}

func (p *haSelectedReplicaProjection) applyConfirmedPrefixOwned(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) (result replication.ProjectionState, resultErr error) {
	f := p.owner
	if f == nil || f.reopened == nil || f.ctx.Err() != nil {
		return replication.ProjectionState{}, authenticatedstate.ErrSealIneligible
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ledger.ErrSegmentRetirement) {
			f.mu.Lock()
			f.applyErr = errors.Join(f.applyErr, resultErr)
			f.closeErr = errors.Join(f.closeErr, resultErr)
			f.mu.Unlock()
			f.cancel()
		}
	}()
	var cut statecut.HAConfirmedSources
	if err := statecut.WithHAConfirmedSourceAuthority(ctx, f.owners, f.replication.receiver, prefix, func(_ context.Context, actual statecut.HAConfirmedSources) error { cut = actual; return nil }); err != nil {
		return replication.ProjectionState{}, err
	}
	ctx = context.WithValue(ctx, haSelectedConfirmedJobKey{}, &haSelectedConfirmedJob{owner: f, prefix: prefix})
	c := f.reopened.Owners.Components
	work, err := c.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: ledger.AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return replication.ProjectionState{}, err
	}
	permitted, _, resolveErr := ledger.InspectProjectionCursor(ctx, f.bootstrap.LedgerPath, f.bootstrap.LedgerKey, cut.Allowed[0].Generation, cut.Allowed[0].Sequence)
	releaseErr := work.Release()
	if releaseErr != nil {
		f.mu.Lock()
		f.fees = append(f.fees, work)
		f.mu.Unlock()
		releaseErr = errors.Join(ownedfile.ErrRetirement, releaseErr)
	}
	if err = errors.Join(resolveErr, releaseErr); err != nil {
		return replication.ProjectionState{}, err
	}
	if permitted.Hash != cut.Heads[0] {
		return replication.ProjectionState{}, authenticatedstate.ErrSealIneligible
	}
	if _, err = c.Ledger.AdvanceFromNative(ctx, c.NativeLedger, permitted); err != nil {
		return replication.ProjectionState{}, fmt.Errorf("HA confirmed Ledger: %w", err)
	}
	if f.applyPhase != nil {
		f.applyPhase("ledger-applied")
	}
	if _, err = f.reopened.Derivative.CatchUpTypedLive(ctx, c.NativeLedger, c.Budget); err != nil {
		return replication.ProjectionState{}, fmt.Errorf("HA confirmed Usage: %w", err)
	}
	gov, err := c.NativeGovernance.ResolveProjectionPrefix(ctx, c.Budget, cut.Allowed[2].Sequence, cut.Heads[2])
	if err != nil {
		return replication.ProjectionState{}, err
	}
	if _, err = c.Governance.AdvanceAuthenticatedFromNative(ctx, c.NativeGovernance, gov, c.Budget); err != nil {
		return replication.ProjectionState{}, fmt.Errorf("HA confirmed Governance: %w", err)
	}
	if f.applyPhase != nil {
		f.applyPhase("governance-applied")
	}
	advanceDirectory := c.Directory.AdvanceHASelectedFromNative
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		advanceDirectory = c.Directory.AdvanceHASelectedReplicaDirectoryFromNative
	}
	if _, err = advanceDirectory(ctx, c.NativeLedger, c.Ledger); err != nil {
		return replication.ProjectionState{}, fmt.Errorf("HA confirmed Directory: %w", err)
	}
	head, err := c.Metadata.EpochHead()
	if err != nil || head.Epoch != cut.Allowed[3].Generation {
		return replication.ProjectionState{}, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if err = statecut.WithHAConfirmedSourceAuthority(ctx, f.owners, f.replication.receiver, prefix, func(ctx context.Context, actual statecut.HAConfirmedSources) error {
		if actual != cut {
			return authenticatedstate.ErrSealIneligible
		}
		_, replayErr := c.Metadata.ReplayTo(ctx, head, actual.Allowed[3].Sequence)
		return replayErr
	}); err != nil {
		return replication.ProjectionState{}, fmt.Errorf("HA confirmed Metadata: %w", err)
	}
	if f.applyPhase != nil {
		f.applyPhase("metadata-applied")
	}
	var final statecut.HAConfirmedSources
	if err := statecut.WithHAConfirmedSourceAuthority(ctx, f.owners, f.replication.receiver, prefix, func(_ context.Context, actual statecut.HAConfirmedSources) error { final = actual; return nil }); err != nil {
		return replication.ProjectionState{}, err
	}
	if final != cut {
		return replication.ProjectionState{}, authenticatedstate.ErrSealIneligible
	}
	return replication.ProjectionState{Index: cut.Position.Confirmed, MetadataEpoch: cut.Allowed[3].Generation, MetadataSequence: cut.Allowed[3].Sequence}, nil
}
