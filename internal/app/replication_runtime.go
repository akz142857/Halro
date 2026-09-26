package app

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	storelock "github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
	"github.com/go-chi/chi/v5"
)

const memberBinaryVersion uint16 = 1

// replicationRuntime is the one HA subsystem owned by Runtime. Its internals
// deliberately stay out of Runtime's already-wide field list: role state,
// ordering, native-source authentication and peer transport have one lifetime.
type replicationRuntime struct {
	role               replication.Role
	clusterKey         [32]byte
	publisher          *replication.StatePublisher
	journal            *replication.OrderingJournal
	source             *replication.NativeSource
	manager            *replication.ConnectionManager
	coordinator        *replication.PrimaryCoordinator
	receiver           *replication.ReplicaReceiver
	applier            *replication.ReplicaApplier
	startupReady       atomic.Bool
	helloMu            sync.Mutex
	startupPeers       map[string]replication.Hello
	startupIndex       uint64
	startupOnce        sync.Once
	startupCtx         context.Context
	startupStop        context.CancelFunc
	fatal              chan error
	objectSourceDir    string
	incompatibleSchema atomic.Uint64
	incompatibleKey    atomic.Uint64
	incompatibleSPKI   atomic.Uint64
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
) (*Runtime, error) {
	metadata, err := boltstore.OpenReplica(cfg.MetadataPath())
	if err != nil {
		return nil, err
	}
	failMetadata := func(openErr error) (*Runtime, error) {
		return nil, errors.Join(openErr, metadata.Close())
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
		cfg, state, clusterKey, ledgerKey, auditKey, governanceKey, metadataKey,
	)
	if err != nil {
		return failMetadata(err)
	}
	failReplication := func(openErr error) (*Runtime, error) {
		return failMetadata(errors.Join(openErr, replicationRuntime.close()))
	}
	metadataLog, err := metadatajournal.OpenReplica(filepathMetadataJournal(cfg), metadataKey)
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
	ledgerLog, err := ledger.OpenWithOptions(cfg.LedgerPath(), accountingStatus, ledger.Options{ChainKey: ledgerKey, Replica: true})
	if err != nil {
		return failReplication(fmt.Errorf("open Replica Ledger: %w", err))
	}
	failLedger := func(openErr error) (*Runtime, error) {
		return failReplication(errors.Join(openErr, ledgerLog.Close()))
	}
	ledgerState := ledger.NewState()
	appliedCursors, err := replicationRuntime.journal.StoreCursorsThrough(state.AppliedIndex)
	if err != nil {
		return failLedger(err)
	}
	ledgerApplied := appliedCursors[replication.StoreLedger-replication.StoreLedger]
	if _, err := ledgerLog.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Sequence > ledgerApplied.Sequence {
			return nil
		}
		return ledgerState.Apply(record)
	}); err != nil {
		return failLedger(fmt.Errorf("replay Replica applied Ledger prefix: %w", err))
	}
	if ledgerState.Watermark().Sequence != ledgerApplied.Sequence {
		return failLedger(errors.New("Replica Ledger projection does not reach authenticated applied prefix"))
	}

	auditLog, err := audit.OpenWithOptions(cfg.AuditPath(), auditKey, audit.Options{Replica: true})
	if err != nil {
		return failLedger(fmt.Errorf("open Replica Audit log: %w", err))
	}
	failAudit := func(openErr error) (*Runtime, error) {
		return failLedger(errors.Join(openErr, auditLog.Close()))
	}
	governanceLog, err := governance.OpenWithOptions(cfg.GovernancePath(), governanceKey, governance.Options{Replica: true})
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
	projection, err := replication.NewNativeProjection(ledgerLog, ledgerState, metadata)
	if err != nil {
		return failGovernance(err)
	}
	applier, err := replication.NewReplicaApplier(receiver, replicationRuntime.journal, projection)
	if err != nil {
		return failGovernance(err)
	}
	replicationRuntime.receiver = receiver
	replicationRuntime.applier = applier
	if err := replicationRuntime.completeReplicaTransport(cfg, logger); err != nil {
		return failGovernance(err)
	}
	if _, err := applier.ApplyConfirmed(ctx); err != nil {
		return failGovernance(fmt.Errorf("apply Replica confirmed prefix at startup: %w", err))
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
		ledger: ledgerLog, state: ledgerState, status: accountingStatus,
		governance: governanceRuntime{log: governanceLog}, vault: secretVault, audit: auditLog,
		adminSessions: adminSessions, startedAt: time.Now(), now: time.Now,
		backgroundCtx: backgroundContext, backgroundCancel: backgroundCancel,
		metricsScrapes:   make(chan struct{}, max(1, cfg.Metrics.MaxConcurrentScrapes)),
		metricsTokenHash: metricsTokenHash, metricsAuthorizer: metricsAuthorizer,
		replication: replicationRuntime,
	}
	return runtime, nil
}

func openReplicaReplicationFoundation(
	cfg config.Config,
	state replication.MemberState,
	clusterKey [32]byte,
	ledgerKey, auditKey, governanceKey, metadataKey []byte,
) (*replicationRuntime, error) {
	if state.Role != replication.RoleReplica {
		return nil, fmt.Errorf("member role %q is not a Replica", state.Role)
	}
	journal, err := replication.OpenExistingOrderingJournal(
		cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation,
		state.DurableIndex, state.OrderingHeadMAC,
	)
	if err != nil {
		return nil, err
	}
	source, err := replication.NewNativeSource(replication.NativeSourceOptions{
		LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(),
		MetadataPath: filepathMetadataJournal(cfg), LedgerKey: ledgerKey, AuditKey: auditKey,
		GovernanceKey: governanceKey, MetadataKey: metadataKey,
		ProviderObjectDir: providerObjectSourceDir(cfg),
	})
	if err != nil {
		_ = journal.Close()
		return nil, err
	}
	if err := replication.RecoverReplicaStores(journal, source); err != nil {
		source.Close()
		_ = journal.Close()
		return nil, err
	}
	publisher, err := replication.NewStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], state)
	if err != nil {
		source.Close()
		_ = journal.Close()
		return nil, err
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

func (r *replicationRuntime) completeReplicaTransport(cfg config.Config, logger *slog.Logger) error {
	handler, err := replication.NewReplicaRecordHandler(r.receiver, r.applier)
	if err != nil {
		return err
	}
	state := r.publisher.Snapshot()
	endpoints := make([]replication.PeerEndpoint, 0, len(state.Peers))
	for _, peer := range state.Peers {
		endpoints = append(endpoints, replication.PeerEndpoint{NodeID: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	manager, err := replication.NewConnectionManager(replication.ConnectionManagerOptions{
		LocalNode: state.NodeID,
		TLS: replication.TLSFiles{
			CAFile: cfg.Replication.TLS.CAFile, CertFile: cfg.Replication.TLS.CertFile, KeyFile: cfg.Replication.TLS.KeyFile,
		},
		ClusterKey: r.clusterKey[:],
		Peers:      endpoints, LocalHello: r.localHello, OnAuthenticatedHello: r.authorizeReplicaHello,
		Handle: r.handleRecords(handler.Handle),
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
	state, err := replication.ReadStateWithMasterKey(cfg.ReplicationStatePath(), masterKey)
	if err != nil {
		return replication.MemberState{}, [32]byte{}, err
	}
	peers := make([]replication.StatePeer, 0, len(cfg.Replication.Peers))
	for _, peer := range cfg.Replication.Peers {
		peers = append(peers, replication.StatePeer{Name: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	if err := replication.ValidateMemberConfiguration(state, cfg.Replication.ClusterID, cfg.Replication.NodeID, peers); err != nil {
		return replication.MemberState{}, [32]byte{}, err
	}
	key, err := replication.DeriveClusterKey(masterKey, state.Incarnation)
	if err != nil {
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
) (*replicationRuntime, error) {
	if state.Role != replication.RolePrimary {
		return nil, fmt.Errorf("member role %q is not a Primary", state.Role)
	}
	journal, err := replication.OpenExistingOrderingJournal(
		cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation,
		state.DurableIndex, state.OrderingHeadMAC,
	)
	if err != nil {
		return nil, err
	}
	failJournal := func(openErr error) (*replicationRuntime, error) {
		return nil, errors.Join(openErr, journal.Close())
	}
	source, err := replication.NewNativeSource(replication.NativeSourceOptions{
		LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(),
		MetadataPath: filepathMetadataJournal(cfg), LedgerKey: ledgerKey, AuditKey: auditKey,
		GovernanceKey: governanceKey, MetadataKey: metadataKey,
		ProviderObjectDir: providerObjectSourceDir(cfg),
	})
	if err != nil {
		return failJournal(err)
	}
	failSource := func(openErr error) (*replicationRuntime, error) {
		source.Close()
		return failJournal(openErr)
	}
	recovered, err := replication.RecoverLocalCommits(state.ClusterID, state.Incarnation, state.ConfirmedIndex, journal, source)
	if err != nil {
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
	coordinator, err := replication.NewPrimaryCoordinator(
		state.ClusterID, state.Incarnation, state.NodeID, state.Term, state.ConfirmedIndex,
		peerNames, journal, outbound, publisher.PublishPrimary, recovered...,
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
	hello := replication.Hello{
		ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: state.NodeID,
		Role: state.Role, Term: state.Term, PromisedTerm: state.PromisedTerm,
		DurableIndex: state.DurableIndex, AppliedIndex: state.AppliedIndex,
		Binary:   replication.VersionRange{Current: memberBinaryVersion, Minimum: memberBinaryVersion, Maximum: memberBinaryVersion},
		Protocol: replication.VersionRange{Current: replication.ProtocolVersion, Minimum: replication.ProtocolVersion, Maximum: replication.ProtocolVersion},
		Schema:   replication.VersionRange{Current: uint16(boltstore.CurrentSchemaVersion()), Minimum: uint16(boltstore.CurrentSchemaVersion()), Maximum: uint16(boltstore.CurrentSchemaVersion())},
		Ledger:   replication.VersionRange{Current: ledger.ReplicationFormatVersion(), Minimum: ledger.ReplicationFormatVersion(), Maximum: ledger.ReplicationFormatVersion()},
		Metadata: replication.VersionRange{Current: metadatajournal.ReplicationFormatVersion(), Minimum: metadatajournal.ReplicationFormatVersion(), Maximum: metadatajournal.ReplicationFormatVersion()},
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
	if peer.DurableIndex > head || peer.AppliedIndex > state.ConfirmedIndex {
		return false, errors.New("Replica advertises progress beyond the Primary authenticated prefix")
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
	if peer.Role != replication.RolePrimary || peer.Term != state.Term || peer.PromisedTerm != state.Term {
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
	previousRole := state.Role
	if previousRole == replication.RolePrimary {
		r.coordinator.MarkUnavailable(errors.New("durable higher-term promise received"))
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
		return err
	}
	_, lastFrameTerm, _ := r.journal.Head()
	promise := replication.PromotionPromise{
		Version: replication.PromotionProtocolVersion, ClusterID: state.ClusterID, Incarnation: state.Incarnation,
		NodeID: state.NodeID, Role: state.Role, Term: state.Term, PromisedTerm: state.PromisedTerm,
		DurableIndex: state.DurableIndex, AppliedIndex: state.AppliedIndex, LastFrameTerm: lastFrameTerm,
	}
	response, err := promise.MarshalBinary()
	if err != nil {
		return err
	}
	if err := send(response); err != nil {
		return err
	}
	if previousRole == replication.RolePrimary {
		r.fail(errors.New("Primary durably promised a higher term and stepped down"))
	}
	return nil
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

func (r *replicationRuntime) catchUpPrimaryPeer(peer string, hello replication.Hello) error {
	head, _, _ := r.journal.Head()
	if hello.DurableIndex > head {
		return errors.New("Replica durable index exceeds Primary ordering head")
	}
	first := hello.DurableIndex
	if first == 0 {
		first = 1
	}
	if err := replication.ReconstructRangeEach(
		hello.ClusterID, hello.Incarnation, first, head, r.journal, r.source,
		func(commit replication.LocalCommit) error { return r.manager.Send(peer, commit.Encoded) },
	); err != nil {
		return fmt.Errorf("catch up Replica %s: %w", peer, err)
	}
	if notice := r.coordinator.PendingCommitNotice(); len(notice) > 0 {
		if err := r.manager.Send(peer, notice); err != nil {
			return fmt.Errorf("send current commit notice to Replica %s: %w", peer, err)
		}
	}
	if err := r.coordinator.RetryPending(); err != nil {
		return fmt.Errorf("retry pending replication after %s catch-up: %w", peer, err)
	}
	r.helloMu.Lock()
	allPeers := len(r.startupPeers) == len(r.publisher.Snapshot().Peers)
	r.helloMu.Unlock()
	if allPeers {
		r.startupOnce.Do(func() {
			go func() {
				if err := r.coordinator.WaitConfirmed(r.startupCtx, r.startupIndex); err != nil {
					if !errors.Is(err, context.Canceled) {
						r.fail(fmt.Errorf("confirm leadership establishment: %w", err))
					}
					return
				}
				r.startupReady.Store(true)
			}()
		})
	}
	return nil
}

func (r *replicationRuntime) ledgerHooks(options *ledger.Options) {
	options.RequireExisting = true
	options.AfterDurable = func(batch ledger.DurableBatch) (uint64, error) {
		commit, err := r.coordinator.RecordLedgerBatch(batch)
		return commit.Index, err
	}
	options.WaitConfirmed = r.coordinator.WaitConfirmed
	options.BeforeRoll = r.coordinator.RequireLedgerConfirmed
	options.AfterRoll = func(segment ledger.Segment) error {
		_, err := r.coordinator.RecordLedgerRoll(segment)
		return err
	}
}

func (r *replicationRuntime) auditOptions() audit.Options {
	return audit.Options{RequireExisting: true, AfterDurable: func(batch audit.DurableBatch) error {
		_, err := r.coordinator.RecordAuditBatch(batch)
		return err
	}}
}

func (r *replicationRuntime) governanceOptions() governance.Options {
	return governance.Options{RequireExisting: true, AfterDurable: func(batch governance.DurableBatch) error {
		_, err := r.coordinator.RecordGovernanceBatch(batch)
		return err
	}}
}

func (r *replicationRuntime) installMetadataHook(store *boltstore.Store) error {
	return store.SetMetadataJournalAfterDurable(func(batch metadatajournal.DurableBatch) (uint64, error) {
		commit, err := r.coordinator.RecordMetadataBatch(batch)
		return commit.Index, err
	}, r.coordinator.WaitConfirmed)
}

func (r *replicationRuntime) replicateProviderObject(ctx context.Context, name string, sealed []byte) error {
	if r == nil || r.role != replication.RolePrimary || r.coordinator == nil {
		return errors.New("provider-object replication requires the active Primary")
	}
	if err := replication.PersistProviderObjectSource(r.objectSourceDir, name, sealed); err != nil {
		return fmt.Errorf("persist provider-object replication source: %w", err)
	}
	index, err := r.coordinator.RecordProviderObject(name, sealed)
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
	if r.publisher != nil {
		r.publisher.Close()
	}
	if r.startupStop != nil {
		r.startupStop()
	}
	clear(r.clusterKey[:])
	if r.source != nil {
		r.source.Close()
	}
	if r.journal != nil {
		return r.journal.Close()
	}
	return nil
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
	state := r.replication.publisher.Snapshot()
	writeJSON(writer, http.StatusOK, map[string]any{
		"cluster_id": state.ClusterID, "incarnation": state.Incarnation, "node_id": state.NodeID,
		"role": state.Role, "term": state.Term, "promised_term": state.PromisedTerm,
		"durable_index": state.DurableIndex, "confirmed_index": state.ConfirmedIndex,
		"applied_index": state.AppliedIndex, "projection": state.Projection,
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
