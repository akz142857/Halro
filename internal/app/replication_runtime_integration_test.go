package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame(t *testing.T) {
	testPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame(t, false)
}

func TestAuthenticatedRecoveryWriterRealPrimaryConfirmationAndStepdown(t *testing.T) {
	testPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame(t, true)
}

func configureGovernanceJournalDiskFixture(b *config.ResourceBudget) {
	b.GovernanceLogDiskBytes = 1 << 20
	b.AuditLogDiskBytes = 1 << 20
	b.MetadataJournalDiskBytes = 1 << 20
	b.MetadataStoreDiskBytes = 8 << 20
	b.OrderingJournalDiskBytes = 1 << 20
	b.TotalDiskBytes = 64 << 20
	b.BackgroundDiskBytes = 32 << 20
	b.RecoveryDiskBytes = 32 << 20
	b.RecoveryReserveDiskBytes = 32 << 20
	b.RecoveryClaims += 5
	// Four persistent journals and one Metadata data-file owner need normal claims after recovery handoff,
	// while the existing request, pricing IO and bounded Audit IO claims remain available.
	b.MaxClaims += 10
	b.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.ControlRAMBytes += 10 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += 5 * resourcebudget.ClaimRAMBytes
	// Mapping/freelist admission is separate from the persistent file's bytes.
	ram := b.MetadataStoreDiskBytes + 32<<20
	b.TotalRAMBytes += 2 * ram
	b.BackgroundRAMBytes += ram
	b.RecoveryRAMBytes += ram
	b.RecoveryReserveRAMBytes += ram
	// Startup compares the preexisting Usage archive while native owners remain live.
	b.TotalRAMBytes += 512 << 20
	b.RecoveryRAMBytes += 512 << 20
	b.RecoveryReserveRAMBytes += 512 << 20
	b.TotalDiskBytes += 2 << 30
	b.RecoveryDiskBytes += 1 << 30
	b.RecoveryReserveDiskBytes += 1 << 30
}

func testPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame(t *testing.T, authenticated bool) {
	primaryConfig := testConfig(t)
	adminPassword := []byte("correct horse battery staple")
	if err := Initialize(primaryConfig); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), primaryConfig, "admin", adminPassword); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), primaryConfig, BootstrapOptions{
		ProviderName: "Test", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com",
		ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "HA", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := OpenWithOptions(context.Background(), primaryConfig, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}

	replicaRoot := t.TempDir()
	replicaConfig := primaryConfig
	replicaConfig.Storage.DataDir = filepath.Join(replicaRoot, "data")
	replicaConfig.Storage.MasterKey.File = filepath.Join(replicaRoot, "master.key")
	copyTestTree(t, primaryConfig.Storage.DataDir, replicaConfig.Storage.DataDir)
	copyTestRegularFile(t, primaryConfig.Storage.MasterKey.File, replicaConfig.Storage.MasterKey.File)

	listenerPrimary, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenerReplica, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		listenerPrimary.Close()
		t.Fatal(err)
	}
	ca, caKey := makeReplicationCertificate(t, nil, nil, true, "halro-test-ca")
	primaryCert, primaryKey := makeReplicationCertificate(t, ca, caKey, false, "halro-0")
	replicaCert, replicaKey := makeReplicationCertificate(t, ca, caKey, false, "halro-1")
	primaryTLS := writeReplicationTLSFiles(t, t.TempDir(), ca, primaryCert, primaryKey)
	replicaTLS := writeReplicationTLSFiles(t, t.TempDir(), ca, replicaCert, replicaKey)
	primaryPin := sha256.Sum256(primaryCert.RawSubjectPublicKeyInfo)
	replicaPin := sha256.Sum256(replicaCert.RawSubjectPublicKeyInfo)
	primaryConfig.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-0", Listen: listenerPrimary.Addr().String(), TLS: primaryTLS,
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: listenerReplica.Addr().String(), SPKISHA256: "sha256:" + hex.EncodeToString(replicaPin[:])}},
	}
	replicaConfig.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: listenerReplica.Addr().String(), TLS: replicaTLS,
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: listenerPrimary.Addr().String(), SPKISHA256: "sha256:" + hex.EncodeToString(primaryPin[:])}},
	}
	if err := EstablishMemberState(context.Background(), primaryConfig, replication.RolePrimary, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	if err := EstablishMemberState(context.Background(), replicaConfig, replication.RoleReplica, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	primaryPressure, primaryBudget := runtimePressureFixture(), runtimeCandidateBudgetFixture()
	configureGovernanceJournalDiskFixture(&primaryBudget)
	primaryConfig.ResourcePressure, primaryConfig.ResourceBudget = &primaryPressure, &primaryBudget
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	primary, err := OpenWithOptions(context.Background(), primaryConfig, logger, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	if primary.usage.Aggregate != nil || primary.usage.disk == nil || primary.usage.owner == nil || primary.usage.owner.accounting == nil {
		t.Fatal("Primary retained resident Usage or lacked its disk evidence owner")
	}
	if primary.accountingState.projection == nil || primary.accountingState.reader != primary.accountingState.projection || primary.usage.owner.accounting != primary.accountingState.projection || primary.usage.owner.accountingPath != "" {
		t.Fatal("Primary did not select one shared disk business accounting owner")
	}
	if snapshot := primary.resourcePressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Background].Disk != primaryBudget.GovernanceLogDiskBytes+primaryBudget.AuditLogDiskBytes+primaryBudget.MetadataStoreDiskBytes+primaryBudget.MetadataJournalDiskBytes+primaryBudget.OrderingJournalDiskBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 0 || primary.governance.log.Summary().Bytes > primaryBudget.GovernanceLogDiskBytes || primary.audit.Summary().Bytes > primaryBudget.AuditLogDiskBytes {
		t.Fatal("Primary did not retain the configured native journal disk ceilings", snapshot, primary.governance.log.Summary(), primary.audit.Summary())
	}
	if info, err := os.Stat(primaryConfig.OrderingJournalPath()); err != nil || info.Size() > primaryBudget.OrderingJournalDiskBytes {
		t.Fatal("Primary Ordering journal exceeds its configured file ceiling", info, err)
	}
	if info, err := os.Stat(primaryConfig.MetadataPath()); err != nil || info.Size() > primaryBudget.MetadataStoreDiskBytes {
		t.Fatal("Metadata data file exceeded its configured ceiling", info, err)
	}
	primaryMetadataState, err := primary.store.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(primaryMetadataState.Path); err != nil || info.Size() > primaryBudget.MetadataJournalDiskBytes {
		t.Fatal("Primary Metadata journal exceeds its configured file ceiling", info, err)
	}
	if budget := primary.resourcePressure.logicalBudget(); budget == nil || budget.Snapshot().Acquired[resourcebudget.Recovery] < 4 || budget.Snapshot().RecoveryClaims != 4 {
		t.Fatal("Primary construction did not retain NativeSource, Audit/Governance caches and pricing lifetime owners while publishing Metadata disk ownership")
	}

	// This harness serves only the replication listeners; no HTTP listener can
	// own an admitted request. Use the production drain for its real deferred,
	// checkpoint/Usage and alert producers, with no business HTTP servers.
	primary.installPlannedStepdownDrain(nil)
	pressurePolicy := runtimePressureFixture()
	replicaConfig.ResourcePressure = &pressurePolicy
	replicaBudget := runtimeCandidateBudgetFixture()
	configureGovernanceJournalDiskFixture(&replicaBudget)
	replicaConfig.ResourceBudget = &replicaBudget
	replica, err := OpenWithOptions(context.Background(), replicaConfig, logger, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if replica.accountingState.replica == nil || replica.accountingState.projection == nil || replica.usage.Aggregate != nil || replica.usage.disk != replica.accountingState.replica.usageDisk {
		t.Fatal("Replica did not publish one disk Ledger/Usage generation")
	}
	if replica.resourcePressure == nil {
		t.Fatal("configured Replica pressure sampler missing")
	}
	if snapshot := replica.resourcePressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Background].Disk != replicaBudget.GovernanceLogDiskBytes+replicaBudget.AuditLogDiskBytes+replicaBudget.MetadataStoreDiskBytes+replicaBudget.MetadataJournalDiskBytes+replicaBudget.OrderingJournalDiskBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 0 || replica.governance.log.Summary().Bytes > replicaBudget.GovernanceLogDiskBytes || replica.audit.Summary().Bytes > replicaBudget.AuditLogDiskBytes {
		t.Fatal("Replica did not retain the configured native journal disk ceilings after confirmed-prefix application", snapshot, replica.governance.log.Summary(), replica.audit.Summary())
	}
	if info, err := os.Stat(replicaConfig.OrderingJournalPath()); err != nil || info.Size() > replicaBudget.OrderingJournalDiskBytes {
		t.Fatal("Replica Ordering journal exceeds its configured file ceiling", info, err)
	}
	if info, err := os.Stat(replicaConfig.MetadataPath()); err != nil || info.Size() > replicaBudget.MetadataStoreDiskBytes {
		t.Fatal("Metadata data file exceeded its configured ceiling", info, err)
	}
	replicaMetadataState, err := replica.store.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(replicaMetadataState.Path); err != nil || info.Size() > replicaBudget.MetadataJournalDiskBytes {
		t.Fatal("Replica Metadata journal exceeds its configured file ceiling", info, err)
	}
	pressureDeadline := time.Now().Add(time.Second)
	for replica.resourcePressure.monitor.Snapshot().Samples == 0 && time.Now().Before(pressureDeadline) {
		time.Sleep(time.Millisecond)
	}
	if replica.resourcePressure.monitor.Snapshot().Samples == 0 {
		t.Fatal("Replica sampler did not run")
	}
	// Tiny fixture thresholds deliberately close physical admission. Replica
	// transport and candidate readiness still follow authenticated HA progress.
	beforeReplicaMetadata, err := os.ReadFile(replicaConfig.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	beforeReplicaAudit, err := os.ReadFile(replicaConfig.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	replica.replicaAdminRouter().ServeHTTP(login, adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{
		"username": "admin", "password": string(adminPassword),
	}))
	if login.Code != http.StatusOK || len(login.Result().Cookies()) == 0 {
		t.Fatalf("Replica admin login=%d body=%s", login.Code, login.Body.String())
	}
	afterReplicaMetadata, _ := os.ReadFile(replicaConfig.MetadataPath())
	afterReplicaAudit, _ := os.ReadFile(replicaConfig.AuditPath())
	if string(afterReplicaMetadata) != string(beforeReplicaMetadata) || string(afterReplicaAudit) != string(beforeReplicaAudit) {
		t.Fatal("Replica admin login mutated replicated metadata or Audit")
	}
	rejected := httptest.NewRecorder()
	replica.gatewayRouter().ServeHTTP(rejected, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rejected.Code != http.StatusServiceUnavailable || rejected.Header().Get("Retry-After") != "1" {
		t.Fatalf("Replica gateway response=%d retry-after=%q body=%s", rejected.Code, rejected.Header().Get("Retry-After"), rejected.Body.String())
	}
	ready := httptest.NewRecorder()
	replica.gatewayRouter().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("Replica readiness=%d body=%s", ready.Code, ready.Body.String())
	}

	// Populate a source-durable ordered backlog before socket Run starts. The
	// old whole-suffix catch-up filled its 64-record queue and closed the
	// session before a writer could drain it; ACK windows must finish this.
	for i := 0; i < 256; i++ {
		if _, err := primary.audit.Append(context.Background(), audit.Event{EventID: fmt.Sprintf("evt_pre_run_%d", i), OccurredAt: time.Now().UTC(), ActorType: "system", Action: "replication.pre_run_backlog", Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	startupHead, _, _ := primary.replication.journal.Head()
	if startupHead <= 64 {
		t.Fatal("fixture did not exceed the socket queue", startupHead)
	}
	ledgerCut, _, _ := primary.ledger.ChainHead()
	recoveryLedger := replication.StoreCursor{Generation: ledgerCut.Generation, Sequence: ledgerCut.Sequence}
	if _, err := primary.replication.coordinator.CaptureRecoveryTarget(context.Background(), recoveryLedger); !errors.Is(err, replication.ErrReplicationUnavailable) {
		t.Fatal("pre-transport recovery target did not fail closed", err)
	}
	primaryContext, primaryCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer primaryCancel()
	replicaContext, replicaCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer replicaCancel()
	primaryError := make(chan error, 1)
	replicaError := make(chan error, 1)
	go func() { primaryError <- primary.replication.serve(primaryContext, listenerPrimary) }()
	go func() { replicaError <- replica.replication.serve(replicaContext, listenerReplica) }()
	startupDeadline := time.Now().Add(20 * time.Second)
	for !primary.replication.startupReady.Load() {
		if time.Now().After(startupDeadline) {
			confirmed, available, cause := primary.replication.coordinator.Status()
			var primaryTransport, replicaTransport error
			select {
			case primaryTransport = <-primaryError:
			default:
			}
			select {
			case replicaTransport = <-replicaError:
			default:
			}
			var startupCause error
			select {
			case startupCause = <-primary.replication.fatal:
			default:
			}
			t.Fatalf("Primary did not complete startup peer adjudication: primary=%#v replica=%#v coordinator=%d/%t/%v transport=%v/%v startup=%v budget=%+v", primary.replication.publisher.Snapshot(), replica.replication.publisher.Snapshot(), confirmed, available, cause, primaryTransport, replicaTransport, startupCause, primary.resourcePressure.logicalBudget().Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if budget := primary.resourcePressure.logicalBudget(); budget.Snapshot().Acquired[resourcebudget.Recovery] <= 3 {
		t.Fatal("live HA windows did not use the runtime shared manager")
	}
	for _, runtime := range []*Runtime{primary, replica} {
		stats := runtime.resourcePressure.logicalBudget().Snapshot()
		if stats.RecoveryClaims < 3 || stats.Classes[resourcebudget.Recovery].RAM < runtime.config.ResourceBudget.ReplicationQueueRAMBytes+runtime.config.ResourceBudget.ReplicationReadRAMBytes+runtime.config.ResourceBudget.ReplicationReaderRAMBytes {
			t.Fatal("live member session did not charge its shared queue envelope", stats)
		}
	}
	if confirmed, _, _ := primary.replication.coordinator.Status(); confirmed < startupHead {
		t.Fatalf("Primary became ready before startup prefix confirmation: confirmed=%d target=%d", confirmed, startupHead)
	}
	// The Ledger is unchanged, but recovery must confirm the whole ordering cut,
	// including this later Audit fact over the actual mTLS/apply/ACK path.
	if _, err := primary.audit.Append(context.Background(), audit.Event{EventID: "evt_recovery_target_current", OccurredAt: time.Now().UTC(), ActorType: "system", Action: "replication.recovery_target", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	recoveryTarget, err := primary.replication.coordinator.CaptureRecoveryTarget(context.Background(), recoveryLedger)
	if err != nil {
		t.Fatal(err)
	}
	if evidence := recoveryTarget.Evidence(); evidence.Index <= startupHead || evidence.Stores[replication.StoreAudit-replication.StoreLedger].Sequence < 257 || evidence.Stores[0] != recoveryLedger {
		t.Fatal("recovery target omitted non-Ledger durable facts", evidence)
	}
	recoveryContext, recoveryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recoveryCancel()
	if err := primary.replication.coordinator.WaitRecoveryTarget(recoveryContext, recoveryTarget); err != nil {
		t.Fatal(err)
	}
	if confirmed, _, _ := primary.replication.coordinator.Status(); confirmed < recoveryTarget.Evidence().Index {
		t.Fatal("recovery returned before full ordering confirmation", confirmed, recoveryTarget.Evidence())
	}
	select {
	case err := <-primaryError:
		t.Fatalf("Primary replication transport stopped before object replication: %v", err)
	case err := <-replicaError:
		t.Fatalf("replication transport stopped before object replication: %v", err)
	default:
	}
	objectName := "file_runtime.content"
	objectBytes := []byte("sealed-runtime-provider-object")
	objectContext, objectCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer objectCancel()
	if err := primary.replication.replicateProviderObject(objectContext, objectName, objectBytes); err != nil {
		select {
		case transportErr := <-primaryError:
			t.Fatalf("replicate object: %v; Primary transport: %v", err, transportErr)
		case transportErr := <-replicaError:
			t.Fatalf("replicate object: %v; Replica transport: %v", err, transportErr)
		default:
			confirmed, available, cause := primary.replication.coordinator.Status()
			t.Fatalf("object replication: %v; Primary d/c/a=%d/%d/%d, Replica=%#v; coordinator=%d/%t/%v", err, primary.replication.publisher.Snapshot().DurableIndex, primary.replication.publisher.Snapshot().ConfirmedIndex, primary.replication.publisher.Snapshot().AppliedIndex, replica.replication.publisher.Snapshot(), confirmed, available, cause)
		}
	}
	replicatedObject, err := os.ReadFile(filepath.Join(replicaConfig.Storage.DataDir, "provider-objects", objectName))
	if err != nil || string(replicatedObject) != string(objectBytes) {
		t.Fatalf("replicated provider object=%q err=%v", replicatedObject, err)
	}
	key, err := primary.store.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	key.Enabled = false
	if _, err := primary.store.PutGatewayKey(context.Background(), key, key.Revision, nil); err != nil {
		t.Fatal(err)
	}
	revocationDeadline := time.Now().Add(5 * time.Second)
	for {
		replicatedKey, readErr := replica.store.GetGatewayKey(context.Background(), bootstrap.KeyID)
		if readErr == nil && !replicatedKey.Enabled {
			break
		}
		if time.Now().After(revocationDeadline) {
			t.Fatalf("revoked Gateway Key did not survive replication: enabled=%v err=%v", replicatedKey.Enabled, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := primary.audit.Append(context.Background(), audit.Event{
		EventID: "evt_runtime_replication", OccurredAt: time.Now().UTC(), ActorType: "system",
		Action: "replication.integration", Outcome: "success",
	}); err != nil {
		t.Fatal(err)
	}
	// First-run recovery still confirms all non-Ledger facts when the native
	// Ledger has its initial generation in the authenticated ordering header
	// but no Ledger data records. Keep that exact physical cursor.
	emptyMasterKey, err := unlockMemberMasterKey(context.Background(), primaryConfig)
	if err != nil {
		t.Fatal(err)
	}
	emptyLedgerKey, err := loadLedgerHMACKey(primary.store, primary.vault, emptyMasterKey)
	clear(emptyMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(emptyLedgerKey)
	primary.draining.Store(true)
	emptyLease, emptyCandidate := projectedWriterCandidate(t, primary.ledger, primaryConfig.LedgerPath(), emptyLedgerKey)
	emptyContext, emptyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer emptyCancel()
	emptyWriter, err := primary.newProjectedRecoveryWriter(emptyContext, emptyLease, emptyCandidate)
	if err != nil {
		t.Fatal("initial empty Ledger recovery writer", err)
	}
	emptyResult, err := budget.RecoverProjectedRequests(emptyContext, emptyCandidate, emptyWriter, emptyWriter.role, ledger.ProjectionPageLimits{Rows: 8, Bytes: 8 << 20})
	if err != nil || emptyResult.AttemptsSettled != 0 || emptyResult.RequestsFinalized != 0 || emptyResult.ConfirmedHead.Generation != 1 || emptyResult.ConfirmedHead.Sequence != 0 {
		emptyWriter.Close()
		t.Fatal("first-run recovery skipped or misbound full ordering confirmation", emptyResult, err)
	}
	emptyEvidence := emptyWriter.authority.Evidence()
	if emptyEvidence.Index == 0 || emptyEvidence.Stores[replication.StoreLedger-replication.StoreLedger] != (replication.StoreCursor{Generation: emptyResult.ConfirmedHead.Generation, Sequence: emptyResult.ConfirmedHead.Sequence}) || primary.replication.publisher.Snapshot().ConfirmedIndex < emptyEvidence.Index {
		emptyWriter.Close()
		t.Fatal("empty Ledger credited without actual complete-prefix ACK", emptyEvidence)
	}
	// Exercise ordinary accounting with disk reads AND error-returning disk
	// Apply after the real two-level/ordering maintenance confirmation. This
	// harness does not publish a production Runtime generation. Its selected
	// disk/Usage observers mirror facts so the default Runtime can resume below.
	diskAccounting, err := budget.NewWithProjection(emptyContext, primary.ledger, emptyCandidate, primary.accounting.Periods(), emptyLease, budget.Options{})
	if err != nil {
		emptyWriter.Close()
		t.Fatal("bind normal disk accounting to confirmed maintenance cut", err)
	}
	defer func() { primary.Close(); emptyCandidate.Close() }()
	diskAccounting.AddObserver(func(record ledger.Record) {
		head, hash, readErr := emptyCandidate.ReadPosition(context.Background())
		if readErr != nil || head != (ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}) || hash != record.Hash {
			t.Error("normal disk observer preceded actual projection Apply", head, readErr)
		}
		if applyErr := primary.accountingState.projection.Apply(context.Background(), record); applyErr != nil {
			t.Error("test-only selected disk mirror diverged", applyErr)
		}
		primary.usageCollector.Observe(record)
	})
	if err := emptyWriter.Close(); err != nil {
		t.Fatal(err)
	}
	diskRequest, err := diskAccounting.BeginRequest(emptyContext, bootstrap.ProjectID, "req_runtime_disk_accounting")
	if err != nil {
		t.Fatal(err)
	}
	diskAttempt, err := diskAccounting.ReserveAttempt(emptyContext, diskRequest, 10_000, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := diskAccounting.MarkStarted(emptyContext, diskAttempt); err != nil {
		t.Fatal(err)
	}
	if err := diskAccounting.Settle(emptyContext, diskAttempt, budget.Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := diskAccounting.Finalize(emptyContext, diskRequest, "success"); err != nil {
		t.Fatal(err)
	}
	diskHead, diskHash, err := emptyCandidate.ReadPosition(emptyContext)
	if err != nil || diskHead.Sequence != 5 || diskHash == ([32]byte{}) {
		t.Fatal("ordinary disk accounting did not apply its full lifecycle", diskHead, err)
	}
	diskTarget, err := primary.replication.coordinator.CaptureRecoveryTarget(emptyContext, replication.StoreCursor{Generation: diskHead.Generation, Sequence: diskHead.Sequence})
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.replication.coordinator.WaitRecoveryTarget(emptyContext, diskTarget); err != nil {
		t.Fatal("normal disk lifecycle lacks actual Replica confirmation", err)
	}
	diskReplicaReport, diskPartial, err := ledger.InspectReplayAuthenticated(replicaConfig.LedgerPath(), emptyLedgerKey, nil)
	if err != nil || diskPartial || diskReplicaReport.Head != diskHead || diskReplicaReport.ChainHash != diskHash || usageWatermarkForTest(t, replica).Sequence != diskHead.Sequence {
		t.Fatal("ordinary disk accounting differs from real Replica applied WAL/Usage", diskReplicaReport, diskHead, err)
	}
	// The harness resumes the existing State-backed Runtime below. Permanently
	// retire its temporary disk Manager first; a stale owner must not append
	// after the native maintenance lease has been released.
	if err := diskAccounting.Retire(emptyContext); err != nil {
		t.Fatal("retire temporary disk accounting after real HA confirmation", err)
	}
	beforeRetired := primary.ledger.Stats().Records
	if _, err := diskAccounting.BeginRequest(emptyContext, bootstrap.ProjectID, "req_runtime_stale_disk_owner"); !errors.Is(err, budget.ErrAccountingRetired) || primary.ledger.Stats().Records != beforeRetired {
		t.Fatal("retired disk owner appended after its lease ended", err)
	}
	primary.draining.Store(false)
	// Exercise the actual default Manager, including synchronous State/Usage
	// Apply, rather than writing a fact that its local state never observes.
	normalRecords := make(chan ledger.Record, 1)
	primary.accounting.AddObserver(func(record ledger.Record) {
		if record.Event.Kind == ledger.EventRequestAccepted && record.Event.RequestID == "req_runtime_usage_replication" {
			normalRecords <- record
		}
	})
	if _, err := primary.accounting.BeginRequest(context.Background(), bootstrap.ProjectID, "req_runtime_usage_replication"); err != nil {
		t.Fatal("normal Primary accounting append/apply", err)
	}
	ledgerRecord := <-normalRecords
	ledgerHead := ledger.Watermark{Generation: ledgerRecord.Generation, Sequence: ledgerRecord.Sequence, Offset: ledgerRecord.Offset}
	if ledgerRecord.Hash == ([32]byte{}) || ledgerRecord.Epoch != uint8(ledger.ReplicationFormatVersion()) || accountingReaderHead(t, primary.accountingState.reader) != ledgerHead {
		t.Fatal("normal Manager did not apply actual native evidence", ledgerRecord, accountingReaderHead(t, primary.accountingState.reader))
	}
	normalTarget, err := primary.replication.coordinator.CaptureRecoveryTarget(context.Background(), replication.StoreCursor{Generation: ledgerHead.Generation, Sequence: ledgerHead.Sequence})
	if err != nil || normalTarget.Evidence().Index == 0 {
		t.Fatal("normal applied Record has no complete ordering target", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		primaryState := primary.replication.publisher.Snapshot()
		replicaState := replica.replication.publisher.Snapshot()
		if primaryState.ConfirmedIndex >= 2 && primaryState.DurableIndex == primaryState.ConfirmedIndex &&
			replicaState.DurableIndex == primaryState.ConfirmedIndex && replicaState.ConfirmedIndex == primaryState.ConfirmedIndex &&
			replicaState.AppliedIndex == primaryState.ConfirmedIndex &&
			replica.audit.Summary().Records == primary.audit.Summary().Records &&
			replica.usage != nil && usageWatermarkForTest(t, replica).Sequence == ledgerHead.Sequence {
			break
		}
		if time.Now().After(deadline) {
			var replicaUsage uint64
			if replica.usage != nil {
				replicaUsage = usageWatermarkForTest(t, replica).Sequence
			}
			t.Fatalf("replication did not converge: primary=%#v replica=%#v audit=%d/%d usage=%d/%d",
				primaryState, replicaState, primary.audit.Summary().Records, replica.audit.Summary().Records,
				replicaUsage, ledgerHead.Sequence)
		}
		time.Sleep(10 * time.Millisecond)
	}
	seenLedgerRecord := false
	replicaMasterKey, err := unlockMemberMasterKey(context.Background(), replicaConfig)
	if err != nil {
		t.Fatal(err)
	}
	replicaLedgerKey, err := loadLedgerHMACKey(replica.store, replica.vault, replicaMasterKey)
	clear(replicaMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(replicaLedgerKey)
	replicaReport, partial, err := ledger.InspectReplayAuthenticated(replicaConfig.LedgerPath(), replicaLedgerKey, func(actual ledger.Record) error {
		if actual.Sequence == ledgerRecord.Sequence {
			if !reflect.DeepEqual(actual, ledgerRecord) {
				return errors.New("append Record differs from authenticated Replica apply/replay")
			}
			seenLedgerRecord = true
		}
		return nil
	})
	if err != nil || partial || !seenLedgerRecord || replicaReport.Head != ledgerHead || replicaReport.ChainHash != ledgerRecord.Hash {
		t.Fatal("durable Record did not survive real HA", seenLedgerRecord, replicaReport, partial, err)
	}
	// Drain the actual Runtime producers and permanently retire its old Manager
	// while the data/ACK transport is still live, before acquiring native ownership.
	if err := primary.replication.plannedStepdownDrain(); err != nil {
		t.Fatal("production business drain before native maintenance", err)
	}
	if _, err := primary.accounting.BeginRequest(context.Background(), bootstrap.ProjectID, "req_after_runtime_drain"); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal("Runtime drain left its old Manager admitted", err)
	}
	drainedTarget, err := primary.replication.coordinator.CaptureRecoveryTarget(context.Background(), replication.StoreCursor{Generation: ledgerHead.Generation, Sequence: ledgerHead.Sequence})
	if err != nil {
		t.Fatal(err)
	}
	drainedCtx, drainedCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainedCancel()
	if err := primary.replication.coordinator.WaitRecoveryTarget(drainedCtx, drainedTarget); err != nil {
		t.Fatal("drained background prefix did not retain live data/ACK confirmation", err)
	}
	if usageWatermarkForTest(t, primary) != ledgerHead {
		t.Fatal("Runtime drain did not reconcile Usage to its frozen Ledger cut")
	}
	// This harness owns no admitted HTTP requests. Exercise the native exclusive
	// lease with a fresh derived candidate and the actual full ordering ACK path;
	// this is not selection/publication of a new application Runtime projection.
	lease, err := primary.beginAccountingMaintenance(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var candidate interface {
		ReadFinalization(context.Context, string) (ledger.Record, bool, error)
		Close() error
	}
	var boltCandidate *ledger.LedgerProjection
	var authCandidate *ledger.AuthenticatedLedgerProjection
	var authOptions ledger.AuthenticatedLedgerOptions
	if authenticated {
		authOptions = authenticatedWriterOptions(t)
	}
	if err := lease.WithHead(context.Background(), func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(primaryConfig.LedgerPath(), replicaLedgerKey, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("maintenance input differs from authenticated native cut"))
		}
		if authenticated {
			authCandidate, err = ledger.BuildAuthenticatedLedgerProjection(ctx, primaryConfig.LedgerPath(), replicaLedgerKey, filepath.Join(t.TempDir(), "maintenance-auth"), report, authOptions)
			candidate = authCandidate
		} else {
			boltCandidate, err = ledger.BuildLedgerProjection(ctx, primaryConfig.LedgerPath(), replicaLedgerKey, filepath.Join(t.TempDir(), "maintenance-candidate.db"), report)
			candidate = boltCandidate
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if authCandidate != nil {
		defer authCandidate.Discard()
	}
	primary.draining.Store(true) // Harness owns no admitted business workers.
	confirmContext, confirmCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer confirmCancel()
	var writer *projectedRecoveryWriter
	if authenticated {
		writer, err = primary.newAuthenticatedRecoveryWriter(confirmContext, lease, authCandidate)
	} else {
		writer, err = primary.newProjectedRecoveryWriter(confirmContext, lease, boltCandidate)
	}
	if err != nil {
		candidate.Close()
		t.Fatal(err)
	}
	defer func() { writer.Close(); candidate.Close() }()
	var result budget.ProjectedRecoveryResult
	if authenticated {
		result, err = budget.RecoverAuthenticatedRequests(confirmContext, authCandidate, writer, writer.role, ledger.ProjectionPageLimits{Rows: 8, Bytes: 8 << 20})
	} else {
		result, err = budget.RecoverProjectedRequests(confirmContext, boltCandidate, writer, writer.role, ledger.ProjectionPageLimits{Rows: 8, Bytes: 8 << 20})
	}
	if err != nil || result.AttemptsSettled != 0 || result.RequestsFinalized != 1 || result.ConfirmedHead.Sequence != ledgerRecord.Sequence+1 {
		t.Fatal("actual maintenance recovery failed", result, err)
	}
	finalRecord, found, err := candidate.ReadFinalization(confirmContext, ledgerRecord.Event.RequestID)
	if err != nil || !found || finalRecord.Sequence != result.ConfirmedHead.Sequence || finalRecord.Hash == ([32]byte{}) {
		t.Fatal(finalRecord, found, err)
	}
	if usageWatermarkForTest(t, replica).Sequence != finalRecord.Sequence {
		t.Fatal("maintenance confirmation omitted real Replica apply", usageWatermarkForTest(t, replica), finalRecord.Sequence)
	}
	// Keep the original writer alive across the actual planned handoff below.
	// Closing it first would only exercise closure, not role-authority revocation.

	replicaCancel()
	if err := <-replicaError; err != nil {
		t.Fatalf("Replica replication serve shutdown: %v", err)
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if stats := replica.resourcePressure.logicalBudget().Snapshot(); stats.Used != (resourcebudget.Cost{RAM: 5 * resourcebudget.ClaimRAMBytes, Disk: replicaBudget.GovernanceLogDiskBytes + replicaBudget.AuditLogDiskBytes + replicaBudget.MetadataStoreDiskBytes + replicaBudget.MetadataJournalDiskBytes + replicaBudget.OrderingJournalDiskBytes}) || stats.Classes[resourcebudget.Background].Disk != replicaBudget.GovernanceLogDiskBytes+replicaBudget.AuditLogDiskBytes+replicaBudget.MetadataStoreDiskBytes+replicaBudget.MetadataJournalDiskBytes+replicaBudget.OrderingJournalDiskBytes || stats.RecoveryClaims != 0 {
		t.Fatal("Replica Close did not retire queue fees while retaining the native journal owners", stats)
	}
	replicaState, err := ClusterStatus(context.Background(), replicaConfig)
	if err != nil {
		t.Fatal(err)
	}
	stepdownContext, stepdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stepdownCancel()
	promoted, err := StepdownMember(stepdownContext, replicaConfig, "halro-1", PromoteMemberOptions{
		ExpectedTerm: replicaState.Term, ExpectedAppliedIndex: replicaState.AppliedIndex,
		OldPrimaryNodeID: "halro-0", Username: "admin", Password: adminPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.State.Role != replication.RolePrimary || promoted.State.Term != replicaState.Term+1 {
		t.Fatalf("promoted state=%#v", promoted.State)
	}
	oldPrimaryState, err := ClusterStatus(context.Background(), primaryConfig)
	if err != nil {
		t.Fatal(err)
	}
	if oldPrimaryState.Role != replication.RoleReplica || oldPrimaryState.PromisedTerm != promoted.State.Term {
		t.Fatalf("old Primary did not durably step down: %#v", oldPrimaryState)
	}
	beforeRevokedWrite, beforeRevokedHash, _ := primary.ledger.ChainHead()
	if err := writer.ConfirmRecoveryPrefix(context.Background(), beforeRevokedWrite); err == nil {
		t.Fatal("former Primary maintenance writer retained confirmation authority")
	}
	if _, err := writer.AppendApplyRecovery(context.Background(), projectedWriterAcceptance("revoked_request")); err == nil {
		t.Fatal("former Primary maintenance writer appended a new native fact")
	}
	afterRevokedWrite, afterRevokedHash, _ := primary.ledger.ChainHead()
	if afterRevokedWrite != beforeRevokedWrite || afterRevokedHash != beforeRevokedHash || primary.ledger.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("revoked writer changed native history or failed to fence accounting")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	primaryCancel()
	if err := <-primaryError; err != nil {
		t.Fatalf("Primary replication serve shutdown: %v", err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	if stats := primary.resourcePressure.logicalBudget().Snapshot(); stats.Used != (resourcebudget.Cost{RAM: 5 * resourcebudget.ClaimRAMBytes, Disk: primaryBudget.GovernanceLogDiskBytes + primaryBudget.AuditLogDiskBytes + primaryBudget.MetadataStoreDiskBytes + primaryBudget.MetadataJournalDiskBytes + primaryBudget.OrderingJournalDiskBytes}) || stats.Classes[resourcebudget.Background].Disk != primaryBudget.GovernanceLogDiskBytes+primaryBudget.AuditLogDiskBytes+primaryBudget.MetadataStoreDiskBytes+primaryBudget.MetadataJournalDiskBytes+primaryBudget.OrderingJournalDiskBytes || stats.RecoveryClaims != 0 {
		t.Fatal("Primary Close did not retire candidate/queue fees while retaining the native journal owners", stats)
	}
	rejoined, err := OpenWithOptions(context.Background(), primaryConfig, logger, OpenOptions{})
	if err != nil {
		t.Fatalf("former Primary could not reopen as Replica after planned handoff: %v", err)
	}
	if rejoined.replication.publisher.Snapshot().Role != replication.RoleReplica {
		t.Fatal("former Primary reopened with an unexpected role")
	}
	// Prepare an independent candidate from the actual authenticated applied cut.
	// The ordinary runtime already selects its own disk generation; construction
	// must not replace or retire that live owner.
	candidatePath := filepath.Join(t.TempDir(), "replica-owner.db")
	accountingCandidate, err := rejoined.replication.prepareReplicaAccountingGeneration(context.Background(), rejoined.ledger, rejoined.store, candidatePath)
	if err != nil {
		t.Fatal("build role-bound Replica Ledger/Usage generation", err)
	}
	candidateCut := accountingCandidate.prefix
	ownerHead, ownerHash, err := accountingCandidate.ledger.ReadPosition(context.Background())
	allowedCursors, cursorErr := rejoined.replication.journal.StoreCursorsThrough(rejoined.replication.publisher.Snapshot().AppliedIndex)
	allowedLedger := allowedCursors[replication.StoreLedger-replication.StoreLedger]
	if err != nil || cursorErr != nil || ownerHead != candidateCut.Head || ownerHash != candidateCut.Hash || ownerHead.Generation != allowedLedger.Generation || ownerHead.Sequence != allowedLedger.Sequence {
		_ = accountingCandidate.Close()
		t.Fatal("candidate differs from exact applied role cut", ownerHead, ownerHash, candidateCut, allowedLedger, err, cursorErr)
	}

	candidateUsage, usageErr := accountingCandidate.usageDisk.ReadStats(context.Background())
	if usageErr != nil || candidateUsage.Watermark.Sequence != candidateCut.Head.Sequence {
		_ = accountingCandidate.Close()
		t.Fatal("fresh Usage differs from applied role cut")
	}
	if err := accountingCandidate.Close(); err != nil {
		t.Fatal(err)
	}
	promotedMetadata, err := boltstore.OpenReadOnly(replicaConfig.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	promotedKey, err := promotedMetadata.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil || promotedKey.Enabled {
		t.Fatalf("revoked Gateway Key revived after promotion: key=%#v err=%v", promotedKey, err)
	}
	if err := promotedMetadata.Close(); err != nil {
		t.Fatal(err)
	}
	// Exercise the real server lifecycle after promotion. A clean Primary
	// shutdown must confirm its final audit while the Replica transport still
	// runs, leaving a prefix eligible for an offline seed approval.
	rejoined.config.Server.GatewayListen = unusedLoopbackAddress(t)
	rejoined.config.Server.AdminListen = unusedLoopbackAddress(t)
	replicaConfig.Server.GatewayListen = unusedLoopbackAddress(t)
	replicaConfig.Server.AdminListen = unusedLoopbackAddress(t)
	promotedRuntime, err := OpenWithOptions(context.Background(), replicaConfig, logger, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rejoinedContext, stopRejoined := context.WithCancel(context.Background())
	rejoinedError := make(chan error, 1)
	go func() { rejoinedError <- rejoined.RunWithReady(rejoinedContext, nil) }()
	promotedContext, stopPromoted := context.WithCancel(context.Background())
	promotedError := make(chan error, 1)
	go func() { promotedError <- promotedRuntime.RunWithReady(promotedContext, nil) }()
	startupDeadline = time.Now().Add(8 * time.Second)
	for !promotedRuntime.replication.startupReady.Load() {
		if time.Now().After(startupDeadline) {
			t.Fatal("promoted Primary did not become ready after former Primary rejoined")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A structural-only roll must advance the actual Replica owner, even when
	// no semantic Ledger event follows. Usage has no new fact to apply. This
	// exercises the default live disk generation and its structural cursor,
	// while Usage keeps its last semantic position.
	usageBeforeRoll := usageWatermarkForTest(t, rejoined)
	rolled, err := promotedRuntime.ledger.Roll()
	if err != nil || !rolled.Rolled {
		t.Fatal("live Primary roll", rolled, err)
	}
	rollHead, _, _ := promotedRuntime.ledger.ChainHead()
	rollCtx, cancelRoll := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelRoll()
	rollTarget, err := promotedRuntime.replication.coordinator.CaptureRecoveryTarget(rollCtx, replication.StoreCursor{Generation: rollHead.Generation, Sequence: rollHead.Sequence})
	if err != nil {
		t.Fatal(err)
	}
	if err := promotedRuntime.replication.coordinator.WaitRecoveryTarget(rollCtx, rollTarget); err != nil {
		t.Fatal("live structural roll confirmation", err)
	}
	for rejoined.replication.publisher.Snapshot().AppliedIndex < rollTarget.Evidence().Index {
		if err := rollCtx.Err(); err != nil {
			t.Fatal("Replica did not apply structural-only roll", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if accountingReaderHead(t, rejoined.accountingState.reader) != rollHead || usageWatermarkForTest(t, rejoined) != usageBeforeRoll {
		t.Fatal("structural roll did not preserve semantic Usage and move Replica cut", accountingReaderHead(t, rejoined.accountingState.reader), rollHead, usageWatermarkForTest(t, rejoined), usageBeforeRoll)
	}
	// A runtime-owned worker may append an ordered frame when cancellation
	// settles in-flight work. It must finish before system.shutdown is written
	// and the final prefix is confirmed, rather than writing after transport
	// cancellation in Close.
	backgroundAudit := make(chan error, 1)
	promotedRuntime.backgroundWait.Add(1)
	go func() {
		defer promotedRuntime.backgroundWait.Done()
		<-promotedRuntime.backgroundCtx.Done()
		backgroundAudit <- appendSystemAudit(promotedRuntime.audit, promotedRuntime.store, "system.shutdown.background.probe")
	}()
	stopPromoted()
	if err := <-promotedError; err != nil {
		t.Fatalf("promoted Primary graceful shutdown: %v", err)
	}
	if err := <-backgroundAudit; err != nil {
		t.Fatalf("background shutdown audit: %v", err)
	}
	var backgroundSequence, shutdownSequence uint64
	if _, err := promotedRuntime.audit.Replay(func(record audit.Record) error {
		switch record.Event.Action {
		case "system.shutdown.background.probe":
			backgroundSequence = record.Sequence
		case "system.shutdown":
			shutdownSequence = record.Sequence
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if backgroundSequence == 0 || shutdownSequence <= backgroundSequence {
		t.Fatalf("background audit must precede final shutdown audit: background=%d shutdown=%d", backgroundSequence, shutdownSequence)
	}
	if err := promotedRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	stopped, err := ClusterStatus(context.Background(), replicaConfig)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.DurableIndex != stopped.ConfirmedIndex || stopped.AppliedIndex != stopped.ConfirmedIndex ||
		stopped.Projection.Index != stopped.AppliedIndex {
		t.Fatalf("cleanly stopped Primary has an unconfirmed or unprojected tail: %#v", stopped)
	}
	stopRejoined()
	if err := <-rejoinedError; err != nil {
		t.Fatalf("former Primary Replica shutdown: %v", err)
	}
	if err := rejoined.Close(); err != nil {
		t.Fatal(err)
	}
}

func unusedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func copyTestTree(t *testing.T, source, destination string) {
	t.Helper()
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		copyTestRegularFile(t, path, target)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func copyTestRegularFile(t *testing.T, source, destination string) {
	t.Helper()
	payload, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeReplicationCertificate(t *testing.T, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey, isCA bool, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	if issuer == nil {
		issuer, issuerKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func writeReplicationTLSFiles(t *testing.T, directory string, ca, certificate *x509.Certificate, key *ecdsa.PrivateKey) config.ReplicationTLS {
	t.Helper()
	caPath := filepath.Join(directory, "ca.pem")
	certPath := filepath.Join(directory, "tls.pem")
	keyPath := filepath.Join(directory, "tls-key.pem")
	writeReplicationPEM(t, caPath, "CERTIFICATE", ca.Raw)
	writeReplicationPEM(t, certPath, "CERTIFICATE", certificate.Raw)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeReplicationPEM(t, keyPath, "PRIVATE KEY", keyDER)
	return config.ReplicationTLS{CAFile: caPath, CertFile: certPath, KeyFile: keyPath}
}

func writeReplicationPEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
