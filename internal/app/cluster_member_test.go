package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/replication"
)

func TestEstablishMemberStatePublishesAuthenticatedBaselineAtomically(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	standalone, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-0", Listen: "127.0.0.1:9910",
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), cfg, replication.RolePrimary, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	masterKey, err := unlockMemberMasterKey(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(masterKey)
	state, err := replication.ReadStateWithMasterKey(cfg.ReplicationStatePath(), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if state.Role != replication.RolePrimary || state.Term != 1 || state.DurableIndex != 0 || state.Projection.MetadataEpoch == 0 {
		t.Fatalf("member state=%#v", state)
	}
	clusterKey, err := replication.DeriveClusterKey(masterKey, state.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(clusterKey[:])
	header, err := replication.ReadOrderingHeader(cfg.OrderingJournalPath(), clusterKey[:])
	if err != nil {
		t.Fatal(err)
	}
	metadataCursor := header.StoreCursors[replication.StoreMetadata-replication.StoreLedger]
	if metadataCursor.Generation != state.Projection.MetadataEpoch || metadataCursor.Sequence != state.Projection.MetadataSequence {
		t.Fatalf("metadata baseline=%#v projection=%#v", metadataCursor, state.Projection)
	}
	before, err := os.ReadFile(cfg.ReplicationStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := EstablishMemberState(context.Background(), cfg, replication.RoleReplica, "inc_02", 2); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second establishment error=%v", err)
	}
	after, err := os.ReadFile(cfg.ReplicationStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed repeated establishment changed authenticated member state")
	}
}

func TestPromoteMemberNoPeerPromiseIsExplicitAndDurable(t *testing.T) {
	cfg := testConfig(t)
	password := []byte("correct horse battery staple")
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", password); err != nil {
		t.Fatal(err)
	}
	standalone, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), cfg, replication.RoleReplica, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	// Simulate a failed prepare whose durable promise survived but whose
	// response did not. A retry must allocate a newer term instead of reusing 2.
	masterKey, err := unlockMemberMasterKey(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	state, err := replication.ReadStateWithMasterKey(cfg.ReplicationStatePath(), masterKey)
	if err != nil {
		clear(masterKey)
		t.Fatal(err)
	}
	clusterKey, err := replication.DeriveClusterKey(masterKey, state.Incarnation)
	clear(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := replication.NewStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], state)
	clear(clusterKey[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promise(2); err != nil {
		publisher.Close()
		t.Fatal(err)
	}
	publisher.Close()
	result, err := PromoteMember(context.Background(), cfg, PromoteMemberOptions{
		ExpectedTerm: 1, ExpectedAppliedIndex: 0, OldPrimaryNodeID: "halro-0",
		FencedBy: replication.FenceNodeIsolated, NoPeerPromise: true,
		Username: "admin", Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Role != replication.RolePrimary || result.State.Term != 3 || result.State.PromisedTerm != 3 || len(result.Promises) != 0 {
		t.Fatalf("promotion result=%#v", result)
	}
	onDisk, err := ClusterStatus(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Role != replication.RolePrimary || onDisk.Term != 3 || onDisk.PromisedTerm != 3 {
		t.Fatalf("on-disk state=%#v", onDisk)
	}
	if onDisk.DurableIndex != 2 || onDisk.ConfirmedIndex != 0 {
		t.Fatalf("promotion Audit ordering progress=%#v", onDisk)
	}
}

func TestPromoteSelfRequiresExplicitStoppedPrimaryFence(t *testing.T) {
	cfg := testConfig(t)
	password := []byte("correct horse battery staple")
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", password); err != nil {
		t.Fatal(err)
	}
	standalone, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-0", Listen: "127.0.0.1:9910",
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), cfg, replication.RolePrimary, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := PromoteMember(context.Background(), cfg, PromoteMemberOptions{
		ExpectedTerm: 1, OldPrimaryNodeID: "halro-0", FencedBy: replication.FenceSelfStopped,
		NoPeerPromise: true, Username: "admin", Password: password,
	}); err == nil || !strings.Contains(err.Error(), "explicit --self") {
		t.Fatalf("self promotion without --self error=%v", err)
	}
	result, err := PromoteMember(context.Background(), cfg, PromoteMemberOptions{
		ExpectedTerm: 1, OldPrimaryNodeID: "halro-0", FencedBy: replication.FenceSelfStopped,
		NoPeerPromise: true, Self: true, Username: "admin", Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Role != replication.RolePrimary || result.State.Term != 2 || result.State.PromisedTerm != 2 {
		t.Fatalf("self promotion state=%#v", result.State)
	}
}

func TestReplicaMaintenanceSentinelIsPrivateAndImmediatelyObservable(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	standalone, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), cfg, replication.RoleReplica, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	if err := SetMemberMaintenance(context.Background(), cfg, true); err != nil {
		t.Fatal(err)
	}
	requested, err := memberMaintenanceRequested(cfg)
	if err != nil || !requested {
		t.Fatalf("maintenance requested=%v error=%v", requested, err)
	}
	info, err := os.Stat(maintenancePath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("maintenance mode=%#o want 0600", info.Mode().Perm())
	}
	if err := SetMemberMaintenance(context.Background(), cfg, false); err != nil {
		t.Fatal(err)
	}
	requested, err = memberMaintenanceRequested(cfg)
	if err != nil || requested {
		t.Fatalf("maintenance requested after off=%v error=%v", requested, err)
	}
}

func TestReplicaMaintenanceRejectsAnExistingUnsafeSentinel(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	standalone, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), cfg, replication.RoleReplica, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(maintenancePath(cfg), []byte("unsafe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the process umask, so force the unsafe mode even when
	// the test runner starts with a restrictive umask.
	if err := os.Chmod(maintenancePath(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(maintenancePath(cfg)); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("unsafe sentinel mode=%v err=%v", info, err)
	}
	if err := SetMemberMaintenance(context.Background(), cfg, true); err == nil {
		t.Fatal("maintenance accepted an existing non-private sentinel")
	}
}

func TestMaintenanceHandlersExposeOnlyLivenessAndAuthenticatedModeMetric(t *testing.T) {
	cfg := testConfig(t)
	cfg.Metrics.Enabled = true
	cfg.Metrics.RequireAuth = true
	token := "maintenance-metrics-token"
	runtime := &Runtime{config: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), metricsTokenHash: sha256.Sum256([]byte(token))}
	health, metrics := maintenanceHandlers(runtime, cfg)

	live := httptest.NewRecorder()
	health.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if live.Code != http.StatusOK || !strings.Contains(live.Body.String(), `"mode":"maintenance"`) {
		t.Fatalf("maintenance liveness=%d body=%s", live.Code, live.Body.String())
	}
	ready := httptest.NewRecorder()
	health.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable || ready.Header().Get("Retry-After") != "1" {
		t.Fatalf("maintenance readiness=%d retry-after=%q", ready.Code, ready.Header().Get("Retry-After"))
	}
	unauthorized := httptest.NewRecorder()
	metrics.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated maintenance metrics=%d", unauthorized.Code)
	}
	authorizedRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer "+token)
	authorized := httptest.NewRecorder()
	metrics.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), "halro_cluster_maintenance 1") {
		t.Fatalf("maintenance metrics=%d body=%s", authorized.Code, authorized.Body.String())
	}
}

func TestLeaveMemberRequiresPasswordAndAuditsBeforeRemovingState(t *testing.T) {
	cfg := testConfig(t)
	password := []byte("correct horse battery staple")
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", password); err != nil {
		t.Fatal(err)
	}
	seedRuntime, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	readerPassword := []byte("another correct horse battery staple")
	reader, err := adminauth.NewUser("reader", readerPassword, domain.AdminRoleReadOnly, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(reader.PasswordHash)
	defer clear(reader.PasswordSalt)
	if _, err := seedRuntime.store.PutAdminUser(context.Background(), reader, 0); err != nil {
		t.Fatal(err)
	}
	if err := seedRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), cfg, replication.RoleReplica, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	if err := LeaveMember(context.Background(), cfg, "production-a/halro-1", "admin", []byte("wrong password")); err == nil {
		t.Fatal("cluster leave accepted the wrong password")
	}
	if _, err := os.Stat(cfg.ReplicationStatePath()); err != nil {
		t.Fatalf("failed leave removed member state: %v", err)
	}
	if err := LeaveMember(context.Background(), cfg, "production-a/halro-1", "reader", readerPassword); err == nil {
		t.Fatal("cluster leave accepted a read-only administrator")
	}
	if _, err := os.Stat(cfg.ReplicationStatePath()); err != nil {
		t.Fatalf("read-only leave removed member state: %v", err)
	}
	if err := LeaveMember(context.Background(), cfg, "production-a/halro-1", "admin", password); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.ClusterDirectoryPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cluster directory still exists after leave: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.DataDir, "cluster.left")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired cluster tombstone was not cleaned up: %v", err)
	}
	cfg.Replication = nil
	standalone, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	foundLeave := false
	if _, err := standalone.audit.Replay(func(record audit.Record) error {
		if record.Event.Action == "cluster.leave" {
			foundLeave = record.Event.Metadata["role"] == string(replication.RoleReplica) &&
				record.Event.Metadata["node_id"] == "halro-1"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !foundLeave {
		t.Fatal("cluster.leave Audit record lacks the member role or node identity")
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaveMemberResumesRetiredTombstoneCleanup(t *testing.T) {
	cfg := testConfig(t)
	password := []byte("correct horse battery staple")
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", password); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), cfg, replication.RoleReplica, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	retiredPath := filepath.Join(cfg.Storage.DataDir, "cluster.left")
	if err := os.Rename(cfg.ClusterDirectoryPath(), retiredPath); err != nil {
		t.Fatal(err)
	}
	if err := LeaveMember(context.Background(), cfg, "production-a/halro-1", "admin", []byte("wrong password")); err == nil {
		t.Fatal("tombstone cleanup accepted the wrong administrator password")
	}
	if _, err := os.Stat(retiredPath); err != nil {
		t.Fatalf("failed tombstone cleanup attempt removed retired state: %v", err)
	}
	if err := LeaveMember(context.Background(), cfg, "production-a/halro-1", "admin", password); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(retiredPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired member state still exists after resumed cleanup: %v", err)
	}
	// A lost response after the final directory fsync is idempotent too.
	if err := LeaveMember(context.Background(), cfg, "production-a/halro-1", "admin", password); err != nil {
		t.Fatalf("completed leave retry failed: %v", err)
	}
}
