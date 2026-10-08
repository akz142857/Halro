package app

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

// HAMetadataMaintenance owns the real source/sink/coordinator/receiver and
// TLS sessions for one explicitly approved migration. It has no HTTP server,
// business facade, promotion endpoint or readiness callback. Normal Runtime
// startup continues to require complete HA seven-root selection.
type HAMetadataMaintenance struct {
	mu                    sync.Mutex
	cfg                   config.Config
	member                *replicationRuntime
	authority             *replication.MetadataBaselineAuthority
	baseline              *metadatastate.Baseline
	metadata              *boltstore.Store
	metadataLog           *metadatajournal.Log
	nativeLedger          *ledger.Log
	nativeAudit           *audit.Log
	nativeGovernance      *governance.Log
	ledgerProjection      *ledger.LedgerProjection
	ledgerScratch         *haMetadataScratch
	inventory             *diskinventory.Baseline
	growth                *runtimeLedgerAppendDiskOwner
	disk                  [7]*runtimeProjectionDiskPermit
	rootDisk, journalDisk resourcebudget.Permit
	verifyFee             resourcebudget.Permit
	closeErr              error
	closed                bool
}

type HAMetadataMaintenanceFailure struct {
	Owner *HAMetadataMaintenance
	Cause error
}

func (e *HAMetadataMaintenanceFailure) Error() string { return e.Cause.Error() }
func (e *HAMetadataMaintenanceFailure) Unwrap() error { return e.Cause }

// OpenHAMetadataMaintenance requires the original offline member lock and the
// real sampled Manager. The bounded signed carrier independently selects the
// baseline root/header before candidate bytes are opened. Source and member
// authentication precede any transport publication.
func OpenHAMetadataMaintenance(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger) (result *HAMetadataMaintenance, resultErr error) {
	return openHAMetadataMaintenance(ctx, cfg, keys, guard, manager, logger, resourcebudget.Permit{}, resourcebudget.Permit{})
}

// The installer may lend its already reconciled persistent reservations only
// in this same Manager. RetainCovering verifies the live accounting authority;
// a caller cannot use candidate bytes or a stale permit to create credit.
func openHAMetadataMaintenance(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger, rootReservation, journalReservation resourcebudget.Permit) (result *HAMetadataMaintenance, resultErr error) {
	if cfg.Replication == nil || cfg.ResourceBudget == nil || manager == nil || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) || logger == nil {
		return nil, errors.New("HA Metadata maintenance requires the actual offline member lock, sampled budget and logger")
	}
	if err := keys.validate(); err != nil {
		return nil, err
	}
	if err := cfg.ResourceBudget.ValidateReplicationCandidates(); err != nil {
		return nil, err
	}
	state, clusterKey, err := readMemberState(cfg, keys.Master)
	if err != nil {
		return nil, err
	}
	defer clear(clusterKey[:])
	workspace := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory)
	m := &HAMetadataMaintenance{cfg: cfg}
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				m.closeErr = errors.Join(m.closeErr, resultErr)
			}
			if err := m.Close(); err != nil {
				result = m
				resultErr = &HAMetadataMaintenanceFailure{Owner: m, Cause: errors.Join(resultErr, err)}
			}
		}
	}()
	m.verifyFee, err = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: maxHAMetadataCarrierBytes})
	if err != nil {
		return m, err
	}
	carrier, authority, err := readHAMetadataCarrier(ctx, filepath.Join(workspace, "carrier.json"), clusterKey[:], state)
	if err != nil {
		return nil, err
	}
	claims := authority.Claims()
	if claims.New.MaxFileBytes != cfg.ResourceBudget.MetadataJournalDiskBytes {
		return nil, errors.New("HA Metadata local file ceiling differs from independent group permission")
	}
	m.authority = authority
	if err := m.verifyFee.Release(); err != nil {
		return m, err
	}
	m.verifyFee = resourcebudget.Permit{}
	pressure := &resourcePressureRuntime{budget: manager}
	m.growth, err = prepareRuntimeLedgerAppendDisk(ctx, cfg, pressure)
	if err != nil {
		return m, err
	}
	m.inventory, err = preparePersistentInventory(ctx, cfg, guard, pressure)
	if err != nil {
		return m, err
	}
	if err := m.growth.publish(); err != nil {
		return m, err
	}
	diskFactories := []func(context.Context, config.Config, *resourcePressureRuntime) (*runtimeProjectionDiskPermit, error){acquireMetadataStoreDisk, acquireMetadataJournalDisk, acquireOrderingJournalDisk, acquireAuditLogDisk, acquireGovernanceLogDisk, acquireLedgerProjectionDisk, acquireLedgerSegmentIndexDisk}
	for i, factory := range diskFactories {
		m.disk[i], err = factory(ctx, cfg, pressure)
		if err != nil {
			return m, err
		}
	}
	if rootReservation != (resourcebudget.Permit{}) {
		m.rootDisk, err = rootReservation.RetainCovering(manager, resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataStoreDiskBytes})
	} else {
		m.rootDisk, err = manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataStoreDiskBytes})
	}
	if err != nil {
		return m, err
	}
	if journalReservation != (resourcebudget.Permit{}) {
		m.journalDisk, err = journalReservation.RetainCovering(manager, resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataJournalDiskBytes})
	} else {
		m.journalDisk, err = manager.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataJournalDiskBytes})
	}
	if err != nil {
		return m, err
	}
	newKey, err := hkdf.Key(sha256.New, keys.Ledger, claims.New.Baseline.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return m, err
	}
	defer clear(newKey)
	journal, err := replication.OpenExistingOrderingJournalWithOptions(cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC, orderingJournalOptions(cfg))
	if err != nil {
		return m, err
	}
	r := &replicationRuntime{role: state.Role, clusterKey: clusterKey, journal: journal, fatal: make(chan error, 1), objectSourceDir: providerObjectSourceDir(cfg)}
	m.member = r
	sourceOptions := replication.NativeSourceOptions{LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(), MetadataPath: filepathMetadataJournal(cfg), LedgerKey: keys.Ledger, AuditKey: keys.Audit, GovernanceKey: keys.Governance, MetadataKey: keys.Metadata, ProviderObjectDir: providerObjectSourceDir(cfg), LedgerDirectoryIndexMaxBytes: ledgerSegmentIndexShareBytes(cfg), ReaderBudget: manager, ReaderRAMBytes: replicationReaderRAM(cfg, manager) + replication.MetadataBaselineReaderRAMBytes, MetadataBaseline: &replication.MetadataBaselineSourceOptions{Authority: authority, Path: filepath.Join(workspace, "metadata.journal"), Key: newKey}}
	if m.growth != nil {
		sourceOptions.ReserveLedgerFileDisk = m.growth.reserveNativeFile
	}
	r.source, err = replication.NewNativeSource(sourceOptions)
	if err != nil {
		return m, err
	}
	if state.Role == replication.RoleReplica {
		err = replication.RecoverReplicaStores(journal, r.source)
	} else {
		err = replication.ReconcileLocalStores(state.ClusterID, state.Incarnation, state.ConfirmedIndex, journal, r.source)
	}
	if err != nil {
		return m, err
	}
	r.publisher, err = replication.NewStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], state)
	if err != nil {
		return m, err
	}
	// Header/root selection is independent even after a failed prior semantic
	// apply. The signed initial SealRoot can select its retained immutable pages;
	// OpenFromRoot reduces only the MemberState-permitted prefix.
	newPath := filepath.Join(workspace, "metadata.journal")
	generation, sequence, err := metadatajournal.BoundReplicationCursor(ctx, newPath, newKey, claims.New)
	if err != nil {
		return m, err
	}
	nativeHead, err := metadatajournal.BoundReplicationPrefixHead(ctx, newPath, newKey, claims.New, generation, sequence)
	if err != nil {
		return m, err
	}
	permittedSequence := uint64(0)
	if state.Projection.MetadataEpoch == claims.New.Baseline.Epoch {
		permittedSequence = state.Projection.MetadataSequence
	} else if state.Projection.MetadataEpoch != claims.Cursors[replication.StoreMetadata-replication.StoreLedger].Generation {
		return m, errors.New("HA maintenance Metadata projection belongs to another epoch")
	}
	permittedHead, err := metadatajournal.BoundReplicationPrefixHead(ctx, newPath, newKey, claims.New, generation, permittedSequence)
	if err != nil {
		return m, err
	}
	receipt := carrier.Preparation
	options := metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: receipt.Evidence.Identity, InstanceID: receipt.Evidence.InstanceID, KeyGeneration: receipt.Evidence.KeyGeneration, Budget: manager, MaxSourceBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxDiskBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxAppendBytes: min(cfg.ResourceBudget.MetadataStoreDiskBytes/2, 8<<20)}, Evidence: receipt.Evidence, SavedRoot: receipt.Root, SelectedSealRoot: &receipt.Reference.Root, SavedHead: receipt.Head, NativeHead: nativeHead, PermittedHead: permittedHead, RootDiskReservation: m.rootDisk, JournalDiskReservation: m.journalDisk, MaxJournalBytes: claims.New.MaxFileBytes}
	m.baseline, _, err = metadatastate.OpenFromRoot(ctx, keys.Ledger, filepath.Join(workspace, "metadata-root"), newPath, options)
	if err != nil {
		return m, fmt.Errorf("HA Metadata open selected preparation: %w", err)
	}
	if err := m.baseline.VerifyHAMaintenanceRoot(ctx); err != nil {
		return m, fmt.Errorf("HA Metadata complete preparation audit: %w", err)
	}
	m.metadata, err = boltstore.OpenReplicaWithDiskLimit(cfg.MetadataPath(), metadataLegacyFileCeiling(cfg))
	if err != nil {
		return m, err
	}
	oldPosition, err := m.metadata.ReadMetadataAppliedPosition(ctx)
	if err != nil {
		return m, err
	}
	oldCursor := claims.Cursors[replication.StoreMetadata-replication.StoreLedger]
	if state.Role == replication.RolePrimary {
		if oldPosition.Epoch != oldCursor.Generation || oldPosition.Sequence != oldCursor.Sequence {
			return m, errors.New("Primary local-applied Metadata differs from independently prepared durable old cut")
		}
	} else if state.Projection.MetadataEpoch == oldCursor.Generation {
		if oldPosition.Epoch != state.Projection.MetadataEpoch || oldPosition.Sequence != state.Projection.MetadataSequence {
			return m, errors.New("HA maintenance legacy projection differs from actual applied cut")
		}
	} else if oldPosition.Epoch != oldCursor.Generation || oldPosition.Sequence != oldCursor.Sequence {
		return m, errors.New("HA maintenance old projection is not frozen at the baseline boundary")
	}
	m.metadataLog, err = metadatajournal.OpenReplicaContextWithLimit(ctx, filepathMetadataJournal(cfg), keys.Metadata, cfg.ResourceBudget.MetadataJournalDiskBytes)
	if err != nil {
		return m, err
	}
	if _, err = m.metadata.AttachReplicaMetadataJournal(m.metadataLog, keys.Metadata); err != nil {
		return m, err
	}
	ledgerOptions := ledger.Options{Replica: true, ChainKey: keys.Ledger, MaxSegmentDirectoryIndexBytes: ledgerSegmentIndexShareBytes(cfg)}
	if m.growth != nil {
		ledgerOptions.ReserveAppendDisk = m.growth.reserve
		ledgerOptions.ReserveNativeFileDisk = m.growth.reserveNativeFile
	}
	m.nativeLedger, err = ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledgerOptions)
	if err != nil {
		return m, err
	}
	m.nativeAudit, err = openConfiguredAuditLog(cfg, cfg.AuditPath(), keys.Audit, audit.Options{Replica: true, MaxDiskBytes: cfg.ResourceBudget.AuditLogDiskBytes}, manager)
	if err != nil {
		return m, err
	}
	m.nativeGovernance, err = governance.OpenWithOptions(cfg.GovernancePath(), keys.Governance, governance.Options{Replica: true, MaxDiskBytes: cfg.ResourceBudget.GovernanceLogDiskBytes, CacheBudget: manager, CacheRAMBytes: cfg.ResourceBudget.GovernanceLogRAMBytes})
	if err != nil {
		return m, err
	}
	peers := make([]string, 0, len(state.Peers))
	endpoints := make([]replication.PeerEndpoint, 0, len(state.Peers))
	for _, peer := range state.Peers {
		peers = append(peers, peer.Name)
		endpoints = append(endpoints, replication.PeerEndpoint{NodeID: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	session, err := replicationSessionOptions(cfg, manager)
	if err != nil {
		return m, err
	}
	var handler replication.ConnectionRecordHandler
	sender := &deferredPeerSender{}
	if state.Role == replication.RolePrimary {
		outbound, err := replication.NewConnectionOutbound(peers, sender)
		if err != nil {
			return m, err
		}
		limits := replication.DefaultPrimaryWindowLimits()
		limits.CandidateBudget = manager
		limits.CandidateRAMBytes = cfg.ResourceBudget.ReplicationCandidateRAMBytes
		if cfg.ResourceBudget.TotalUnconfirmedBytes > 0 {
			limits.UnconfirmedBudget = manager
			limits.MaxUnconfirmedEntries = cfg.ResourceBudget.UnconfirmedMaxEntries
			limits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
			limits.RecoveryReserveUnconfirmedBytes = cfg.ResourceBudget.RecoveryReserveUnconfirmedBytes
		}
		if err := limits.ValidateSession(session); err != nil {
			return m, err
		}
		r.coordinator, err = replication.NewPrimaryCoordinatorFromSource(state.ClusterID, state.Incarnation, state.NodeID, state.Term, state.ConfirmedIndex, peers, journal, outbound, r.publisher.PublishPrimary, r.source, limits)
		if err != nil {
			return m, err
		}
		actual, err := replication.NewPrimaryRecordHandler(r.coordinator)
		if err != nil {
			return m, err
		}
		handler = actual.Handle
		if err := m.baseline.ConfigureHAPrimaryJournal(ctx, claims.New, func(batch metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) {
			return r.coordinator.ReserveNativeSourceUnconfirmed(len(batch.Frames))
		}, func(batch metadatajournal.DurableBatch) (uint64, error) {
			commit, err := r.coordinator.RecordMetadataBatch(batch)
			return commit.Index, err
		}, r.coordinator.WaitConfirmed); err != nil {
			return m, err
		}
	} else {
		successor, err := m.baseline.ConfigureHAReplicaJournal(ctx, keys.Ledger, claims.New)
		if err != nil {
			return m, err
		}
		sink, err := replication.NewNativeSink(m.nativeLedger, m.nativeAudit, m.nativeGovernance, m.metadataLog, filepath.Join(cfg.Storage.DataDir, "provider-objects"), providerObjectSourceDir(cfg))
		if err != nil {
			return m, err
		}
		if err := sink.ConfigureMetadataBaseline(authority, r.source, successor); err != nil {
			return m, err
		}
		r.receiver, err = replication.NewReplicaReceiver(state.ClusterID, state.Incarnation, state.NodeID, claims.Primary, state.Term, state.PromisedTerm, state.ConfirmedIndex, state.AppliedIndex, journal, sink, replication.ReplicaReceiverOptions{PersistState: r.publisher.PublishReplica, Projection: state.Projection})
		if err != nil {
			return m, err
		}
		work, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: cfg.ResourceBudget.AccountingRecoveryRAMBytes})
		if err != nil {
			return m, err
		}
		m.ledgerScratch, err = createHAMetadataScratch(ctx, workspace, clusterKey[:], authority, state, manager)
		if err != nil {
			return m, errors.Join(err, work.Release())
		}
		path := filepath.Join(m.ledgerScratch.path, "projection.db")
		m.ledgerProjection, _, err = r.buildReplicaLedgerOwnerWithLimit(ctx, m.nativeLedger, path, cfg.ResourceBudget.LedgerProjectionDiskBytes, nil)
		if err = errors.Join(err, work.Release()); err != nil {
			return m, err
		}
		projection, err := replication.NewNativeProjectionWithLedgerOwner(m.nativeLedger, m.ledgerProjection, m.metadata)
		if err != nil {
			return m, err
		}
		owner := &haMetadataProjection{baseline: m.baseline, legacy: m.metadata, authority: authority, selected: state.Projection.MetadataEpoch == claims.New.Baseline.Epoch}
		if err := projection.ConfigureMetadataBaseline(owner); err != nil {
			return m, err
		}
		r.applier, err = replication.NewReplicaApplier(r.receiver, journal, projection)
		if err != nil {
			return m, err
		}
		actual, err := replication.NewReplicaRecordHandler(r.receiver, r.applier)
		if err != nil {
			return m, err
		}
		handler = actual.Handle
	}
	managerOptions := replication.ConnectionManagerOptions{LocalNode: state.NodeID, TLS: replication.TLSFiles{CAFile: cfg.Replication.TLS.CAFile, CertFile: cfg.Replication.TLS.CertFile, KeyFile: cfg.Replication.TLS.KeyFile}, ClusterKey: clusterKey[:], Peers: endpoints, LocalHello: m.localHello, OnAuthenticatedHello: m.authorizeHello, Handle: handler, Session: session, OnSessionError: func(peer string, err error) {
		logger.Warn("HA Metadata maintenance peer failed", "peer", peer, "error", err)
	}}
	if r.coordinator != nil {
		managerOptions.OnDataSessionReady = func(peer string, hello replication.Hello, send func([]byte) error) error {
			return r.coordinator.PreparePeer(peer, hello.DurableIndex, hello.AppliedIndex, send)
		}
		managerOptions.OnDataSessionClosed = r.coordinator.ForgetPeer
	}
	r.manager, err = replication.NewConnectionManager(managerOptions)
	if err != nil {
		return m, err
	}
	sender.Set(r.manager)
	result = m
	return result, nil
}

func (m *HAMetadataMaintenance) localHello() (replication.Hello, error) {
	hello, err := m.member.localHello()
	if err != nil {
		return hello, err
	}
	hello.Protocol = replication.VersionRange{Current: 2, Minimum: 2, Maximum: 2}
	hello.MetadataBaselineAuthority = m.authority.Descriptor().AuthorityDigest
	hello.Metadata = replication.VersionRange{Current: hello.Metadata.Current, Minimum: 1, Maximum: 2}
	return hello, nil
}
func (m *HAMetadataMaintenance) authorizeHello(_ context.Context, peer replication.Hello) (bool, error) {
	state := m.member.publisher.Snapshot()
	claims := m.authority.Claims()
	if peer.ClusterID != state.ClusterID || peer.Incarnation != state.Incarnation || peer.Term != state.Term || peer.PromisedTerm != state.Term || peer.Protocol.Current != 2 || peer.Protocol.Minimum != 2 || peer.Protocol.Maximum != 2 || peer.Metadata.Minimum != 1 || peer.Metadata.Maximum != 2 || peer.DurableIndex > claims.Index+1 || peer.MetadataBaselineAuthority != m.authority.Descriptor().AuthorityDigest {
		return false, errors.New("peer has no compatible same-term Metadata maintenance capability")
	}
	if state.Role == replication.RolePrimary && peer.Role != replication.RoleReplica || state.Role == replication.RoleReplica && (peer.Role != replication.RolePrimary || peer.NodeID != claims.Primary) {
		return false, errors.New("Metadata maintenance peer role differs from independent carrier")
	}
	return true, nil
}

// Run serves only the authenticated replication listener. Primary completes
// N confirmation and the N+1 boundary; Replica waits for actual apply at N+1.
// Both keep sessions alive until cancellation so every approved member can ACK.
func (m *HAMetadataMaintenance) Run(ctx context.Context, onBoundary func(replication.MemberState) error) error {
	listener, err := net.Listen("tcp", m.cfg.Replication.Listen)
	if err != nil {
		return err
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- m.member.serve(runCtx, listener) }()
	boundaryDone := make(chan error, 1)
	go func() {
		var err error
		if m.member.coordinator != nil {
			err = m.completeBoundary(runCtx)
			if err == nil && onBoundary != nil {
				err = onBoundary(m.member.publisher.Snapshot())
			}
			boundaryDone <- err
			return
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			state := m.member.publisher.Snapshot()
			if state.AppliedIndex >= m.authority.Claims().Index+1 {
				var err error
				if onBoundary != nil {
					err = onBoundary(state)
				}
				boundaryDone <- err
				return
			}
			select {
			case <-runCtx.Done():
				boundaryDone <- runCtx.Err()
				return
			case <-ticker.C:
			}
		}
	}()
	var boundaryErr error
	select {
	case err = <-done:
		stop()
		boundaryErr = <-boundaryDone
		return errors.Join(err, boundaryErr)
	case boundaryErr = <-boundaryDone:
		if boundaryErr != nil {
			stop()
			return errors.Join(boundaryErr, <-done)
		}
	case <-ctx.Done():
		stop()
		boundaryErr = <-boundaryDone
		return errors.Join(ctx.Err(), boundaryErr, <-done)
	}
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		stop()
		return errors.Join(ctx.Err(), <-done)
	}
}

func (m *HAMetadataMaintenance) completeBoundary(ctx context.Context) error {
	// Initial/replaced sockets may have no prepared source window yet. Retry
	// only transport unavailability; actual journal head decides whether N+1
	// was already durable, so reconnect never appends a second boundary.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		head, _, _ := m.member.journal.Head()
		var err error
		if head == m.authority.Claims().Index {
			_, err = m.member.coordinator.RecordMetadataBaseline(ctx, m.authority)
		} else if head == m.authority.Claims().Index+1 {
			err = m.member.coordinator.WaitConfirmed(ctx, head)
		} else {
			return errors.New("HA maintenance head is outside its one authorized boundary")
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, replication.ErrReplicationUnavailable) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (m *HAMetadataMaintenance) Snapshot() replication.MemberState {
	return m.member.publisher.Snapshot()
}

func (m *HAMetadataMaintenance) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	m.closeErr = errors.Join(m.closeErr, m.member.close())
	if m.closeErr == nil && m.verifyFee != (resourcebudget.Permit{}) {
		m.closeErr = m.verifyFee.Release()
		if m.closeErr == nil {
			m.verifyFee = resourcebudget.Permit{}
		}
	}
	// Transport joins first. A callback's borrowed root/log never loses its fee
	// or key before the actual invocation returns.
	if m.closeErr != nil {
		return m.closeErr
	}
	if m.baseline != nil {
		m.closeErr = errors.Join(m.closeErr, m.baseline.Close())
	}
	if m.metadata != nil {
		m.closeErr = errors.Join(m.closeErr, m.metadata.Close())
	}
	if m.metadataLog != nil {
		m.closeErr = errors.Join(m.closeErr, m.metadataLog.Close())
	}
	if m.nativeLedger != nil {
		m.closeErr = errors.Join(m.closeErr, m.nativeLedger.Close())
	}
	if m.nativeAudit != nil {
		m.closeErr = errors.Join(m.closeErr, m.nativeAudit.Close())
	}
	if m.nativeGovernance != nil {
		m.closeErr = errors.Join(m.closeErr, m.nativeGovernance.Close())
	}
	if m.ledgerProjection != nil {
		if err := m.ledgerProjection.Discard(); err != nil {
			m.closeErr = errors.Join(m.closeErr, ownedfile.ErrRetirement, err)
		}
	}
	if m.closeErr == nil {
		m.closeErr = errors.Join(m.closeErr, m.ledgerScratch.Discard())
	}
	if m.closeErr != nil {
		return m.closeErr
	}
	// Only the actual fresh Ledger derivative and disposable directory indexes
	// retired their physical bytes. Native/root/ordering Disk ceilings persist;
	// Close cannot refund their surviving inodes or a signed baseline's pages.
	m.closeErr = errors.Join(m.closeErr, m.disk[0].retireRAMAfterClose(nil), m.disk[5].Release(), m.disk[6].Release(), m.growth.close(), m.inventory.Close())
	return m.closeErr
}

type haMetadataProjection struct {
	baseline  *metadatastate.Baseline
	legacy    *boltstore.Store
	authority *replication.MetadataBaselineAuthority
	selected  bool
}

func (p *haMetadataProjection) ApplyMetadataBaseline(ctx context.Context, b replication.MetadataBaselineMetadata) error {
	if b != p.authority.Descriptor() {
		return errors.New("confirmed Metadata baseline differs from independent projection permission")
	}
	head, err := p.baseline.AppliedHead()
	if err != nil {
		return err
	}
	if head.Epoch != b.Epoch || head.Baseline != p.authority.Claims().New.Baseline {
		return errors.New("confirmed Metadata baseline does not own qualified content")
	}
	p.selected = true
	return ctx.Err()
}
func (p *haMetadataProjection) ApplyMetadataThrough(ctx context.Context, epoch, sequence uint64) error {
	if epoch == p.authority.Descriptor().PreviousEpoch {
		_, err := p.legacy.ApplyReplicaMetadataThrough(ctx, epoch, sequence)
		return err
	}
	if !p.selected || epoch != p.authority.Descriptor().Epoch {
		return errors.New("Metadata successor projection has no confirmed baseline")
	}
	head, err := p.baseline.EpochHead()
	if err != nil {
		return err
	}
	_, err = p.baseline.ReplayTo(ctx, head, sequence)
	return err
}
func (p *haMetadataProjection) ValidateSchemaBoundary(ctx context.Context, from, to uint32) error {
	if p.selected {
		if uint64(to) != p.authority.Claims().New.Baseline.Schema || to <= from {
			return errors.New("Metadata baseline schema boundary differs")
		}
		return ctx.Err()
	}
	version, err := p.legacy.SchemaVersion()
	if err != nil || version != uint64(to) || to <= from {
		return errors.Join(err, errors.New("legacy Metadata schema boundary differs"))
	}
	return ctx.Err()
}
