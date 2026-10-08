package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// The supplied Standalone seed is closed. Both members start from its actual
// authenticated four-store baseline and exchange real TLS frames/ACKs.
func openPrimaryDiskPair(t *testing.T, cfg config.Config) (*Runtime, *Runtime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	primaryListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	replicaListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		primaryListener.Close()
		t.Fatal(err)
	}
	ca, caKey := makeReplicationCertificate(t, nil, nil, true, "disk-primary-ca")
	pc, pk := makeReplicationCertificate(t, ca, caKey, false, "halro-0")
	rc, rk := makeReplicationCertificate(t, ca, caKey, false, "halro-1")
	pp, rp := sha256.Sum256(pc.RawSubjectPublicKeyInfo), sha256.Sum256(rc.RawSubjectPublicKeyInfo)
	replicaCfg := cfg
	replicaRoot := t.TempDir()
	replicaCfg.Storage.DataDir = filepath.Join(replicaRoot, "data")
	replicaCfg.Storage.MasterKey.File = filepath.Join(replicaRoot, "master.key")
	copyTestTree(t, cfg.Storage.DataDir, replicaCfg.Storage.DataDir)
	copyTestRegularFile(t, cfg.Storage.MasterKey.File, replicaCfg.Storage.MasterKey.File)
	cfg.Replication = &config.Replication{ClusterID: "disk-primary-cluster", NodeID: "halro-0", Listen: primaryListener.Addr().String(), TLS: writeReplicationTLSFiles(t, t.TempDir(), ca, pc, pk),
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: replicaListener.Addr().String(), SPKISHA256: "sha256:" + hex.EncodeToString(rp[:])}}}
	replicaCfg.Replication = &config.Replication{ClusterID: cfg.Replication.ClusterID, NodeID: "halro-1", Listen: replicaListener.Addr().String(), TLS: writeReplicationTLSFiles(t, t.TempDir(), ca, rc, rk),
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: primaryListener.Addr().String(), SPKISHA256: "sha256:" + hex.EncodeToString(pp[:])}}}
	t.Cleanup(func() { cancel(); primaryListener.Close(); replicaListener.Close() })
	for _, member := range []struct {
		cfg  config.Config
		role replication.Role
	}{{cfg, replication.RolePrimary}, {replicaCfg, replication.RoleReplica}} {
		if err := EstablishMemberState(ctx, member.cfg, member.role, "inc_disk_primary", 7); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	primary, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { primary.Close() })
	if primary.accountingState.projection == nil || primary.usage.owner.accounting != primary.accountingState.projection || primary.replication.startupReady.Load() {
		t.Fatal("default Primary selected State or published readiness before recovery")
	}
	replica, err := Open(ctx, replicaCfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { replica.Close() })
	done := make(chan struct{}, 2)
	go func() { _ = primary.replication.serve(ctx, primaryListener); done <- struct{}{} }()
	go func() { _ = replica.replication.serve(ctx, replicaListener); done <- struct{}{} }()
	t.Cleanup(func() { cancel(); primaryListener.Close(); replicaListener.Close(); <-done; <-done })
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !primary.replication.startupReady.Load() {
		select {
		case <-deadline.C:
			t.Fatal("default disk Primary startup did not complete")
		case err := <-primary.replication.fatal:
			t.Fatal("default disk Primary startup failed", err)
		case <-ticker.C:
		}
	}
	return primary, replica
}

func TestPrimaryDiskDefaultRecoversAcceptedAndFinalizedPendingRequests(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r.stopBackgroundWork()
	events := []ledger.Event{projectedWriterAcceptance("accepted_without_lease"), projectedWriterAcceptance("finalized_with_pending")}
	base := events[1]
	for i, kind := range []ledger.EventKind{ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventRequestFinalized} {
		e := base
		e.Kind, e.EventID = kind, "pending_fact_"+string(rune('a'+i))
		e.AttemptID = "pending_attempt"
		if kind == ledger.EventReservationCreated {
			e.ReservationMicrosUSD = ledger.MicrosUSD(100)
		}
		if kind == ledger.EventRequestFinalized {
			e.Outcome = "interrupted"
			e.AttemptID = ""
		}
		events = append(events, e)
	}
	for _, event := range events {
		_, _, err := r.ledger.AppendApplyRecordWithReceipt(context.Background(), event, r.accountingState.projection.Apply)
		if err != nil {
			r.Close()
			t.Fatal(err)
		}
	}
	if err := r.usageCollector.CatchUp(context.Background()); err != nil {
		r.Close()
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	primary, replica := openPrimaryDiskPair(t, cfg)
	for _, request := range []string{"accepted_without_lease", "finalized_with_pending"} {
		value, found, err := primary.accountingState.projection.ReadRequest(context.Background(), request)
		if err != nil || !found || value.Finalized == nil {
			t.Fatal("default recovery missed request", request, value, err)
		}
	}
	settlement, found, err := primary.accountingState.projection.ReadSettlement(context.Background(), "pending_attempt")
	if err != nil || !found || settlement.Settlement.Outcome != "recovered_started_unknown_result" {
		t.Fatal("default recovery missed late settlement", settlement, err)
	}
	stats, err := primary.usage.disk.ReadStats(context.Background())
	if err != nil || stats.Metrics.ActiveRequests != 0 || stats.Totals.Requests != 2 || stats.Totals.Attempts != 1 {
		t.Fatal("recovered Usage did not reach complete semantic prefix", stats, err)
	}
	state := primary.replication.publisher.Snapshot()
	deadline := time.Now().Add(5 * time.Second)
	for replica.replication.publisher.Snapshot().AppliedIndex < state.ConfirmedIndex {
		if time.Now().After(deadline) {
			t.Fatal("Replica did not apply confirmed recovery")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, found, err := replica.accountingState.projection.ReadSettlement(context.Background(), "pending_attempt"); err != nil || !found {
		t.Fatal("Replica missed actual confirmed recovery fact", err)
	}
}

func TestPrimaryDiskDefaultNoPeerCloseReleasesStartupLeaseAndOwnedFiles(t *testing.T) {
	cfg, _, _ := replicaStartupPrefixFixture(t)
	b := runtimeCandidateBudgetFixture()
	b.TotalDiskBytes = 1 << 30
	b.BackgroundDiskBytes = 512 << 20
	b.RecoveryDiskBytes = 512 << 20
	b.RecoveryReserveDiskBytes = 512 << 20
	b.LedgerProjectionDiskBytes = 64 << 20
	b.LedgerSegmentIndexDiskBytes = 64 << 20
	b.UsageProjectionDiskBytes = 64 << 20
	addProjectionMappingFixtureRAM(t, &b)
	b.IODiskBytes = 64 << 20
	b.UsageArchiveDiskBytes = 32 << 20
	b.RecoveryClaims = 17
	// Pricing/request + four persistent disk owners + four possible
	// archive-operation owners must coexist after recovery handoff.
	b.MaxClaims = 28
	b.ControlRAMBytes = 28 * resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 11 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	pressure := runtimePressureFixture()
	cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	// Build a real Primary member configuration on the authenticated native seed.
	master, err := unlockMemberMasterKey(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	state, err := replication.ReadStateWithMasterKey(cfg.ReplicationStatePath(), master)
	if err != nil {
		clear(master)
		t.Fatal(err)
	}
	key, err := replication.DeriveClusterKey(master, state.Incarnation)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key[:])
	state.Role = replication.RolePrimary
	if err := replication.WriteState(cfg.ReplicationStatePath(), state, key[:]); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if r.accountingState.projection == nil || r.accountingState.startupLease == nil || r.replication.startupReady.Load() {
		r.Close()
		t.Fatal("no-peer startup was incorrectly published")
	}
	if snapshot := r.resourcePressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Background].Disk != b.LedgerSegmentIndexDiskBytes+b.LedgerProjectionDiskBytes+b.UsageProjectionDiskBytes+b.UsageArchiveDiskBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 0 {
		r.Close()
		t.Fatal("Primary Ledger locator, projections and archive did not retain their Background disk ceilings", snapshot)
	}
	if err := r.maintainUsageDiskCycle(context.Background(), time.Now()); err == nil {
		r.Close()
		t.Fatal("unconfirmed startup admitted Usage maintenance")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot := r.resourcePressure.logicalBudget().Snapshot(); snapshot.Used.Disk != b.UsageArchiveDiskBytes || snapshot.Owners != 1 || snapshot.Classes[resourcebudget.Background].Disk != b.UsageArchiveDiskBytes {
		t.Fatal("Primary close did not release the volatile Ledger index while retaining the Usage archive charge", snapshot)
	}
	for _, path := range []string{primaryProjectionPath(cfg), standaloneUsageProjectionPath(cfg)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("no-peer close retained derived resources", path, err)
		}
	}
}

func TestReplicaDiskDefaultCloseReleasesLedgerDirectoryIndexClaim(t *testing.T) {
	cfg, _, _ := replicaStartupPrefixFixture(t)
	b := runtimeCandidateBudgetFixture()
	b.TotalDiskBytes = 1 << 30
	b.BackgroundDiskBytes = 512 << 20
	b.RecoveryDiskBytes = 512 << 20
	b.RecoveryReserveDiskBytes = 512 << 20
	b.LedgerProjectionDiskBytes = 64 << 20
	b.LedgerSegmentIndexDiskBytes = 64 << 20
	b.UsageProjectionDiskBytes = 64 << 20
	addProjectionMappingFixtureRAM(t, &b)
	b.IODiskBytes = 64 << 20
	b.RecoveryClaims = 17
	b.MaxClaims = 24
	b.ControlRAMBytes = 24 * resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 8 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += 6 * resourcebudget.ClaimRAMBytes
	pressure := runtimePressureFixture()
	cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b

	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := r.resourcePressure.logicalBudget().Snapshot()
	if snapshot.Classes[resourcebudget.Background].Disk < b.LedgerSegmentIndexDiskBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 0 {
		_ = r.Close()
		t.Fatal("Replica Ledger locator disk claim was not retained by the Background owner", snapshot)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot := r.resourcePressure.logicalBudget().Snapshot(); snapshot.Used.Disk != 0 {
		t.Fatal("Replica close retained a Ledger directory index claim without a persistent sidecar", snapshot)
	}
}
