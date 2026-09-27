package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type PromoteMemberOptions struct {
	ExpectedTerm         uint64
	ExpectedAppliedIndex uint64
	OldPrimaryNodeID     string
	FencedBy             string
	NoPeerPromise        bool
	Self                 bool
	Username             string
	Password             []byte
	TOTPCode             string
	PlannedStepdown      bool
}

type PromoteMemberResult struct {
	State    replication.MemberState
	Promises []replication.PromotionPromise
}

// EstablishMemberState turns an already-identical, offline native-store
// snapshot into one cluster member. Seed transport is responsible for copying
// and digest-verifying that snapshot first; this function atomically publishes
// only the node-local authenticated cluster state and never edits native data.
func EstablishMemberState(ctx context.Context, cfg config.Config, role replication.Role, incarnation string, term uint64) error {
	if cfg.Replication == nil {
		return errors.New("replication configuration is required to establish member state")
	}
	if role != replication.RolePrimary && role != replication.RoleReplica {
		return errors.New("initial member role must be primary or replica")
	}
	if term == 0 {
		return errors.New("initial member term must be positive")
	}
	clusterPath := cfg.ClusterDirectoryPath()
	if _, err := os.Lstat(clusterPath); err == nil {
		return errors.New("cluster member state already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()
	masterKey, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		return err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	metadata, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer metadata.Close()
	if err := verifyVaultKeyCheck(metadata, secretVault); err != nil {
		return err
	}
	ledgerKey, err := loadLedgerHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return err
	}
	defer clear(ledgerKey)
	auditKey, err := loadAuditHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return err
	}
	defer clear(auditKey)
	governanceKey, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		return err
	}
	defer clear(governanceKey)
	metadataKey, err := loadMetadataJournalHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return err
	}
	defer clear(metadataKey)
	source, err := replication.NewNativeSource(replication.NativeSourceOptions{
		LedgerPath: cfg.LedgerPath(), AuditPath: cfg.AuditPath(), GovernancePath: cfg.GovernancePath(),
		MetadataPath: metadata.MetadataJournalPath(), LedgerKey: ledgerKey, AuditKey: auditKey,
		GovernanceKey: governanceKey, MetadataKey: metadataKey,
	})
	if err != nil {
		return err
	}
	defer source.Close()
	header := replication.OrderingHeader{ClusterID: cfg.Replication.ClusterID, Incarnation: incarnation}
	for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
		cursor, err := source.Cursor(store)
		if err != nil {
			return fmt.Errorf("read seed store %d cursor: %w", store, err)
		}
		head, err := source.HeadAt(store, cursor)
		if err != nil {
			return fmt.Errorf("authenticate seed store %d head: %w", store, err)
		}
		header.StoreCursors[store-replication.StoreLedger] = cursor
		header.StoreHeads[store-replication.StoreLedger] = head
	}
	info, err := metadata.Info()
	if err != nil {
		return err
	}
	metadataCursor := header.StoreCursors[replication.StoreMetadata-replication.StoreLedger]
	if info.MetadataJournalEpoch != metadataCursor.Generation || info.MetadataJournalSequence != metadataCursor.Sequence {
		return errors.New("seed bbolt projection does not match the metadata journal tail")
	}
	clusterKey, err := replication.DeriveClusterKey(masterKey, incarnation)
	if err != nil {
		return err
	}
	defer clear(clusterKey[:])
	staging, err := os.MkdirTemp(cfg.Storage.DataDir, ".cluster-staging-")
	if err != nil {
		return fmt.Errorf("create cluster-state staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	journal, err := replication.OpenOrderingJournal(
		filepath.Join(staging, "ordering.journal"), clusterKey[:], header, 0, [32]byte{},
	)
	if err != nil {
		return err
	}
	if err := journal.Close(); err != nil {
		return err
	}
	peers := make([]replication.StatePeer, 0, len(cfg.Replication.Peers))
	for _, peer := range cfg.Replication.Peers {
		peers = append(peers, replication.StatePeer{Name: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	state := replication.MemberState{
		Version: replication.StateVersion, ClusterID: cfg.Replication.ClusterID, Incarnation: incarnation,
		NodeID: cfg.Replication.NodeID, Role: role, Term: term, PromisedTerm: term,
		Projection: replication.ProjectionState{MetadataEpoch: metadataCursor.Generation, MetadataSequence: metadataCursor.Sequence},
		Peers:      peers,
	}
	if err := replication.WriteState(filepath.Join(staging, "state.json"), state, clusterKey[:]); err != nil {
		return err
	}
	if err := os.Rename(staging, clusterPath); err != nil {
		return fmt.Errorf("publish cluster member state: %w", err)
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return fmt.Errorf("persist cluster member-state publication: %w", err)
	}
	published = true
	return nil
}

// ClusterStatus authenticates state.json with the member Master Key and
// returns no unauthenticated fallback. It is safe while the process is running
// because state publication is atomic.
func ClusterStatus(ctx context.Context, cfg config.Config) (replication.MemberState, error) {
	if cfg.Replication == nil {
		return replication.MemberState{}, errors.New("replication configuration is required")
	}
	if err := replication.RequireMemberRuntime(cfg.Storage.DataDir, true, "cluster status"); err != nil {
		return replication.MemberState{}, err
	}
	masterKey, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		return replication.MemberState{}, err
	}
	defer clear(masterKey)
	state, err := replication.ReadStateWithMasterKey(cfg.ReplicationStatePath(), masterKey)
	if err != nil {
		return replication.MemberState{}, err
	}
	peers := make([]replication.StatePeer, 0, len(cfg.Replication.Peers))
	for _, peer := range cfg.Replication.Peers {
		peers = append(peers, replication.StatePeer{Name: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	if err := replication.ValidateMemberConfiguration(state, cfg.Replication.ClusterID, cfg.Replication.NodeID, peers); err != nil {
		return replication.MemberState{}, err
	}
	return state, nil
}

// PromoteMember performs the offline candidate side of prepare. The stopped
// candidate owns its directory lock while peers durably promise over fresh
// authenticated control sessions; only then does it publish role=primary.
func PromoteMember(ctx context.Context, cfg config.Config, options PromoteMemberOptions) (PromoteMemberResult, error) {
	if cfg.Replication == nil {
		return PromoteMemberResult{}, errors.New("replication configuration is required")
	}
	if options.ExpectedTerm == 0 || options.OldPrimaryNodeID == "" || options.FencedBy == "" {
		return PromoteMemberResult{}, errors.New("promotion requires expected term, old Primary and fencing assertion")
	}
	if options.Username == "" || len(options.Password) == 0 {
		return PromoteMemberResult{}, errors.New("promotion requires administrator reauthentication")
	}
	if err := replication.RequireMemberRuntime(cfg.Storage.DataDir, true, "cluster promote"); err != nil {
		return PromoteMemberResult{}, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return PromoteMemberResult{}, fmt.Errorf("promotion requires the local Halro process to be stopped: %w", err)
	}
	defer dataLock.Close()
	masterKey, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		return PromoteMemberResult{}, err
	}
	defer clear(masterKey)
	state, clusterKey, err := readMemberState(cfg, masterKey)
	if err != nil {
		return PromoteMemberResult{}, err
	}
	defer clear(clusterKey[:])
	if state.Role != replication.RoleReplica && !(options.Self && state.Role == replication.RolePrimary) {
		return PromoteMemberResult{}, errors.New("only an offline Replica or an explicit --self Primary may be promoted")
	}
	if state.DurableIndex != state.ConfirmedIndex || state.ConfirmedIndex != state.AppliedIndex {
		return PromoteMemberResult{}, errors.New("promotion candidate must have one fully confirmed and applied prefix")
	}
	if state.Term != options.ExpectedTerm || state.AppliedIndex != options.ExpectedAppliedIndex {
		return PromoteMemberResult{}, errors.New("--expect-term or --expect-index does not match authenticated local state")
	}
	if err := authenticateClusterOperator(ctx, cfg, masterKey, options.Username, options.Password, options.TOTPCode); err != nil {
		return PromoteMemberResult{}, err
	}
	if state.PromisedTerm == ^uint64(0) {
		return PromoteMemberResult{}, errors.New("promotion term space is exhausted")
	}
	// A failed prepare may have left promises durable while its response was
	// lost. Promises are intentionally not idempotent without a persisted
	// candidate identity, because accepting the same term for two candidates
	// would violate election safety. Every retry therefore follows the protocol
	// rule T' = max(term, promised_term) + 1 and makes a fresh durable round.
	proposalTerm := state.PromisedTerm + 1
	proposal := replication.PromotionProposal{
		Version: replication.PromotionProtocolVersion, ClusterID: state.ClusterID, Incarnation: state.Incarnation,
		CandidateNodeID: state.NodeID, Term: proposalTerm, ExpectedTerm: options.ExpectedTerm,
		ExpectedAppliedIndex: options.ExpectedAppliedIndex, OldPrimaryNodeID: options.OldPrimaryNodeID, FencedBy: options.FencedBy,
		Self:            options.Self,
		PlannedStepdown: options.PlannedStepdown, ActorID: options.Username,
	}
	if err := proposal.Validate(); err != nil {
		return PromoteMemberResult{}, err
	}
	publisher, err := replication.NewStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], state)
	if err != nil {
		return PromoteMemberResult{}, err
	}
	defer publisher.Close()
	state, err = publisher.Promise(proposalTerm)
	if err != nil {
		return PromoteMemberResult{}, err
	}
	localHello := promotionHello(state)
	promises := make([]replication.PromotionPromise, 0, len(cfg.Replication.Peers))
	var peerErrors []error
	if !options.NoPeerPromise {
		for _, configured := range cfg.Replication.Peers {
			peer := replication.PeerEndpoint{NodeID: configured.Name, Address: configured.Address, SPKISHA256: configured.SPKISHA256}
			promise, requestErr := replication.RequestPromotionPromise(ctx, replication.TLSFiles{
				CAFile: cfg.Replication.TLS.CAFile, CertFile: cfg.Replication.TLS.CertFile, KeyFile: cfg.Replication.TLS.KeyFile,
			}, clusterKey[:], localHello, peer, proposal)
			if requestErr != nil {
				peerErrors = append(peerErrors, fmt.Errorf("peer %s: %w", configured.Name, requestErr))
				continue
			}
			promises = append(promises, promise)
		}
	}
	if err := replication.ValidatePromotionPromises(state, proposal, promises, options.NoPeerPromise); err != nil {
		return PromoteMemberResult{}, errors.Join(err, errors.Join(peerErrors...))
	}
	promoted, err := publisher.Promote(options.ExpectedTerm, options.ExpectedAppliedIndex, proposalTerm)
	if err != nil {
		return PromoteMemberResult{}, err
	}
	if err := appendOfflinePromotionAudit(ctx, cfg, masterKey, clusterKey, publisher, promoted, proposal, promises, options); err != nil {
		return PromoteMemberResult{}, fmt.Errorf("promotion term is durable but its Audit frame could not be recorded; keep the member stopped: %w", err)
	}
	promoted = publisher.Snapshot()
	return PromoteMemberResult{State: promoted, Promises: promises}, nil
}

type offlinePeerSender struct{}

func (offlinePeerSender) Send(string, []byte) error { return replication.ErrPeerNotConnected }

func appendOfflinePromotionAudit(
	ctx context.Context,
	cfg config.Config,
	masterKey []byte,
	clusterKey [32]byte,
	publisher *replication.StatePublisher,
	state replication.MemberState,
	proposal replication.PromotionProposal,
	promises []replication.PromotionPromise,
	options PromoteMemberOptions,
) error {
	journal, err := replication.OpenExistingOrderingJournal(
		cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation,
		state.DurableIndex, state.OrderingHeadMAC,
	)
	if err != nil {
		return err
	}
	defer journal.Close()
	peerNames := make([]string, 0, len(state.Peers))
	for _, peer := range state.Peers {
		peerNames = append(peerNames, peer.Name)
	}
	outbound, err := replication.NewConnectionOutbound(peerNames, offlinePeerSender{})
	if err != nil {
		return err
	}
	coordinator, err := replication.NewPrimaryCoordinator(
		state.ClusterID, state.Incarnation, state.NodeID, state.Term, state.ConfirmedIndex,
		peerNames, journal, outbound, publisher.PublishPrimary,
	)
	if err != nil {
		return err
	}
	// Every term starts with this structural frame. Recording it here makes the
	// promotion and its Audit event one recoverable unconfirmed suffix; startup
	// replays both to peers before it can become ready.
	if _, err := coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone}); err != nil {
		return err
	}
	metadata, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer metadata.Close()
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	auditKey, err := loadAuditHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return err
	}
	defer clear(auditKey)
	auditLog, err := audit.OpenWithOptions(cfg.AuditPath(), auditKey, audit.Options{
		RequireExisting: true,
		AfterDurable: func(batch audit.DurableBatch) error {
			_, recordErr := coordinator.RecordAuditBatch(batch)
			return recordErr
		},
	})
	if err != nil {
		return err
	}
	defer auditLog.Close()
	sort.Slice(promises, func(i, j int) bool { return promises[i].NodeID < promises[j].NodeID })
	encodedPromises, err := json.Marshal(promises)
	if err != nil {
		return err
	}
	promiseDigest := sha256.Sum256(encodedPromises)
	promiseNodes := make([]string, 0, len(promises))
	for _, promise := range promises {
		promiseNodes = append(promiseNodes, promise.NodeID)
	}
	action := "cluster.promote"
	eventMetadata := map[string]any{
		"cluster_id": state.ClusterID, "incarnation": state.Incarnation, "node_id": state.NodeID,
		"term": state.Term, "expect_term": proposal.ExpectedTerm, "expect_index": proposal.ExpectedAppliedIndex,
		"old_primary": proposal.OldPrimaryNodeID, "fenced_by": proposal.FencedBy,
		"promise_nodes": promiseNodes, "promise_digest": "sha256:" + hex.EncodeToString(promiseDigest[:]),
		"self": proposal.Self, "no_peer_promise": options.NoPeerPromise,
	}
	if proposal.PlannedStepdown {
		eventMetadata["from_node"] = proposal.OldPrimaryNodeID
		eventMetadata["to_node"] = state.NodeID
		eventMetadata["leadership_index"] = state.DurableIndex + 1
		requestedID, err := id.New("evt")
		if err != nil {
			return err
		}
		if _, err := auditLog.Append(ctx, audit.Event{
			EventID: requestedID, OccurredAt: time.Now().UTC(), ActorType: "admin", ActorID: options.Username,
			Action: "cluster.stepdown.requested", TargetType: "cluster_member", TargetID: state.NodeID,
			Outcome: "success", Metadata: eventMetadata,
		}); err != nil {
			return err
		}
		action = "cluster.stepdown.completed"
	}
	eventID, err := id.New("evt")
	if err != nil {
		return err
	}
	_, err = auditLog.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "admin", ActorID: options.Username,
		Action: action, TargetType: "cluster_member", TargetID: state.NodeID, Outcome: "success", Metadata: eventMetadata,
	})
	return err
}

func StepdownMember(ctx context.Context, cfg config.Config, to string, options PromoteMemberOptions) (PromoteMemberResult, error) {
	if cfg.Replication == nil || to == "" || to != cfg.Replication.NodeID {
		return PromoteMemberResult{}, errors.New("stepdown must run on the stopped target Replica and --to must equal its node_id")
	}
	if options.NoPeerPromise || options.Self {
		return PromoteMemberResult{}, errors.New("planned stepdown requires the old Primary's durable promise")
	}
	options.PlannedStepdown = true
	options.FencedBy = replication.FencePrimaryPromise
	return PromoteMember(ctx, cfg, options)
}

func promotionHello(state replication.MemberState) replication.Hello {
	currentSchema := uint16(boltstore.CurrentSchemaVersion())
	return replication.Hello{
		ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: state.NodeID,
		Role: replication.RoleAwaitingDecision, Term: state.Term, PromisedTerm: state.PromisedTerm,
		DurableIndex: state.DurableIndex, AppliedIndex: state.AppliedIndex,
		Nonce:    [replication.NonceBytes]byte{1},
		Binary:   replication.VersionRange{Current: memberBinaryVersion, Minimum: memberBinaryVersion, Maximum: memberBinaryVersion},
		Protocol: replication.VersionRange{Current: replication.ProtocolVersion, Minimum: replication.ProtocolVersion, Maximum: replication.ProtocolVersion},
		Schema:   replication.VersionRange{Current: currentSchema, Minimum: currentSchema, Maximum: currentSchema},
		Ledger:   replication.VersionRange{Current: ledger.ReplicationFormatVersion(), Minimum: ledger.ReplicationFormatVersion(), Maximum: ledger.ReplicationFormatVersion()},
		Metadata: replication.VersionRange{Current: metadatajournal.ReplicationFormatVersion(), Minimum: metadatajournal.ReplicationFormatVersion(), Maximum: metadatajournal.ReplicationFormatVersion()},
	}
}

func authenticateClusterOperator(ctx context.Context, cfg config.Config, masterKey []byte, username string, password []byte, totpCode string) error {
	metadata, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer metadata.Close()
	user, err := metadata.GetAdminUser(ctx, username)
	if err != nil {
		adminauth.DummyVerify(password)
		return errors.New("administrator reauthentication failed")
	}
	if user.Role != domain.AdminRoleAdministrator || !adminauth.VerifyPassword(user, password) {
		return errors.New("administrator reauthentication failed")
	}
	authenticators, err := metadata.ListAdminMFAAuthenticators(ctx, username)
	if err != nil {
		return err
	}
	active := make([]domain.AdminMFAAuthenticator, 0, len(authenticators))
	for _, authenticator := range authenticators {
		if authenticator.Status == domain.AdminMFAStatusActive {
			active = append(active, authenticator)
		}
	}
	if len(active) == 0 {
		return nil
	}
	if totpCode == "" {
		return errors.New("administrator TOTP code is required")
	}
	watermarks, err := readTOTPWatermarks(cfg)
	if err != nil {
		return err
	}
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	now := time.Now().UTC()
	for _, authenticator := range active {
		secret, decryptErr := secretVault.DecryptAdminMFA(authenticator.ID, authenticator.Username, authenticator.SecretCiphertext)
		if decryptErr != nil {
			continue
		}
		lastAccepted := authenticator.LastAcceptedTimeStep
		if watermarks[authenticator.ID] > lastAccepted {
			lastAccepted = watermarks[authenticator.ID]
		}
		step, ok := adminauth.VerifyTOTP(secret, totpCode, now, lastAccepted)
		clear(secret)
		if !ok {
			continue
		}
		watermarks[authenticator.ID] = step
		if err := writeTOTPWatermarks(cfg, watermarks); err != nil {
			return err
		}
		return nil
	}
	return errors.New("administrator reauthentication failed")
}

func totpWatermarkPath(cfg config.Config) string {
	return filepath.Join(cfg.ClusterDirectoryPath(), "totp-watermark")
}

func readTOTPWatermarks(cfg config.Config) (map[string]int64, error) {
	payload, err := os.ReadFile(totpWatermarkPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]int64), nil
	}
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > 64<<10 {
		return nil, errors.New("TOTP watermark file is empty or exceeds its size bound")
	}
	var watermarks map[string]int64
	if err := json.Unmarshal(payload, &watermarks); err != nil || watermarks == nil {
		return nil, errors.New("TOTP watermark file is invalid")
	}
	for authenticatorID, step := range watermarks {
		if authenticatorID == "" || len(authenticatorID) > 128 || step < 0 {
			return nil, errors.New("TOTP watermark file contains an invalid entry")
		}
	}
	return watermarks, nil
}

func writeTOTPWatermarks(cfg config.Config, watermarks map[string]int64) error {
	payload, err := json.Marshal(watermarks)
	if err != nil {
		return err
	}
	directory := cfg.ClusterDirectoryPath()
	temporary, err := os.CreateTemp(directory, ".totp-watermark-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
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
	if err := os.Rename(temporaryPath, totpWatermarkPath(cfg)); err != nil {
		return err
	}
	return durable.SyncDirectory(directory)
}

// ReportReplicaBackup verifies the encrypted archive and records its identity
// in the stopped Primary's authenticated Audit/ordering suffix. The next start
// replicates and confirms that suffix before readiness, so a backup report can
// never be a local-only Audit frame.
func ReportReplicaBackup(ctx context.Context, cfg config.Config, archivePath string, backupKey []byte, username string, password []byte, totpCode string) (backup.Manifest, error) {
	if cfg.Replication == nil {
		return backup.Manifest{}, errors.New("replication configuration is required")
	}
	if username == "" || len(password) == 0 {
		return backup.Manifest{}, errors.New("backup reporting requires administrator reauthentication")
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("backup reporting requires the Primary process to be stopped: %w", err)
	}
	defer dataLock.Close()
	masterKey, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer clear(masterKey)
	state, clusterKey, err := readMemberState(cfg, masterKey)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer clear(clusterKey[:])
	if state.Role != replication.RolePrimary || state.DurableIndex != state.ConfirmedIndex {
		return backup.Manifest{}, errors.New("backup reporting requires a stopped Primary with no unconfirmed suffix")
	}
	if err := authenticateClusterOperator(ctx, cfg, masterKey, username, password, totpCode); err != nil {
		return backup.Manifest{}, err
	}
	manifest, err := backup.Verify(archivePath, backupKey)
	if err != nil {
		return backup.Manifest{}, err
	}
	if manifest.FormatVersion != 4 || manifest.SourceRole != string(replication.RoleReplica) ||
		manifest.ClusterID != state.ClusterID || manifest.ClusterIncarnation != state.Incarnation ||
		manifest.Term > state.Term || manifest.AppliedIndex > state.ConfirmedIndex || !stateHasPeer(state, manifest.SourceNodeID) {
		return backup.Manifest{}, errors.New("Replica backup manifest does not belong to a configured peer and confirmed cluster prefix")
	}
	publisher, err := replication.NewStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], state)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer publisher.Close()
	journal, err := replication.OpenExistingOrderingJournal(cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer journal.Close()
	peerNames := make([]string, 0, len(state.Peers))
	for _, peer := range state.Peers {
		peerNames = append(peerNames, peer.Name)
	}
	outbound, err := replication.NewConnectionOutbound(peerNames, offlinePeerSender{})
	if err != nil {
		return backup.Manifest{}, err
	}
	coordinator, err := replication.NewPrimaryCoordinator(state.ClusterID, state.Incarnation, state.NodeID, state.Term, state.ConfirmedIndex, peerNames, journal, outbound, publisher.PublishPrimary)
	if err != nil {
		return backup.Manifest{}, err
	}
	_, journalTerm, _ := journal.Head()
	if journalTerm < state.Term {
		if _, err := coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone}); err != nil {
			return backup.Manifest{}, fmt.Errorf("record offline Primary leadership before backup report: %w", err)
		}
	}
	metadata, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		return backup.Manifest{}, err
	}
	defer metadata.Close()
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer secretVault.Close()
	auditKey, err := loadAuditHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer clear(auditKey)
	auditLog, err := audit.OpenWithOptions(cfg.AuditPath(), auditKey, audit.Options{RequireExisting: true, AfterDurable: func(batch audit.DurableBatch) error {
		_, appendErr := coordinator.RecordAuditBatch(batch)
		return appendErr
	}})
	if err != nil {
		return backup.Manifest{}, err
	}
	defer auditLog.Close()
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return backup.Manifest{}, err
	}
	manifestDigest := sha256.Sum256(manifestJSON)
	eventID, err := id.New("evt")
	if err != nil {
		return backup.Manifest{}, err
	}
	_, err = auditLog.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "admin", ActorID: username,
		Action: "cluster.backup.reported", TargetType: "backup", TargetID: manifest.BackupID, Outcome: "success",
		Metadata: map[string]any{
			"backup_id": manifest.BackupID, "source_node_id": manifest.SourceNodeID,
			"applied_index": manifest.AppliedIndex, "manifest_digest": "sha256:" + hex.EncodeToString(manifestDigest[:]),
			"verification": "passed",
		},
	})
	if err != nil {
		return backup.Manifest{}, err
	}
	return manifest, nil
}

func stateHasPeer(state replication.MemberState, nodeID string) bool {
	for _, peer := range state.Peers {
		if peer.Name == nodeID {
			return true
		}
	}
	return false
}

// LeaveMember turns one explicitly selected, offline member directory back
// into Standalone data. The final member identity is appended to that
// directory's own Audit chain before cluster/ is removed.
func LeaveMember(ctx context.Context, cfg config.Config, confirm, username string, password []byte) error {
	if cfg.Replication == nil {
		return errors.New("replication configuration is required")
	}
	if username == "" || len(password) == 0 {
		return errors.New("cluster leave requires an administrator username and password")
	}
	wantConfirmation := cfg.Replication.ClusterID + "/" + cfg.Replication.NodeID
	if confirm != wantConfirmation {
		return fmt.Errorf("--confirm must exactly equal %q", wantConfirmation)
	}
	clusterPath := cfg.ClusterDirectoryPath()
	if filepath.Dir(clusterPath) != filepath.Clean(cfg.Storage.DataDir) || filepath.Base(clusterPath) != replication.ClusterDirectoryName {
		return errors.New("refusing to remove an unexpected cluster path")
	}
	retiredPath := filepath.Join(cfg.Storage.DataDir, "cluster.left")
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("cluster leave requires the member process to be stopped: %w", err)
	}
	defer dataLock.Close()
	clusterInfo, clusterErr := os.Lstat(clusterPath)
	retiredInfo, retiredErr := os.Lstat(retiredPath)
	if clusterErr == nil && retiredErr == nil {
		return errors.New("refusing cluster leave because active and retired member state both exist")
	}
	if clusterErr != nil && !errors.Is(clusterErr, os.ErrNotExist) {
		return fmt.Errorf("inspect active cluster member state: %w", clusterErr)
	}
	if retiredErr != nil && !errors.Is(retiredErr, os.ErrNotExist) {
		return fmt.Errorf("inspect retired cluster member state: %w", retiredErr)
	}
	if retiredErr == nil {
		if !retiredInfo.IsDir() {
			return errors.New("refusing cluster leave because cluster.left is not a directory")
		}
		if err := authenticateLeaveAdministrator(ctx, cfg, username, password); err != nil {
			return err
		}
		// A prior call crossed the atomic rename boundary. Repeating both
		// directory barriers makes either uncertain fsync outcome recoverable;
		// cleanup may then resume even if an earlier RemoveAll was interrupted.
		if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
			return fmt.Errorf("sync retired cluster member state: %w", err)
		}
		return cleanupRetiredMemberState(cfg.Storage.DataDir, retiredPath)
	}
	if clusterErr != nil {
		// No active state and no tombstone is the idempotent completed result. A
		// previous call may have removed cluster.left and then lost the final
		// directory-fsync result; repeat that barrier before reporting success.
		if err := authenticateLeaveAdministrator(ctx, cfg, username, password); err != nil {
			return err
		}
		return durable.SyncDirectory(cfg.Storage.DataDir)
	}
	if !clusterInfo.IsDir() {
		return errors.New("refusing cluster leave because cluster is not a directory")
	}
	if err := replication.RequireMemberRuntime(cfg.Storage.DataDir, true, "cluster leave"); err != nil {
		return err
	}
	masterKey, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		return err
	}
	defer clear(masterKey)
	state, clusterKey, err := readMemberState(cfg, masterKey)
	if err != nil {
		return err
	}
	defer clear(clusterKey[:])
	metadata, err := boltstore.OpenPrimary(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer metadata.Close()
	user, err := metadata.GetAdminUser(ctx, username)
	if err != nil || user.Role != domain.AdminRoleAdministrator || !adminauth.VerifyPassword(user, password) {
		return errors.New("administrator authentication failed")
	}
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	auditKey, err := loadAuditHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		return err
	}
	defer clear(auditKey)
	auditLog, err := audit.OpenWithOptions(cfg.AuditPath(), auditKey, audit.Options{RequireExisting: true})
	if err != nil {
		return err
	}
	eventID, err := id.New("evt")
	if err == nil {
		_, err = auditLog.Append(ctx, audit.Event{
			EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "admin", ActorID: username,
			Action: "cluster.leave", TargetType: "cluster_member", TargetID: state.NodeID, Outcome: "success",
			Metadata: map[string]any{"cluster_id": state.ClusterID, "incarnation": state.Incarnation,
				"node_id": state.NodeID, "role": state.Role, "term": state.Term, "applied_index": state.AppliedIndex},
		})
	}
	if closeErr := auditLog.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Rename is the irreversible boundary. Keeping the authenticated retired
	// state as a private tombstone makes a power loss atomic: runtime guards no
	// longer see cluster/, while no recursive deletion can strand a half-member
	// that neither leave nor serve can reopen.
	if err := os.Rename(clusterPath, retiredPath); err != nil {
		return fmt.Errorf("retire cluster member state after leave audit: %w", err)
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return fmt.Errorf("sync retired cluster member state: %w", err)
	}
	return cleanupRetiredMemberState(cfg.Storage.DataDir, retiredPath)
}

func authenticateLeaveAdministrator(ctx context.Context, cfg config.Config, username string, password []byte) error {
	metadata, err := boltstore.OpenPrimary(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer metadata.Close()
	user, err := metadata.GetAdminUser(ctx, username)
	if err != nil || user.Role != domain.AdminRoleAdministrator || !adminauth.VerifyPassword(user, password) {
		return errors.New("administrator authentication failed")
	}
	return nil
}

func cleanupRetiredMemberState(dataDir, retiredPath string) error {
	if err := os.RemoveAll(retiredPath); err != nil {
		return fmt.Errorf("remove retired cluster member state: %w", err)
	}
	if err := durable.SyncDirectory(dataDir); err != nil {
		return fmt.Errorf("sync retired cluster member cleanup: %w", err)
	}
	return nil
}
