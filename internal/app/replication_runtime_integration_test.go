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
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame(t *testing.T) {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	primary, err := OpenWithOptions(context.Background(), primaryConfig, logger, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := OpenWithOptions(context.Background(), replicaConfig, logger, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
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

	primaryContext, primaryCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer primaryCancel()
	replicaContext, replicaCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer replicaCancel()
	primaryError := make(chan error, 1)
	replicaError := make(chan error, 1)
	go func() { primaryError <- primary.replication.serve(primaryContext, listenerPrimary) }()
	go func() { replicaError <- replica.replication.serve(replicaContext, listenerReplica) }()
	startupDeadline := time.Now().Add(8 * time.Second)
	for !primary.replication.startupReady.Load() {
		if time.Now().After(startupDeadline) {
			t.Fatal("Primary did not complete startup peer adjudication")
		}
		time.Sleep(10 * time.Millisecond)
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
			t.Fatal(err)
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
	ledgerHead, err := primary.ledger.Append(context.Background(), ledger.Event{
		EventID: "evt_runtime_usage_replication", Kind: ledger.EventRequestAccepted,
		RequestID: "req_runtime_usage_replication", ProjectID: bootstrap.ProjectID,
		PeriodID: bootstrap.ProjectID + ":2026-09-27:UTC", OccurredAt: time.Now().UTC(),
		PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		primaryState := primary.replication.publisher.Snapshot()
		replicaState := replica.replication.publisher.Snapshot()
		if primaryState.ConfirmedIndex >= 2 && primaryState.DurableIndex == primaryState.ConfirmedIndex &&
			replicaState.DurableIndex == primaryState.ConfirmedIndex && replicaState.ConfirmedIndex == primaryState.ConfirmedIndex &&
			replicaState.AppliedIndex == primaryState.ConfirmedIndex &&
			replica.audit.Summary().Records == primary.audit.Summary().Records &&
			replica.usage != nil && replica.usage.Watermark().Sequence == ledgerHead.Sequence {
			break
		}
		if time.Now().After(deadline) {
			var replicaUsage uint64
			if replica.usage != nil {
				replicaUsage = replica.usage.Watermark().Sequence
			}
			t.Fatalf("replication did not converge: primary=%#v replica=%#v audit=%d/%d usage=%d/%d",
				primaryState, replicaState, primary.audit.Summary().Records, replica.audit.Summary().Records,
				replicaUsage, ledgerHead.Sequence)
		}
		time.Sleep(10 * time.Millisecond)
	}
	replicaCancel()
	if err := <-replicaError; err != nil {
		t.Fatalf("Replica replication serve shutdown: %v", err)
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
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
	primaryCancel()
	if err := <-primaryError; err != nil {
		t.Fatalf("Primary replication serve shutdown: %v", err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	promotedMetadata, err := boltstore.OpenReadOnly(replicaConfig.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer promotedMetadata.Close()
	promotedKey, err := promotedMetadata.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil || promotedKey.Enabled {
		t.Fatalf("revoked Gateway Key revived after promotion: key=%#v err=%v", promotedKey, err)
	}
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
