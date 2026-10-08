package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type haMetadataMaintenanceFixture struct {
	configs     [2]config.Config
	keys        HAMetadataKeys
	instance    string
	oldCut      replication.MemberState
	oldMetadata [2][]byte
}

func prepareHAMetadataMaintenanceFixture(t *testing.T) haMetadataMaintenanceFixture {
	t.Helper()
	standalone, _, _ := openBootstrappedRuntime(t)
	return prepareHAMetadataMaintenanceFixtureFromRuntime(t, standalone)
}

func prepareHAMetadataMaintenanceFixtureFromRuntime(t *testing.T, standalone *Runtime) haMetadataMaintenanceFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cfg := standalone.config
	master, err := unlockMasterKey(ctx, cfg, standalone.store)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	ledgerKey, err := loadLedgerHMACKey(standalone.store, v, master)
	if err != nil {
		t.Fatal(err)
	}
	auditKey, err := loadAuditHMACKey(standalone.store, v, master)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := loadMetadataJournalHMACKey(standalone.store, v, master)
	if err != nil {
		t.Fatal(err)
	}
	governanceKey, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	fixture := haMetadataMaintenanceFixture{keys: HAMetadataKeys{Master: master, Ledger: ledgerKey, Audit: auditKey, Governance: governanceKey, Metadata: metadataKey}, instance: standalone.instanceID}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	replica := cfg
	replicaRoot := t.TempDir()
	replica.Storage.DataDir = filepath.Join(replicaRoot, "data")
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeFile {
		replica.Storage.MasterKey.File = filepath.Join(replicaRoot, "master.key")
	}
	copyTestTree(t, cfg.Storage.DataDir, replica.Storage.DataDir)
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeFile {
		copyTestRegularFile(t, cfg.Storage.MasterKey.File, replica.Storage.MasterKey.File)
	}
	primaryListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	replicaListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ca, caKey := makeReplicationCertificate(t, nil, nil, true, "migration-ca")
	pc, pk := makeReplicationCertificate(t, ca, caKey, false, "halro-0")
	rc, rk := makeReplicationCertificate(t, ca, caKey, false, "halro-1")
	pp, rp := sha256.Sum256(pc.RawSubjectPublicKeyInfo), sha256.Sum256(rc.RawSubjectPublicKeyInfo)
	cfg.Replication = &config.Replication{ClusterID: "production-a", NodeID: "halro-0", Listen: primaryListener.Addr().String(), TLS: writeReplicationTLSFiles(t, t.TempDir(), ca, pc, pk), Peers: []config.ReplicationPeer{{Name: "halro-1", Address: replicaListener.Addr().String(), SPKISHA256: "sha256:" + hex.EncodeToString(rp[:])}}}
	replica.Replication = &config.Replication{ClusterID: "production-a", NodeID: "halro-1", Listen: replicaListener.Addr().String(), TLS: writeReplicationTLSFiles(t, t.TempDir(), ca, rc, rk), Peers: []config.ReplicationPeer{{Name: "halro-0", Address: primaryListener.Addr().String(), SPKISHA256: "sha256:" + hex.EncodeToString(pp[:])}}}
	for _, pair := range []struct {
		cfg  config.Config
		role replication.Role
	}{{cfg, replication.RolePrimary}, {replica, replication.RoleReplica}} {
		if err := EstablishMemberState(ctx, pair.cfg, pair.role, "inc_ha_baseline", 7); err != nil {
			t.Fatal(err)
		}
	}
	limits := selectedRuntimeBudgetFixture()
	candidates := runtimeCandidateBudgetFixture()
	limits.ReplicationCandidateRAMBytes = candidates.ReplicationCandidateRAMBytes
	limits.ReplicationQueueRAMBytes = candidates.ReplicationQueueRAMBytes
	limits.ReplicationReadRAMBytes = candidates.ReplicationReadRAMBytes
	limits.ReplicationReaderRAMBytes = candidates.ReplicationReaderRAMBytes
	limits.OrderingJournalDiskBytes = 1 << 20
	pressure := runtimePressureFixture()
	pressure.MaxAge = config.Duration(time.Minute)
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	replica.ResourceBudget, replica.ResourcePressure = &limits, &pressure
	fixture.configs = [2]config.Config{cfg, replica}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	primaryRuntime, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal("legacy Primary", err)
	}
	replicaRuntime, err := Open(ctx, replica, logger)
	if err != nil {
		primaryRuntime.Close()
		t.Fatal("legacy Replica", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	go func() { done <- primaryRuntime.replication.serve(runCtx, primaryListener) }()
	go func() { done <- replicaRuntime.replication.serve(runCtx, replicaListener) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		ps, rs := primaryRuntime.replication.publisher.Snapshot(), replicaRuntime.replication.publisher.Snapshot()
		if ps.DurableIndex > 0 && ps.ConfirmedIndex == ps.DurableIndex && rs.AppliedIndex == ps.DurableIndex {
			fixture.oldCut = ps
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("legacy actual TLS prefix did not confirm", ps, rs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done
	<-done
	if err := primaryRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := replicaRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	for i, c := range fixture.configs {
		raw, err := os.ReadFile(filepathMetadataJournal(c))
		if err != nil {
			t.Fatal(err)
		}
		fixture.oldMetadata[i] = raw
	}
	return fixture
}

func functionalHAMetadataManager(t *testing.T, cfg config.Config) *resourcebudget.Manager {
	t.Helper()
	manager, err := resourcebudget.New(cfg.ResourceBudget.Limits(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// This is functional protocol evidence; capacity/production uses the real sampler.
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return manager
}

func prepareHAMetadataMaintenanceArtifacts(t *testing.T, f haMetadataMaintenanceFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cfgPath, archivePath := filepath.Join(t.TempDir(), "config.yaml"), filepath.Join(t.TempDir(), "source.hmbk")
	if err := os.WriteFile(cfgPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	backupKey := bytes.Repeat([]byte{0x52}, 32)
	if _, err := CreateBackupWithOptions(ctx, configWithoutBudget(f.configs[1]), cfgPath, archivePath, backupKey, CreateBackupOptions{Replica: true}); err != nil {
		t.Fatal("stopped actual Replica backup", err)
	}
	archive, err := backup.OpenVerified(ctx, archivePath, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	guards := [2]*lock.Lock{}
	for i, cfg := range f.configs {
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer guards[i].Close()
	}
	directory := filepath.Join(f.configs[0].Storage.DataDir, HAMetadataBaselineDirectory)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	manager := functionalHAMetadataManager(t, f.configs[0])
	preparation, err := PrepareHAMetadataBaseline(ctx, f.configs[0], f.keys, archive, directory, guards[0], metadatastate.BaselineOptions{Identity: sha256.Sum256([]byte("independent-ha-baseline-test")), InstanceID: f.instance, KeyGeneration: 1, Budget: manager, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20})
	if err != nil {
		t.Fatal("actual Primary producer", err)
	}
	if err := preparation.Close(); err != nil {
		t.Fatal(err)
	}
	installed, err := InstallHAMetadataBaseline(ctx, f.configs[1], f.keys, directory, guards[1], functionalHAMetadataManager(t, f.configs[1]))
	if err != nil {
		t.Fatal("actual independently qualified Replica carrier installation", err)
	}
	if err := installed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHAMetadataMaintenanceRealTLSBoundaryAndReopen(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var err error
	guards := [2]*lock.Lock{}
	for i, cfg := range f.configs {
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer guards[i].Close()
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for attempt := 0; attempt < 2; attempt++ {
		owners := [2]*HAMetadataMaintenance{}
		budgets := [2]*resourcebudget.Manager{}
		for i, cfg := range f.configs {
			budgets[i] = functionalHAMetadataManager(t, cfg)
			owners[i], err = OpenHAMetadataMaintenance(ctx, cfg, f.keys, guards[i], budgets[i], logger)
			if err != nil {
				t.Fatalf("open maintenance member %d attempt %d: %v", i, attempt, err)
			}
			hello, err := owners[i].localHello()
			if err != nil {
				t.Fatal(err)
			}
			expected := uint16(1)
			if attempt == 1 {
				expected = 2
			}
			if hello.Protocol.Current != 2 || hello.Protocol.Minimum != 2 || hello.Metadata.Current != expected || hello.Metadata.Minimum != 1 || hello.Metadata.Maximum != 2 {
				t.Fatal("maintenance dual-format negotiation differs", hello)
			}
			legacy := hello
			legacy.Protocol = replication.VersionRange{Current: 1, Minimum: 1, Maximum: 1}
			if ready, err := owners[i].authorizeHello(ctx, legacy); err == nil || ready {
				t.Fatal("legacy client joined explicit wire2 maintenance")
			}
		}
		runCtx, stop := context.WithCancel(ctx)
		complete := make(chan replication.MemberState, 2)
		done := make(chan error, 2)
		for _, owner := range owners {
			go func(m *HAMetadataMaintenance) {
				done <- m.Run(runCtx, func(s replication.MemberState) error { complete <- s; return nil })
			}(owner)
		}
		for range 2 {
			select {
			case s := <-complete:
				if s.Incarnation != f.oldCut.Incarnation || s.Term != 7 || s.ConfirmedIndex != f.oldCut.DurableIndex+1 || s.AppliedIndex != s.ConfirmedIndex || s.Projection.MetadataEpoch != f.oldCut.Projection.MetadataEpoch+1 || s.Projection.MetadataSequence != 0 {
					t.Fatal("boundary reset identity/index or advanced wrong applied cut", s, f.oldCut)
				}
			case e := <-done:
				stop()
				t.Fatal("actual maintenance transport exited", e)
			case <-ctx.Done():
				stop()
				t.Fatal("actual boundary ACK/apply timed out", ctx.Err(), owners[0].Snapshot(), owners[1].Snapshot())
			}
		}
		primaryRecord, err := owners[0].member.journal.Record(f.oldCut.DurableIndex + 1)
		if err != nil {
			t.Fatal(err)
		}
		replicaRecord, err := owners[1].member.journal.Record(f.oldCut.DurableIndex + 1)
		if err != nil {
			t.Fatal(err)
		}
		if primaryRecord.Kind != replication.KindMetadataBaseline || primaryRecord.MAC != replicaRecord.MAC || primaryRecord.PreviousMAC != f.oldCut.OrderingHeadMAC {
			t.Fatal("boundary did not extend same actual authenticated ordering", primaryRecord, replicaRecord)
		}
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		}
		for i, owner := range owners {
			if err := owner.Close(); err != nil {
				t.Fatal("actual owner close", i, err)
			}
			if budgets[i].Snapshot().Classes[resourcebudget.Recovery].RAM != 0 {
				t.Fatal("maintenance retained RAM after actual joins", budgets[i].Snapshot())
			}
		}
	}
	for i, cfg := range f.configs {
		raw, err := os.ReadFile(filepathMetadataJournal(cfg))
		if err != nil || !bytes.Equal(raw, f.oldMetadata[i]) {
			t.Fatal("legacy format1 history changed", err)
		}
	}
}
