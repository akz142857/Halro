package app

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/bearercred"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	storelock "github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
	"github.com/go-chi/chi/v5"
)

const memberBinaryVersion uint16 = 1

const reseedRequiredMarker = "reseed-required"

func orderingJournalOptions(cfg config.Config) replication.OrderingJournalOptions {
	options := replication.OrderingJournalOptions{}
	if cfg.ResourceBudget != nil {
		options.MaxDiskBytes = cfg.ResourceBudget.OrderingJournalDiskBytes
	}
	return options
}

// replicationRuntime is the one HA subsystem owned by Runtime. Its internals
// deliberately stay out of Runtime's already-wide field list: role state,
// ordering, native-source authentication and peer transport have one lifetime.
type replicationRuntime struct {
	peerPhysical      atomic.Pointer[haPeerPhysicalController]
	role              replication.Role
	clusterKey        [32]byte
	publisher         *replication.StatePublisher
	journal           *replication.OrderingJournal
	source            *replication.NativeSource
	manager           *replication.ConnectionManager
	coordinator       *replication.PrimaryCoordinator
	receiver          *replication.ReplicaReceiver
	applier           *replication.ReplicaApplier
	startupReady      atomic.Bool
	helloMu           sync.Mutex
	startupPeers      map[string]replication.Hello
	startupIndex      uint64
	startupPeerQuorum int
	startupMu         sync.Mutex
	startupClosed     bool
	startupDone       chan struct{}
	startupFinish     func(context.Context) error // Installed before transport publication.
	startupProducers  chan struct{}               // Closed only after confirmed accounting recovery.
	startupCtx        context.Context
	startupStop       context.CancelFunc
	fatal             chan error
	objectSourceDir   string
	// plannedStepdownDrain is installed by Runtime before the replication
	// listener starts. A planned promise is not a crash fence: the old Primary
	// must first stop accepting HTTP work and wait for every admitted handler to
	// finish, otherwise one paused before Provider I/O can run after promotion.
	plannedStepdownDrain func() error
	incompatibleSchema   atomic.Uint64
	incompatibleKey      atomic.Uint64
	incompatibleSPKI     atomic.Uint64
}

func openReplicaApplication(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	dataLock *storelock.Lock,
	secretVault *vault.Vault,
	masterKey, adminSessionKey []byte,
	metricsTokenHash [32]byte,
	metricsAuthorizer *bearercred.Authorizer,
	state replication.MemberState,
	clusterKey [32]byte,
	resourcePressure *resourcePressureRuntime,
	orderingJournalDiskPermit *runtimeProjectionDiskPermit,
	ledgerSegmentIndexDiskPermit *runtimeProjectionDiskPermit,
	metadataStoreDiskPermit *runtimeProjectionDiskPermit,
	ledgerAppendDisk *runtimeLedgerAppendDiskOwner,
) (*Runtime, error) {
	metadata, err := boltstore.OpenReplicaWithDiskLimit(cfg.MetadataPath(), metadataStoreDiskCeiling(cfg))
	if err != nil {
		return nil, err
	}
	failMetadata := func(openErr error) (*Runtime, error) {
		return nil, errors.Join(openErr, metadata.Close())
	}
	if err := configurePricingResources(cfg, resourcePressure.logicalBudget(), metadata); err != nil {
		return failMetadata(err)
	}
	if err := verifyVaultKeyCheck(metadata, secretVault); err != nil {
		return failMetadata(err)
	}
	auditKey, err := loadAuditHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return failMetadata(err)
	}
	defer clear(auditKey)
	ledgerKey, err := loadLedgerHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return failMetadata(err)
	}
	defer clear(ledgerKey)
	governanceKey, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		return failMetadata(err)
	}
	defer clear(governanceKey)
	metadataKey, err := loadMetadataJournalHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return failMetadata(err)
	}
	defer clear(metadataKey)

	replicationRuntime, err := openReplicaReplicationFoundation(
		cfg, state, clusterKey, ledgerKey, auditKey, governanceKey, metadataKey, resourcePressure.logicalBudget(), ledgerAppendDisk,
	)
	if err != nil {
		return failMetadata(err)
	}
	failReplication := func(openErr error) (*Runtime, error) {
		return failMetadata(errors.Join(openErr, replicationRuntime.close()))
	}
	metadataJournalDiskPermit, err := acquireMetadataJournalDisk(ctx, cfg, resourcePressure)
	if err != nil {
		return failReplication(fmt.Errorf("acquire Replica Metadata journal disk ceiling: %w", err))
	}
	metadataJournalDiskPermitOwned := true
	defer func() {
		if metadataJournalDiskPermitOwned {
			_ = metadataJournalDiskPermit.Release()
		}
	}()
	metadataJournalMaxBytes := int64(0)
	if cfg.ResourceBudget != nil {
		metadataJournalMaxBytes = cfg.ResourceBudget.MetadataJournalDiskBytes
	}
	metadataLog, err := metadatajournal.OpenReplicaContextWithLimit(ctx, filepathMetadataJournal(cfg), metadataKey, metadataJournalMaxBytes)
	if err != nil {
		return failReplication(fmt.Errorf("open Replica metadata journal: %w", err))
	}
	journalAttached := false
	defer func() {
		if !journalAttached {
			_ = metadataLog.Close()
		}
	}()
	journalState, err := metadata.AttachReplicaMetadataJournal(metadataLog, metadataKey)
	if err != nil {
		return failReplication(fmt.Errorf("attach Replica metadata journal: %w", err))
	}
	journalAttached = true
	if journalState.Epoch != state.Projection.MetadataEpoch || journalState.Applied != state.Projection.MetadataSequence {
		return failReplication(errors.New("Replica metadata projection does not match authenticated member state"))
	}

	accountingStatus := ledger.NewStatus()
	ledgerOptions := ledger.Options{ChainKey: ledgerKey, Replica: true, MaxSegmentDirectoryIndexBytes: ledgerSegmentIndexShareBytes(cfg), ReserveAppendDisk: ledgerAppendDisk.reserve}
	if ledgerAppendDisk != nil {
		ledgerOptions.ReserveNativeFileDisk = ledgerAppendDisk.reserveNativeFile
	}
	ledgerLog, err := ledger.OpenWithOptions(cfg.LedgerPath(), accountingStatus, ledgerOptions)
	if err != nil {
		return failReplication(fmt.Errorf("open Replica Ledger: %w", err))
	}
	if err := ledgerAppendDisk.publish(); err != nil {
		return failReplication(errors.Join(fmt.Errorf("publish Replica Ledger append growth owner: %w", err), ledgerLog.Close()))
	}
	var accountingGeneration *replicaAccountingGeneration
	failLedger := func(openErr error) (*Runtime, error) {
		// No transport/public owner has been published on failed startup.
		return failReplication(errors.Join(openErr, accountingGeneration.Close(), ledgerLog.Close()))
	}
	if _, err := replicationRuntime.authenticateHARuntimeCut(ctx, resourcePressure.logicalBudget()); err != nil {
		return failLedger(fmt.Errorf("authenticate Replica common applied cut before generation construction: %w", err))
	}
	accountingGeneration, err = prepareReplicaRuntimeOwner(ctx, cfg, replicationRuntime, ledgerLog, metadata, resourcePressure)
	if err != nil {
		return failLedger(fmt.Errorf("prepare Replica accounting generation: %w", err))
	}

	auditDiskPermit, err := acquireAuditLogDisk(ctx, cfg, resourcePressure)
	if err != nil {
		return failLedger(fmt.Errorf("acquire Replica Audit journal disk ceiling: %w", err))
	}
	auditLog, err := openConfiguredAuditLog(cfg, cfg.AuditPath(), auditKey, audit.Options{Replica: true}, resourcePressure.logicalBudget())
	if err != nil {
		return failLedger(errors.Join(fmt.Errorf("open Replica Audit log: %w", err), auditDiskPermit.Release()))
	}
	failAudit := func(openErr error) (*Runtime, error) {
		return failLedger(errors.Join(openErr, auditLog.Close(), auditDiskPermit.Release()))
	}
	governanceLogDiskPermit, err := acquireGovernanceLogDisk(ctx, cfg, resourcePressure)
	if err != nil {
		return failAudit(fmt.Errorf("acquire Replica Governance journal disk ceiling: %w", err))
	}
	governanceLog, err := governance.OpenWithOptions(cfg.GovernancePath(), governanceKey, governanceLogOptions(cfg, resourcePressure.logicalBudget(), governance.Options{Replica: true}))
	if err != nil {
		return failAudit(fmt.Errorf("open Replica Governance journal: %w", err))
	}
	failGovernance := func(openErr error) (*Runtime, error) {
		return failAudit(errors.Join(openErr, governanceLog.Close()))
	}
	sink, err := replication.NewNativeSink(
		ledgerLog, auditLog, governanceLog, metadataLog,
		filepath.Join(cfg.Storage.DataDir, "provider-objects"),
		providerObjectSourceDir(cfg),
	)
	if err != nil {
		return failGovernance(err)
	}
	receiver, err := replication.NewReplicaReceiver(
		state.ClusterID, state.Incarnation, state.NodeID, "", state.Term, state.PromisedTerm,
		state.ConfirmedIndex, state.AppliedIndex, replicationRuntime.journal, sink,
		replication.ReplicaReceiverOptions{PersistState: replicationRuntime.publisher.PublishReplica, Projection: state.Projection},
	)
	if err != nil {
		return failGovernance(err)
	}
	applier, err := replication.NewReplicaApplier(receiver, replicationRuntime.journal, accountingGeneration)
	if err != nil {
		return failGovernance(err)
	}
	replicationRuntime.receiver = receiver
	replicationRuntime.applier = applier
	if err := replicationRuntime.completeReplicaTransport(cfg, logger, resourcePressure.logicalBudget()); err != nil {
		return failGovernance(err)
	}
	if _, err := applier.ApplyConfirmed(ctx); err != nil {
		return failGovernance(fmt.Errorf("apply Replica confirmed prefix at startup: %w", err))
	}
	if orderingJournalDiskPermit != nil {
		if err := orderingJournalDiskPermit.Reclassify(resourcebudget.Background); err != nil {
			return failGovernance(fmt.Errorf("publish Replica Ordering journal disk owner: %w", err))
		}
	}
	if metadataJournalDiskPermit != nil {
		if err := metadataJournalDiskPermit.Reclassify(resourcebudget.Background); err != nil {
			return failGovernance(fmt.Errorf("publish Replica Metadata journal disk owner: %w", err))
		}
	}
	if metadataStoreDiskPermit != nil {
		if err := metadataStoreDiskPermit.Reclassify(resourcebudget.Background); err != nil {
			return failGovernance(fmt.Errorf("publish Replica Metadata data-file disk owner: %w", err))
		}
	}
	if auditDiskPermit != nil {
		if err := auditDiskPermit.Reclassify(resourcebudget.Background); err != nil {
			return failGovernance(fmt.Errorf("publish Replica Audit journal disk owner: %w", err))
		}
	}
	if governanceLogDiskPermit != nil {
		if err := governanceLogDiskPermit.Reclassify(resourcebudget.Background); err != nil {
			return failGovernance(fmt.Errorf("publish Replica Governance journal disk owner: %w", err))
		}
	}
	if ledgerSegmentIndexDiskPermit != nil {
		if err := ledgerSegmentIndexDiskPermit.Reclassify(resourcebudget.Background); err != nil {
			return failGovernance(fmt.Errorf("publish Replica Ledger segment index disk owner: %w", err))
		}
	}
	memorySessions, err := adminauth.NewMemorySessionStore(metadata)
	if err != nil {
		return failGovernance(err)
	}
	adminSessions, err := adminauth.NewManager(memorySessions, adminSessionKey, cfg.Admin.SessionTTL.Value(), cfg.Admin.IdleTimeout.Value())
	if err != nil {
		return failGovernance(err)
	}
	backgroundContext, backgroundCancel := context.WithCancel(context.Background())
	runtime := &Runtime{
		config: cfg, logger: logger, lock: dataLock, store: metadata,
		ledger: ledgerLog, accountingState: runtimeAccountingState{reader: accountingGeneration.ledger, projection: accountingGeneration.ledger, replica: accountingGeneration}, status: accountingStatus,
		governance: governanceRuntime{log: governanceLog, logDiskPermit: governanceLogDiskPermit}, vault: secretVault, audit: auditLog,
		adminSessions: adminSessions, startedAt: time.Now(), now: time.Now,
		backgroundCtx: backgroundContext, backgroundCancel: backgroundCancel,
		metricsScrapes:   make(chan struct{}, max(1, cfg.Metrics.MaxConcurrentScrapes)),
		metricsTokenHash: metricsTokenHash, metricsAuthorizer: metricsAuthorizer,
		usage: &runtimeUsage{disk: accountingGeneration.usageDisk}, replication: replicationRuntime,

		runtimeResourceOwnership: runtimeResourceOwnership{
			resourcePressure: resourcePressure, auditDiskPermit: auditDiskPermit,
			maintenanceOwners:            newRuntimeMaintenanceOwners(cfg, resourcePressure),
			metadataJournalDiskPermit:    metadataJournalDiskPermit,
			metadataStoreDiskPermit:      metadataStoreDiskPermit,
			orderingJournalDiskPermit:    orderingJournalDiskPermit,
			ledgerSegmentIndexDiskPermit: ledgerSegmentIndexDiskPermit,
			ledgerAppendDisk:             ledgerAppendDisk,
		},
	}
	if resourcePressure != nil {
		runtime.backgroundWait.Add(1)
		go func() {
			defer runtime.backgroundWait.Done()
			runtime.runResourcePressure(backgroundContext, resourcePressure)
		}()
	}
	// Replica maintenance is intentionally limited to node-derived Usage
	// checkpointing and node-local Ledger compression. It does not start the
	// Primary collector/export/seal/anchor/alert loops.
	runtime.backgroundWait.Add(1)
	go func() {
		defer runtime.backgroundWait.Done()
		runtime.runReplicaMaintenance(backgroundContext)
	}()
	metadataJournalDiskPermitOwned = false
	return runtime, nil
}

func openReplicaReplicationFoundation(
	cfg config.Config,
	state replication.MemberState,
	clusterKey [32]byte,
	ledgerKey, auditKey, governanceKey, metadataKey []byte,
	budget *resourcebudget.Manager,
	ledgerAppendDisk *runtimeLedgerAppendDiskOwner,
) (*replicationRuntime, error) {
	if budget != nil {
		if cfg.ResourceBudget == nil {
			return nil, errors.New("Replica reader budget requires resource_budget")
		}
		if err := cfg.ResourceBudget.ValidateReplicationCandidates(); err != nil {
			return nil, fmt.Errorf("Replica resource_budget: %w", err)
		}
	}
	if state.Role != replication.RoleReplica {
		return nil, fmt.Errorf("member role %q is not a Replica", state.Role)
	}
	if err := refuseReseedRequiredMember(cfg); err != nil {
		return nil, err
	}
	journal, err := replication.OpenExistingOrderingJournalWithOptions(
		cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation,
		state.DurableIndex, state.OrderingHeadMAC, orderingJournalOptions(cfg),
	)
	if err != nil {
		return nil, err
	}
	sourceOptions := replication.NativeSourceOptions{
		LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(),
		MetadataPath: filepathMetadataJournal(cfg), LedgerKey: ledgerKey, AuditKey: auditKey,
		GovernanceKey: governanceKey, MetadataKey: metadataKey,
		ProviderObjectDir:            providerObjectSourceDir(cfg),
		LedgerDirectoryIndexMaxBytes: ledgerSegmentIndexShareBytes(cfg),
		ReaderBudget:                 budget, ReaderRAMBytes: replicationReaderRAM(cfg, budget),
	}
	if ledgerAppendDisk != nil {
		sourceOptions.ReserveLedgerFileDisk = ledgerAppendDisk.reserveNativeFile
	}
	source, err := replication.NewNativeSource(sourceOptions)
	if err != nil {
		return nil, errors.Join(err, journal.Close())
	}
	if err := replication.RecoverReplicaStores(journal, source); err != nil {
		return nil, errors.Join(err, source.Close(), journal.Close())
	}
	publisher, err := replication.NewStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], state)
	if err != nil {
		return nil, errors.Join(err, source.Close(), journal.Close())
	}
	runtime := &replicationRuntime{
		role: state.Role, clusterKey: clusterKey, publisher: publisher, journal: journal, source: source,
		startupPeers: make(map[string]replication.Hello), fatal: make(chan error, 1), objectSourceDir: providerObjectSourceDir(cfg),
	}
	// A Replica remains ready while it waits for a Primary (§8.2 row 6). Its
	// data plane still returns not_primary; readiness says it is a healthy
	// candidate and member endpoint.
	runtime.startupReady.Store(true)
	return runtime, nil
}

func (r *replicationRuntime) completeReplicaTransport(cfg config.Config, logger *slog.Logger, budget *resourcebudget.Manager) error {
	handler, err := replication.NewReplicaRecordHandler(r.receiver, r.applier)
	if err != nil {
		return err
	}
	state := r.publisher.Snapshot()
	endpoints := make([]replication.PeerEndpoint, 0, len(state.Peers))
	for _, peer := range state.Peers {
		endpoints = append(endpoints, replication.PeerEndpoint{NodeID: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	sessionOptions, err := replicationSessionOptions(cfg, budget)
	if err != nil {
		return err
	}
	manager, err := replication.NewConnectionManager(replication.ConnectionManagerOptions{
		LocalNode: state.NodeID,
		TLS: replication.TLSFiles{
			CAFile: cfg.Replication.TLS.CAFile, CertFile: cfg.Replication.TLS.CertFile, KeyFile: cfg.Replication.TLS.KeyFile,
		},
		ClusterKey: r.clusterKey[:],
		Peers:      endpoints, LocalHello: r.localHello, OnAuthenticatedHello: r.authorizeReplicaHello,
		Handle: r.handleRecords(handler.Handle), Session: sessionOptions,
		OnSessionError: func(peer string, sessionErr error) {
			r.noteSessionError(sessionErr)
			logger.Warn("replication peer session failed", "peer", peer, "error", sessionErr)
		},
	})
	if err != nil {
		return err
	}
	r.manager = manager
	return nil
}

// deferredPeerSender breaks the construction cycle between a coordinator,
// whose recovered suffix must be authenticated before any network side effect,
// and the connection manager whose record handler calls that coordinator.
// Before installation, enqueue fails closed and the coordinator retains bytes.
type deferredPeerSender struct {
	mu     sync.RWMutex
	sender replication.PeerRecordSender
}

func (s *deferredPeerSender) Set(sender replication.PeerRecordSender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sender = sender
}

func (s *deferredPeerSender) Send(peer string, encoded []byte) error {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender == nil {
		return replication.ErrPeerNotConnected
	}
	return sender.Send(peer, encoded)
}

func readMemberState(cfg config.Config, masterKey []byte) (replication.MemberState, [32]byte, error) {
	key, incarnation, carried, err := readHAGroupClusterKeyCandidate(cfg, masterKey)
	if err != nil {
		return replication.MemberState{}, [32]byte{}, err
	}
	var state replication.MemberState
	if carried {
		// The encrypted carrier supplies only a key candidate. All Member facts
		// still come from the original complete authenticated state bytes.
		state, err = replication.ReadState(cfg.ReplicationStatePath(), key[:])
		if err == nil && sha256.Sum256([]byte(state.Incarnation)) != incarnation {
			err = errors.New("HA cluster-key candidate incarnation differs from authenticated Member")
		}
	} else {
		state, err = replication.ReadStateWithMasterKey(cfg.ReplicationStatePath(), masterKey)
		if err == nil {
			key, err = replication.DeriveClusterKey(masterKey, state.Incarnation)
		}
	}
	if err != nil {
		clear(key[:])
		return replication.MemberState{}, [32]byte{}, err
	}
	peers := make([]replication.StatePeer, 0, len(cfg.Replication.Peers))
	for _, peer := range cfg.Replication.Peers {
		peers = append(peers, replication.StatePeer{Name: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	if err := replication.ValidateMemberConfiguration(state, cfg.Replication.ClusterID, cfg.Replication.NodeID, peers); err != nil {
		clear(key[:])
		return replication.MemberState{}, [32]byte{}, err
	}
	return state, key, nil
}

func openPrimaryReplication(
	cfg config.Config,
	logger *slog.Logger,
	state replication.MemberState,
	clusterKey [32]byte,
	ledgerKey, auditKey, governanceKey, metadataKey []byte,
	candidateBudget *resourcebudget.Manager,
	ledgerAppendDisk *runtimeLedgerAppendDiskOwner,
) (*replicationRuntime, error) {
	if candidateBudget != nil {
		if cfg.ResourceBudget == nil {
			return nil, errors.New("Primary candidate budget requires resource_budget configuration")
		}
		if err := cfg.ResourceBudget.ValidateReplicationCandidates(); err != nil {
			return nil, fmt.Errorf("Primary resource_budget: %w", err)
		}
	}
	if state.Role != replication.RolePrimary {
		return nil, fmt.Errorf("member role %q is not a Primary", state.Role)
	}
	if err := refuseReseedRequiredMember(cfg); err != nil {
		return nil, err
	}
	journal, err := replication.OpenExistingOrderingJournalWithOptions(
		cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation,
		state.DurableIndex, state.OrderingHeadMAC, orderingJournalOptions(cfg),
	)
	if err != nil {
		return nil, err
	}
	failJournal := func(openErr error) (*replicationRuntime, error) {
		return nil, errors.Join(openErr, journal.Close())
	}
	sourceOptions := replication.NativeSourceOptions{
		LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(),
		MetadataPath: filepathMetadataJournal(cfg), LedgerKey: ledgerKey, AuditKey: auditKey,
		GovernanceKey: governanceKey, MetadataKey: metadataKey,
		ProviderObjectDir:            providerObjectSourceDir(cfg),
		LedgerDirectoryIndexMaxBytes: ledgerSegmentIndexShareBytes(cfg),
		ReaderBudget:                 candidateBudget, ReaderRAMBytes: replicationReaderRAM(cfg, candidateBudget),
	}
	if ledgerAppendDisk != nil {
		sourceOptions.ReserveLedgerFileDisk = ledgerAppendDisk.reserveNativeFile
	}
	source, err := replication.NewNativeSource(sourceOptions)
	if err != nil {
		return failJournal(err)
	}
	failSource := func(openErr error) (*replicationRuntime, error) {
		return failJournal(errors.Join(openErr, source.Close()))
	}
	if err := replication.ReconcileLocalStores(state.ClusterID, state.Incarnation, state.ConfirmedIndex, journal, source); err != nil {
		return failSource(err)
	}
	publisher, err := replication.NewStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], state)
	if err != nil {
		return failSource(err)
	}
	peerNames := make([]string, 0, len(state.Peers))
	endpoints := make([]replication.PeerEndpoint, 0, len(state.Peers))
	for _, peer := range state.Peers {
		peerNames = append(peerNames, peer.Name)
		endpoints = append(endpoints, replication.PeerEndpoint{
			NodeID: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256,
		})
	}
	sender := &deferredPeerSender{}
	outbound, err := replication.NewConnectionOutbound(peerNames, sender)
	if err != nil {
		publisher.Close()
		return failSource(err)
	}
	windowLimits := replication.DefaultPrimaryWindowLimits()
	if candidateBudget != nil {
		windowLimits.CandidateBudget = candidateBudget
		windowLimits.CandidateRAMBytes = cfg.ResourceBudget.ReplicationCandidateRAMBytes
		if cfg.ResourceBudget.TotalUnconfirmedBytes > 0 {
			windowLimits.UnconfirmedBudget = candidateBudget
			windowLimits.MaxUnconfirmedEntries = cfg.ResourceBudget.UnconfirmedMaxEntries
			windowLimits.UnconfirmedMetadataRAMBytes = replicationlimits.PrimaryUnconfirmedTableRAMBytes
			windowLimits.RecoveryReserveUnconfirmedBytes = cfg.ResourceBudget.RecoveryReserveUnconfirmedBytes
		}
	}
	sessionOptions, err := replicationSessionOptions(cfg, candidateBudget)
	if err != nil {
		publisher.Close()
		return failSource(err)
	}
	if err := windowLimits.ValidateSession(sessionOptions); err != nil {
		publisher.Close()
		return failSource(err)
	}
	coordinator, err := replication.NewPrimaryCoordinatorFromSource(
		state.ClusterID, state.Incarnation, state.NodeID, state.Term, state.ConfirmedIndex,
		peerNames, journal, outbound, publisher.PublishPrimary,
		source, windowLimits,
	)
	if err != nil {
		publisher.Close()
		return failSource(err)
	}
	handler, err := replication.NewPrimaryRecordHandler(coordinator)
	if err != nil {
		publisher.Close()
		return failSource(err)
	}
	runtime := &replicationRuntime{
		role: state.Role, clusterKey: clusterKey, publisher: publisher, journal: journal, source: source, coordinator: coordinator,
		startupPeers: make(map[string]replication.Hello), fatal: make(chan error, 1), objectSourceDir: providerObjectSourceDir(cfg),
	}
	runtime.startupCtx, runtime.startupStop = context.WithCancel(context.Background())
	manager, err := replication.NewConnectionManager(replication.ConnectionManagerOptions{
		LocalNode: state.NodeID,
		TLS: replication.TLSFiles{
			CAFile: cfg.Replication.TLS.CAFile, CertFile: cfg.Replication.TLS.CertFile, KeyFile: cfg.Replication.TLS.KeyFile,
		},
		ClusterKey: clusterKey[:], Peers: endpoints,
		LocalHello:           runtime.localHello,
		OnAuthenticatedHello: runtime.authorizePrimaryHello,
		Handle:               runtime.handleRecords(handler.Handle),
		OnDataSessionReady:   runtime.catchUpPrimaryPeer,
		OnDataSessionClosed:  coordinator.ForgetPeer,
		Session:              sessionOptions,
		OnSessionError: func(peer string, sessionErr error) {
			runtime.noteSessionError(sessionErr)
			logger.Warn("replication peer session failed", "peer", peer, "error", sessionErr)
		},
	})
	if err != nil {
		publisher.Close()
		return failSource(err)
	}
	runtime.manager = manager
	sender.Set(manager)
	runtime.startupPeerQuorum = primaryStartupPeerRequirement(len(state.Peers), false)
	_, lastTerm, _ := journal.Head()
	if lastTerm < state.Term {
		commit, err := coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone})
		if err != nil && !errors.Is(err, replication.ErrPeerNotConnected) {
			publisher.Close()
			return failSource(fmt.Errorf("record leadership establishment: %w", err))
		}
		runtime.startupIndex = commit.Index
	} else {
		runtime.startupIndex = state.ConfirmedIndex
		for index := state.ConfirmedIndex + 1; index <= state.DurableIndex; index++ {
			record, recordErr := journal.Record(index)
			if recordErr != nil {
				publisher.Close()
				return failSource(recordErr)
			}
			if record.Term == state.Term && record.Kind == replication.KindLeadershipEstablished {
				runtime.startupIndex = index
				// An offline promotion already obtained a durable remote
				// promise before writing this anchor. One connected Replica
				// therefore supplies the second vote needed to confirm it in
				// either supported topology. A clean Primary restart, and a
				// brand-new term without a recovered anchor, still require all
				// configured peers for startup adjudication (§8.2).
				runtime.startupPeerQuorum = primaryStartupPeerRequirement(len(state.Peers), true)
				break
			}
		}
	}
	return runtime, nil
}

func (r *replicationRuntime) noteSessionError(err error) {
	if err == nil {
		return
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "spki"):
		r.incompatibleSPKI.Add(1)
	case strings.Contains(message, "schema") || strings.Contains(message, "version range") || strings.Contains(message, "incompatible"):
		r.incompatibleSchema.Add(1)
	case strings.Contains(message, "challenge") || strings.Contains(message, "proof") || strings.Contains(message, "cluster key"):
		r.incompatibleKey.Add(1)
	}
}

func filepathMetadataJournal(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.MetadataPath()), boltstore.MetadataJournalFileName)
}

func (r *replicationRuntime) localHello() (replication.Hello, error) {
	state := r.publisher.Snapshot()
	if r.source == nil {
		return replication.Hello{}, errors.New("replication Hello requires actual native source")
	}
	metadataVersion, err := r.source.MetadataFormatVersion()
	if err != nil {
		return replication.Hello{}, fmt.Errorf("replication Hello metadata capability: %w", err)
	}
	hello := replication.Hello{
		ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: state.NodeID,
		Role: state.Role, Term: state.Term, PromisedTerm: state.PromisedTerm,
		DurableIndex: state.DurableIndex, AppliedIndex: state.AppliedIndex,
		Binary:   replication.VersionRange{Current: memberBinaryVersion, Minimum: memberBinaryVersion, Maximum: memberBinaryVersion},
		Protocol: replication.VersionRange{Current: replication.ProtocolVersion, Minimum: replication.ProtocolVersion, Maximum: replication.ProtocolVersion},
		Schema:   replication.VersionRange{Current: uint16(boltstore.CurrentSchemaVersion()), Minimum: uint16(boltstore.CurrentSchemaVersion()), Maximum: uint16(boltstore.CurrentSchemaVersion())},
		Ledger:   replication.VersionRange{Current: ledger.ReplicationFormatVersion(), Minimum: ledger.ReplicationFormatVersion(), Maximum: ledger.ReplicationFormatVersion()},
		Metadata: replication.VersionRange{Current: metadataVersion, Minimum: metadataVersion, Maximum: metadataVersion},
	}
	if _, err := cryptorand.Read(hello.Nonce[:]); err != nil {
		return replication.Hello{}, fmt.Errorf("generate replication Hello nonce: %w", err)
	}
	return hello, nil
}

func (r *replicationRuntime) authorizePrimaryHello(_ context.Context, peer replication.Hello) (bool, error) {
	state := r.publisher.Snapshot()
	// An offline promotion candidate durably promises the proposed term before
	// dialing peers, so its control-only Hello is necessarily ahead of the
	// active term. Admit only the control session here; the signed proposal is
	// what authorizes this member to persist the matching promise.
	if peer.Role == replication.RoleAwaitingDecision {
		return false, nil
	}
	if peer.Term > state.Term || peer.PromisedTerm > state.Term {
		err := errors.New("peer has a higher durable term; Primary must step down before serving data")
		r.coordinator.MarkUnavailable(err)
		if primaryProgressRequiresReseed(state, peer) {
			reseedErr := fmt.Errorf(
				"%w: demoted Primary progress d/c/a=%d/%d/%d is incompatible with higher-term peer d/a=%d/%d",
				replication.ErrMemberRequiresFullReseed,
				state.DurableIndex, state.ConfirmedIndex, state.AppliedIndex,
				peer.DurableIndex, peer.AppliedIndex,
			)
			if markerErr := persistReseedRequiredMarker(filepath.Join(filepath.Dir(r.objectSourceDir), reseedRequiredMarker), reseedErr.Error()); markerErr != nil {
				reseedErr = errors.Join(reseedErr, fmt.Errorf("persist reseed-required marker: %w", markerErr))
			}
			err = errors.Join(err, reseedErr)
		}
		adoptedTerm := max(state.Term, peer.Term)
		adoptedPromise := max(adoptedTerm, peer.PromisedTerm)
		if _, publishErr := r.publisher.AdoptHigherTerm(adoptedTerm, adoptedPromise); publishErr != nil {
			err = errors.Join(err, fmt.Errorf("persist Primary demotion: %w", publishErr))
		}
		r.startupReady.Store(false)
		r.fail(err)
		return false, err
	}
	if peer.Role == replication.RolePrimary && peer.Term == state.Term {
		err := errors.New("multiple Primaries reported for the active term")
		r.coordinator.MarkUnavailable(err)
		r.fail(err)
		return false, err
	}
	if peer.Role != replication.RoleReplica || peer.Term > state.Term || peer.PromisedTerm != state.Term {
		return false, nil
	}
	head, _, _ := r.journal.Head()
	if peer.DurableIndex > head {
		return false, errors.New("Replica advertises progress beyond the Primary authenticated ordering head")
	}
	r.helloMu.Lock()
	r.startupPeers[peer.NodeID] = peer
	ready := len(r.startupPeers) == len(state.Peers)
	r.helloMu.Unlock()
	_ = ready // catch-up starts the confirmation waiter only after the session is registered.
	return true, nil
}

func (r *replicationRuntime) authorizeReplicaHello(_ context.Context, peer replication.Hello) (bool, error) {
	state := r.publisher.Snapshot()
	if peer.Role == replication.RoleAwaitingDecision {
		return false, nil
	}
	if peer.Role == replication.RolePrimary && peer.Term > state.Term {
		if peer.Term != peer.PromisedTerm || peer.Term != state.PromisedTerm {
			return false, errors.New("new Primary term does not match this Replica's durable promise")
		}
		if primaryProgressRequiresReseed(state, peer) {
			reseedErr := fmt.Errorf(
				"%w: demoted member progress d/c/a=%d/%d/%d is incompatible with new Primary d/a=%d/%d",
				replication.ErrMemberRequiresFullReseed,
				state.DurableIndex, state.ConfirmedIndex, state.AppliedIndex,
				peer.DurableIndex, peer.AppliedIndex,
			)
			if markerErr := persistReseedRequiredMarker(filepath.Join(filepath.Dir(r.objectSourceDir), reseedRequiredMarker), reseedErr.Error()); markerErr != nil {
				reseedErr = errors.Join(reseedErr, fmt.Errorf("persist reseed-required marker: %w", markerErr))
			}
			r.startupReady.Store(false)
			r.fail(reseedErr)
			return false, reseedErr
		}
		if err := r.receiver.AdoptTerm(peer.Term, peer.NodeID, func() error {
			_, publishErr := r.publisher.AdoptHigherTerm(peer.Term, peer.PromisedTerm)
			return publishErr
		}); err != nil {
			return false, err
		}
		state = r.publisher.Snapshot()
	} else if peer.Term > state.Term || peer.PromisedTerm > state.PromisedTerm {
		return false, errors.New("peer term is ahead without a matching promised Primary transition")
	}
	if state.PromisedTerm != state.Term || peer.Role != replication.RolePrimary || peer.Term != state.Term || peer.PromisedTerm != state.Term {
		return false, nil
	}
	if err := r.receiver.BindPrimary(peer.NodeID); err != nil {
		return false, err
	}
	return true, nil
}

func (r *replicationRuntime) fail(err error) {
	if err == nil {
		return
	}
	select {
	case r.fatal <- err:
	default:
	}
}

func (r *replicationRuntime) fatalErrors() <-chan error { return r.fatal }

func (r *replicationRuntime) handleRecords(data replication.ConnectionRecordHandler) replication.ConnectionRecordHandler {
	return func(ctx context.Context, peer replication.Hello, record replication.StreamRecord, send func([]byte) error) error {
		if record.Kind == replication.StreamRecordPromotionProposal {
			return r.handlePromotionProposal(peer, record.Encoded, send)
		}
		if record.Kind == replication.StreamRecordPromotionPromise {
			return errors.New("unsolicited promotion promise on a member runtime")
		}
		return data(ctx, peer, record, send)
	}
}

func (r *replicationRuntime) handlePromotionProposal(peer replication.Hello, encoded []byte, send func([]byte) error) error {
	proposal, err := replication.UnmarshalPromotionProposal(encoded)
	if err != nil {
		return err
	}
	state := r.publisher.Snapshot()
	if peer.Role != replication.RoleAwaitingDecision || peer.NodeID != proposal.CandidateNodeID ||
		proposal.ClusterID != state.ClusterID || proposal.Incarnation != state.Incarnation {
		return errors.New("promotion proposal does not match the authenticated candidate")
	}
	if err := validatePromotionCandidateHello(peer, proposal); err != nil {
		return err
	}
	primaryRuntime := r.coordinator != nil
	if primaryRuntime {
		if proposal.PlannedStepdown {
			if err := r.freezeForPlannedStepdown(proposal, state); err != nil {
				return err
			}
		} else {
			r.coordinator.MarkUnavailable(errors.New("durable higher-term promise received"))
		}
		r.startupReady.Store(false)
		state, err = r.publisher.Promise(proposal.Term)
	} else {
		err = r.receiver.Promise(proposal.Term, func() error {
			var publishErr error
			state, publishErr = r.publisher.Promise(proposal.Term)
			return publishErr
		})
	}
	if err != nil {
		if primaryRuntime {
			r.fail(fmt.Errorf("Primary could not persist a higher-term promise after freezing old-term work: %w", err))
		}
		return err
	}
	if primaryRuntime {
		// The durable promise is the irreversible capability transition. Exit
		// even if encoding or delivering the response fails; a lost response is
		// recovered by the candidate's next strictly-higher prepare round.
		defer r.fail(errors.New("Primary durably promised a higher term and stepped down"))
	}
	_, lastFrameTerm, _ := r.journal.Head()
	promise := replication.PromotionPromise{
		Version: replication.PromotionProtocolVersion, ClusterID: state.ClusterID, Incarnation: state.Incarnation,
		NodeID: state.NodeID, Role: state.Role, PreviousRole: func() replication.Role {
			if primaryRuntime {
				return replication.RolePrimary
			}
			return replication.RoleReplica
		}(), Term: state.Term, PromisedTerm: state.PromisedTerm,
		DurableIndex: state.DurableIndex, AppliedIndex: state.AppliedIndex, LastFrameTerm: lastFrameTerm,
	}
	response, err := promise.MarshalBinary()
	if err != nil {
		return err
	}
	if err := send(response); err != nil {
		return err
	}
	return nil
}

func (r *replicationRuntime) freezeForPlannedStepdown(proposal replication.PromotionProposal, state replication.MemberState) error {
	if err := validatePlannedStepdownTarget(proposal, state); err != nil {
		return err
	}
	if r.plannedStepdownDrain == nil {
		return errors.New("planned stepdown drain is unavailable")
	}
	if err := r.plannedStepdownDrain(); err != nil {
		return fmt.Errorf("drain old Primary before planned stepdown: %w", err)
	}
	return r.coordinator.FreezeForStepdown(proposal.ExpectedAppliedIndex)
}

func primaryStartupPeerRequirement(peerCount int, recoveredPromotionAnchor bool) int {
	if recoveredPromotionAnchor && peerCount > 0 {
		return 1
	}
	return peerCount
}

func validatePlannedStepdownTarget(proposal replication.PromotionProposal, state replication.MemberState) error {
	if proposal.OldPrimaryNodeID != state.NodeID {
		return errors.New("planned stepdown proposal does not name this Primary")
	}
	if proposal.ExpectedTerm != state.Term {
		return errors.New("planned stepdown expected term does not match this Primary")
	}
	return nil
}

func validatePromotionCandidateHello(peer replication.Hello, proposal replication.PromotionProposal) error {
	if peer.Term != proposal.ExpectedTerm || peer.PromisedTerm != proposal.Term ||
		peer.DurableIndex != proposal.ExpectedAppliedIndex || peer.AppliedIndex != proposal.ExpectedAppliedIndex {
		return errors.New("promotion proposal term or prefix does not match the authenticated candidate Hello")
	}
	return nil
}

func primaryProgressRequiresReseed(state replication.MemberState, peer replication.Hello) bool {
	return state.DurableIndex != state.ConfirmedIndex || state.ConfirmedIndex != state.AppliedIndex ||
		state.DurableIndex > peer.DurableIndex || state.AppliedIndex > peer.AppliedIndex
}

func persistReseedRequiredMarker(path, reason string) error {
	if path == "" || reason == "" {
		return errors.New("reseed-required marker needs a path and reason")
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".reseed-required-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(reason + "\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return durable.SyncDirectory(directory)
}

func refuseReseedRequiredMember(cfg config.Config) error {
	path := filepath.Join(cfg.ClusterDirectoryPath(), reseedRequiredMarker)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect reseed-required marker: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: reseed-required marker is not a private regular file", replication.ErrMemberRequiresFullReseed)
	}
	return fmt.Errorf("%w: durable marker %s is present", replication.ErrMemberRequiresFullReseed, path)
}

func (r *Runtime) requireMemberStartup(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if r.replication == nil || r.replication.role != replication.RolePrimary || r.replication.startupReady.Load() ||
			request.URL.Path == "/health/live" || request.URL.Path == "/health/ready" || request.URL.Path == "/admin/api/v1/cluster/status" {
			next.ServeHTTP(writer, request)
			return
		}
		writer.Header().Set("Retry-After", "1")
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"error": "Primary is waiting for startup peer adjudication", "code": "replication_unavailable",
		})
	})
}

func (r *replicationRuntime) catchUpPrimaryPeer(peer string, hello replication.Hello, send func([]byte) error) error {
	if err := r.coordinator.PreparePeer(peer, hello.DurableIndex, hello.AppliedIndex, send); err != nil {
		return fmt.Errorf("prepare Replica %s send window: %w", peer, err)
	}
	r.helloMu.Lock()
	requiredPeers := r.startupPeerQuorum
	if requiredPeers == 0 {
		requiredPeers = len(r.publisher.Snapshot().Peers)
	}
	allPeers := len(r.startupPeers) >= requiredPeers
	r.helloMu.Unlock()
	if allPeers {
		// Include the complete accepted startup prefix, not only its anchor.
		head, _, _ := r.journal.Head()
		r.startPrimaryCompletion(max(r.startupIndex, head))
	}
	return nil
}

func (r *replicationRuntime) ledgerHooks(options *ledger.Options) {
	options.RequireExisting = true
	options.BeforeDurable = func(batch ledger.DurableBatch) (ledger.AppendAdmission, error) {
		if q := r.peerPhysical.Load(); q != nil {
			if err := q.beforePrimarySource(batch.Context, 0, batch.FirstSequence, batch.LastSequence, len(batch.Frames)); err != nil {
				return nil, err
			}
		}
		return r.coordinator.ReserveNativeSourceUnconfirmedContext(batch.Context, len(batch.Frames))
	}
	options.AfterDurable = func(batch ledger.DurableBatch) (uint64, error) {
		commit, err := r.coordinator.RecordLedgerBatch(batch)
		return commit.Index, err
	}
	options.WaitConfirmed = r.coordinator.WaitConfirmed
	options.BeforeRoll = func(generation, lastSequence uint64) error {
		if r.peerPhysical.Load() != nil {
			return replication.ErrPhysicalCredit
		}
		return r.coordinator.RequireLedgerConfirmed(generation, lastSequence)
	}
	options.AfterRoll = func(segment ledger.Segment) error {
		_, err := r.coordinator.RecordLedgerRoll(segment)
		return err
	}
}

func (r *replicationRuntime) auditOptions() audit.Options {
	return audit.Options{RequireExisting: true, BeforeDurable: func(batch audit.DurableBatch) (audit.AppendAdmission, error) {
		if q := r.peerPhysical.Load(); q != nil {
			if err := q.beforePrimarySource(batch.Context, 1, batch.FirstSequence, batch.LastSequence, len(batch.Frames)); err != nil {
				return nil, err
			}
		}
		return r.coordinator.ReserveNativeSourceUnconfirmedContext(batch.Context, len(batch.Frames))
	}, AfterDurable: func(batch audit.DurableBatch) error {
		_, err := r.coordinator.RecordAuditBatch(batch)
		return err
	}}
}

func (r *replicationRuntime) governanceOptions() governance.Options {
	return governance.Options{RequireExisting: true, BeforeDurable: func(batch governance.DurableBatch) (governance.AppendAdmission, error) {
		if q := r.peerPhysical.Load(); q != nil {
			if err := q.beforePrimarySource(batch.Context, 2, batch.FirstSequence, batch.LastSequence, len(batch.Frames)); err != nil {
				return nil, err
			}
		}
		return r.coordinator.ReserveNativeSourceUnconfirmedContext(batch.Context, len(batch.Frames))
	}, AfterDurable: func(batch governance.DurableBatch) error {
		_, err := r.coordinator.RecordGovernanceBatch(batch)
		return err
	}}
}

func (r *replicationRuntime) installMetadataHook(store *boltstore.Store) error {
	return store.SetMetadataJournalAdmission(func(batch metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) {
		return r.coordinator.ReserveNativeSourceUnconfirmedContext(batch.Context, len(batch.Frames))
	}, func(batch metadatajournal.DurableBatch) (uint64, error) {
		commit, err := r.coordinator.RecordMetadataBatch(batch)
		return commit.Index, err
	}, r.coordinator.WaitConfirmed)
}

func (r *replicationRuntime) replicateProviderObject(ctx context.Context, name string, sealed []byte) error {
	if r == nil || r.role != replication.RolePrimary || r.coordinator == nil {
		return errors.New("provider-object replication requires the active Primary")
	}
	admissions, err := r.coordinator.ReserveProviderObjectAdmissions(name, sealed)
	if err != nil {
		return fmt.Errorf("admit provider-object replication before source publication: %w", err)
	}
	if err := replication.PersistProviderObjectSource(r.objectSourceDir, name, sealed); err != nil {
		r.coordinator.RetainUnconfirmedAdmissions(admissions)
		return fmt.Errorf("persist provider-object replication source: %w", err)
	}
	index, err := r.coordinator.RecordProviderObjectWithAdmissions(name, sealed, admissions)
	if err != nil {
		return err
	}
	return r.coordinator.WaitConfirmed(ctx, index)
}

func providerObjectSourceDir(cfg config.Config) string {
	return filepath.Join(cfg.ClusterDirectoryPath(), "provider-object-sources")
}

func (r *replicationRuntime) serve(ctx context.Context, listener net.Listener) error {
	return r.manager.Serve(ctx, listener)
}

func (r *replicationRuntime) close() error {
	if r == nil {
		return nil
	}
	r.stopPrimaryCompletion()
	if r.coordinator != nil {
		r.coordinator.Stop()
	}
	var err error
	if r.manager != nil {
		err = r.manager.Close()
	}
	if r.publisher != nil {
		r.publisher.Close()
		err = errors.Join(err, r.publisher.PhysicalCloseError())
	}
	clear(r.clusterKey[:])
	if r.source != nil {
		err = errors.Join(err, r.source.Close())
	}
	if r.journal != nil {
		err = errors.Join(err, r.journal.Close())
	}
	if err == nil && r.coordinator != nil {
		err = errors.Join(err, r.coordinator.ReleaseUnconfirmedForShutdown())
	}
	return err
}

func (r *Runtime) replicaGatewayRouter() http.Handler {
	router := chi.NewRouter()
	router.Use(r.recoverPanics)
	router.Use(r.requireMemberStartup)
	router.Get("/health/live", r.live)
	router.Get("/health/ready", r.ready)
	router.Get("/", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"name": "halro", "version": buildinfo.Current(), "role": "replica"})
	})
	router.Handle("/*", http.HandlerFunc(r.writeNotPrimary))
	return router
}

func (r *Runtime) replicaAdminRouter() http.Handler {
	router := chi.NewRouter()
	router.Use(r.recoverPanics)
	router.Use(adminSecurityHeaders)
	router.Use(r.requireMemberStartup)
	router.Get("/health/live", r.live)
	router.Get("/health/ready", r.ready)
	router.Post("/admin/api/v1/session/login", r.loginReplicaAdmin)
	router.With(r.requireAdmin).Get("/admin/api/v1/session", r.getAdminSession)
	router.With(r.requireAdminSelfMutation).Post("/admin/api/v1/session/logout", r.logoutReplicaAdmin)
	router.With(r.requireAdmin).Get("/admin/api/v1/cluster/status", r.adminClusterStatus)
	router.Handle("/*", http.HandlerFunc(r.writeNotPrimary))
	return router
}

func (r *Runtime) writeNotPrimary(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Retry-After", "1")
	writeJSON(writer, http.StatusServiceUnavailable, map[string]string{
		"error": "this Halro member is not the Primary", "code": "not_primary",
	})
}

func (r *Runtime) adminClusterStatus(writer http.ResponseWriter, _ *http.Request) {
	if r.replication == nil {
		writeJSON(writer, http.StatusOK, map[string]string{"mode": "standalone"})
		return
	}
	state := r.replication.publisher.Snapshot()
	connections := map[string]bool{}
	if r.replication.manager != nil {
		connections = r.replication.manager.PeerConnections()
	}
	peers := make([]map[string]any, 0, len(state.Peers))
	for _, peer := range state.Peers {
		peers = append(peers, map[string]any{"node_id": peer.Name, "connected": connections[peer.Name]})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"mode": "ha", "startup_ready": r.replication.startupReady.Load(),
		"cluster_id": state.ClusterID, "incarnation": state.Incarnation, "node_id": state.NodeID,
		"role": state.Role, "term": state.Term, "promised_term": state.PromisedTerm,
		"durable_index": state.DurableIndex, "confirmed_index": state.ConfirmedIndex,
		"applied_index": state.AppliedIndex, "projection": state.Projection, "peers": peers,
	})
}

func (r *Runtime) replicaMetricsRouter() http.Handler {
	router := chi.NewRouter()
	router.Use(r.recoverPanics)
	router.Get("/health/live", r.live)
	router.Get("/metrics", func(writer http.ResponseWriter, request *http.Request) {
		if r.config.Metrics.RequireAuth && !r.authorizeMetrics(request) {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="halro-metrics"`)
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "metrics authentication required"})
			return
		}
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
		output := bufio.NewWriter(writer)
		r.writeResourcePressureMetrics(output)
		r.writeReplicationMetrics(output)
		_ = output.Flush()
	})
	return router
}

func (r *Runtime) writeReplicationMetrics(output *bufio.Writer) {
	if r.replication == nil {
		return
	}
	state := r.replication.publisher.Snapshot()
	metricHeader(output, "halro_cluster_role", "gauge", "Authenticated role held by this member.")
	fmt.Fprintf(output, "halro_cluster_role{role=%s} 1\n", strconv.Quote(string(state.Role)))
	metricHeader(output, "halro_cluster_term", "gauge", "Current durable replication term.")
	fmt.Fprintf(output, "halro_cluster_term %d\n", state.Term)
	metricHeader(output, "halro_cluster_promised_term", "gauge", "Highest durably promised replication term.")
	fmt.Fprintf(output, "halro_cluster_promised_term %d\n", state.PromisedTerm)
	metricHeader(output, "halro_cluster_incarnation_info", "gauge", "Static identity of the active cluster incarnation.")
	fmt.Fprintf(output, "halro_cluster_incarnation_info{incarnation=%s} 1\n", strconv.Quote(state.Incarnation))
	metricHeader(output, "halro_replication_index", "gauge", "Global replication index by durability stage.")
	fmt.Fprintf(output, "halro_replication_index{kind=\"durable\"} %d\n", state.DurableIndex)
	fmt.Fprintf(output, "halro_replication_index{kind=\"confirmed\"} %d\n", state.ConfirmedIndex)
	fmt.Fprintf(output, "halro_replication_index{kind=\"applied\"} %d\n", state.AppliedIndex)
	metricHeader(output, "halro_replication_durable_index", "gauge", "Highest locally durable global replication index.")
	fmt.Fprintf(output, "halro_replication_durable_index %d\n", state.DurableIndex)
	metricHeader(output, "halro_replication_confirmed_index", "gauge", "Highest quorum-confirmed global replication index.")
	fmt.Fprintf(output, "halro_replication_confirmed_index %d\n", state.ConfirmedIndex)
	metricHeader(output, "halro_replication_applied_index", "gauge", "Highest global replication index applied to local projections.")
	fmt.Fprintf(output, "halro_replication_applied_index %d\n", state.AppliedIndex)
	metricHeader(output, "halro_replication_confirmation_lag", "gauge", "Locally durable indexes not yet quorum-confirmed.")
	fmt.Fprintf(output, "halro_replication_confirmation_lag %d\n", state.DurableIndex-state.ConfirmedIndex)
	metricHeader(output, "halro_replication_apply_lag", "gauge", "Confirmed indexes not yet applied locally.")
	fmt.Fprintf(output, "halro_replication_apply_lag %d\n", state.ConfirmedIndex-state.AppliedIndex)
	metricHeader(output, "halro_replication_apply_backlog_frames", "gauge", "Durable frames not yet applied locally.")
	fmt.Fprintf(output, "halro_replication_apply_backlog_frames %d\n", state.DurableIndex-state.AppliedIndex)
	metricHeader(output, "halro_replication_startup_ready", "gauge", "Whether startup role adjudication and leadership confirmation completed.")
	fmt.Fprintf(output, "halro_replication_startup_ready %d\n", boolMetric(r.replication.startupReady.Load()))
	unavailable := false
	if r.replication.coordinator != nil {
		_, available, _ := r.replication.coordinator.Status()
		unavailable = !available
	}
	metricHeader(output, "halro_replication_unavailable", "gauge", "Whether this member cannot currently confirm replicated writes.")
	fmt.Fprintf(output, "halro_replication_unavailable %d\n", boolMetric(unavailable))
	metricHeader(output, "halro_replication_state", "gauge", "Current replication availability state.")
	fmt.Fprintf(output, "halro_replication_state{state=\"replicating\"} %d\n", boolMetric(!unavailable))
	fmt.Fprintf(output, "halro_replication_state{state=\"unavailable\"} %d\n", boolMetric(unavailable))
	if budgetConfig := r.config.ResourceBudget; budgetConfig != nil && budgetConfig.TotalUnconfirmedBytes > 0 && r.replication.coordinator != nil && r.resourcePressure != nil {
		if manager := r.resourcePressure.logicalBudget(); manager != nil {
			writePrimaryUnconfirmedMetrics(output, *budgetConfig, manager.Snapshot(), r.replication.coordinator.UnconfirmedAdmissionStatus())
		}
	}
	metricHeader(output, "halro_replication_peer_connected", "gauge", "Whether the authenticated data session for a configured peer is connected.")
	connections := r.replication.manager.PeerConnections()
	for _, peer := range state.Peers {
		fmt.Fprintf(output, "halro_replication_peer_connected{peer=%s} %d\n", strconv.Quote(peer.Name), boolMetric(connections[peer.Name]))
	}
	metricHeader(output, "halro_cluster_maintenance", "gauge", "Whether this process is serving only maintenance liveness.")
	fmt.Fprintln(output, "halro_cluster_maintenance 0")
	metricHeader(output, "halro_replication_member_incompatible", "gauge", "Whether this process observed an incompatible authenticated member since start.")
	fmt.Fprintf(output, "halro_replication_member_incompatible{reason=\"schema\"} %d\n", boolMetric(r.replication.incompatibleSchema.Load() > 0))
	fmt.Fprintf(output, "halro_replication_member_incompatible{reason=\"key_challenge\"} %d\n", boolMetric(r.replication.incompatibleKey.Load() > 0))
	fmt.Fprintf(output, "halro_replication_member_incompatible{reason=\"spki\"} %d\n", boolMetric(r.replication.incompatibleSPKI.Load() > 0))
}

func writePrimaryUnconfirmedMetrics(output *bufio.Writer, budgetConfig config.ResourceBudget, budgetStats resourcebudget.Stats, admission replication.UnconfirmedAdmissionStatus) {
	metricHeader(output, "halro_replication_unconfirmed_bytes", "gauge", "Configured and currently charged Primary unconfirmed-byte capacity.")
	fmt.Fprintf(output, "halro_replication_unconfirmed_bytes{kind=\"configured_total\"} %d\n", budgetConfig.TotalUnconfirmedBytes)
	fmt.Fprintf(output, "halro_replication_unconfirmed_bytes{kind=\"configured_io\"} %d\n", budgetConfig.IOUnconfirmedBytes)
	fmt.Fprintf(output, "halro_replication_unconfirmed_bytes{kind=\"configured_recovery_class\"} %d\n", budgetConfig.RecoveryUnconfirmedBytes)
	fmt.Fprintf(output, "halro_replication_unconfirmed_bytes{kind=\"configured_recovery_reserve\"} %d\n", budgetConfig.RecoveryReserveUnconfirmedBytes)
	fmt.Fprintf(output, "halro_replication_unconfirmed_bytes{kind=\"used_total\"} %d\n", budgetStats.Used.Unconfirmed)
	fmt.Fprintf(output, "halro_replication_unconfirmed_bytes{kind=\"used_io\"} %d\n", budgetStats.Classes[resourcebudget.IO].Unconfirmed)
	fmt.Fprintf(output, "halro_replication_unconfirmed_bytes{kind=\"used_recovery\"} %d\n", budgetStats.Classes[resourcebudget.Recovery].Unconfirmed)
	metricHeader(output, "halro_replication_unconfirmed_entries", "gauge", "Current bounded Primary unconfirmed entries and configured append limit.")
	fmt.Fprintf(output, "halro_replication_unconfirmed_entries{kind=\"used\"} %d\n", admission.Entries)
	fmt.Fprintf(output, "halro_replication_unconfirmed_entries{kind=\"configured_limit\"} %d\n", budgetConfig.UnconfirmedMaxEntries)
	metricHeader(output, "halro_replication_unconfirmed_recovery_debt_bytes", "gauge", "Recovered suffix bytes not currently charged while over the configured Recovery reserve.")
	fmt.Fprintf(output, "halro_replication_unconfirmed_recovery_debt_bytes %d\n", max(int64(0), admission.RecoveryBytes-admission.RecoveryCharged))
	metricHeader(output, "halro_replication_unconfirmed_recovery_over_limit", "gauge", "Whether a recovered suffix currently blocks new Primary appends.")
	fmt.Fprintf(output, "halro_replication_unconfirmed_recovery_over_limit %d\n", boolMetric(admission.OverLimit))
	metricHeader(output, "halro_replication_unconfirmed_admission_refusals_total", "counter", "Primary append admissions refused by the configured unconfirmed entry or byte limits.")
	fmt.Fprintf(output, "halro_replication_unconfirmed_admission_refusals_total %d\n", admission.AdmissionRefusals)
	metricHeader(output, "halro_replication_unconfirmed_last_progress_timestamp_seconds", "gauge", "Unix timestamp of the latest Primary confirmation-state publication in this process (startup recovery or ACK).")
	if !admission.LastProgress.IsZero() {
		fmt.Fprintf(output, "halro_replication_unconfirmed_last_progress_timestamp_seconds %.3f\n", float64(admission.LastProgress.UnixNano())/1e9)
	}
}

func replicationSessionOptions(cfg config.Config, budget *resourcebudget.Manager) (replication.PeerSessionOptions, error) {
	options := replication.PeerSessionOptions{}
	if budget == nil {
		return options, nil
	}
	if cfg.ResourceBudget == nil {
		return options, errors.New("peer queue budget requires resource_budget")
	}
	if err := cfg.ResourceBudget.ValidateReplicationCandidates(); err != nil {
		return options, err
	}
	queue, err := replication.NewPeerSessionBudget(budget, cfg.ResourceBudget.ReplicationQueueRAMBytes, cfg.ResourceBudget.ReplicationReadRAMBytes, 3)
	if err != nil {
		return options, err
	}
	options.QueueBudget = queue
	return options, nil
}

// Called after HA budget config validation. Unbudgeted/offline paths retain zero.
func replicationReaderRAM(cfg config.Config, budget *resourcebudget.Manager) int64 {
	if budget == nil {
		return 0
	}
	return cfg.ResourceBudget.ReplicationReaderRAMBytes
}
