package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type haThreeGroupFixture struct {
	configs  [3]config.Config
	keys     HAMetadataKeys
	instance string
	oldCut   replication.MemberState
}

func prepareHAThreeGroupFixture(t *testing.T, standalone *Runtime) haThreeGroupFixture {
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
	fixture := haThreeGroupFixture{keys: HAMetadataKeys{Master: master, Ledger: ledgerKey, Audit: auditKey, Governance: governanceKey, Metadata: metadataKey}, instance: standalone.instanceID}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}

	var listeners [3]net.Listener
	var certs [3]*x509.Certificate
	var certKeys [3]*ecdsa.PrivateKey
	var pins [3]string
	fixture.configs[0] = cfg
	for n := 1; n < 3; n++ {
		replica := cfg
		root := t.TempDir()
		replica.Storage.DataDir = filepath.Join(root, "data")
		copyTestTree(t, cfg.Storage.DataDir, replica.Storage.DataDir)
		if cfg.Storage.MasterKey.Mode == config.MasterKeyModeFile {
			replica.Storage.MasterKey.File = filepath.Join(root, "master.key")
			copyTestRegularFile(t, cfg.Storage.MasterKey.File, replica.Storage.MasterKey.File)
		}
		fixture.configs[n] = replica
	}
	ca, caKey := makeReplicationCertificate(t, nil, nil, true, "three-rotation-ca")
	for n := range listeners {
		listeners[n], err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		certs[n], certKeys[n] = makeReplicationCertificate(t, ca, caKey, false, fmt.Sprintf("halro-%d", n))
		pin := sha256.Sum256(certs[n].RawSubjectPublicKeyInfo)
		pins[n] = "sha256:" + hex.EncodeToString(pin[:])
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
	for n := range fixture.configs {
		c := &fixture.configs[n]
		c.ResourceBudget, c.ResourcePressure = &limits, &pressure
		c.Replication = &config.Replication{ClusterID: "production-three", NodeID: fmt.Sprintf("halro-%d", n), Listen: listeners[n].Addr().String(), TLS: writeReplicationTLSFiles(t, t.TempDir(), ca, certs[n], certKeys[n])}
		for peer := range fixture.configs {
			if peer != n {
				c.Replication.Peers = append(c.Replication.Peers, config.ReplicationPeer{Name: fmt.Sprintf("halro-%d", peer), Address: listeners[peer].Addr().String(), SPKISHA256: pins[peer]})
			}
		}
		role := replication.RoleReplica
		if n == 0 {
			role = replication.RolePrimary
		}
		if err := EstablishMemberState(ctx, *c, role, "inc_ha_three_rotation", 7); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var runtimes [3]*Runtime
	for n, c := range fixture.configs {
		runtimes[n], err = Open(ctx, c, logger)
		if err != nil {
			t.Fatal("actual legacy three-member open", n, err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 3)
	for n, r := range runtimes {
		go func(r *Runtime, l net.Listener) { done <- r.replication.serve(runCtx, l) }(r, listeners[n])
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		primary := runtimes[0].replication.publisher.Snapshot()
		full := primary.DurableIndex > 0 && primary.DurableIndex == primary.ConfirmedIndex
		for _, r := range runtimes[1:] {
			full = full && r.replication.publisher.Snapshot().AppliedIndex == primary.DurableIndex
		}
		if full {
			fixture.oldCut = primary
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("actual three-member legacy TLS did not confirm/apply")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	for range 3 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	for _, r := range runtimes {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}
func prepareHAThreeGroupArtifacts(t *testing.T, f haThreeGroupFixture) {
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
	guards := [3]*lock.Lock{}
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
	for n := 1; n < 3; n++ {
		installed, err := InstallHAMetadataBaseline(ctx, f.configs[n], f.keys, directory, guards[n], functionalHAMetadataManager(t, f.configs[n]))
		if err != nil {
			t.Fatal("actual three-member independently qualified Replica install", n, err)
		}
		if err := installed.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
func completeHAThreeGroupBoundary(t *testing.T, f haThreeGroupFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var guards [3]*lock.Lock
	var owners [3]*HAMetadataMaintenance
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i, cfg := range f.configs {
		var err error
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer guards[i].Close()
		owners[i], err = OpenHAMetadataMaintenance(ctx, cfg, f.keys, guards[i], functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		defer owners[i].Close()
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 3)
	complete := make(chan replication.MemberState, 3)
	for _, owner := range owners {
		go func(m *HAMetadataMaintenance) {
			done <- m.Run(runCtx, func(s replication.MemberState) error { complete <- s; return nil })
		}(owner)
	}
	for range 3 {
		select {
		case state := <-complete:
			if state.DurableIndex != f.oldCut.DurableIndex+1 || state.ConfirmedIndex != state.DurableIndex || state.AppliedIndex != state.DurableIndex {
				t.Fatal("actual group boundary differs", state)
			}
		case err := <-done:
			t.Fatal("maintenance exited before actual boundary", err)
		case <-ctx.Done():
			t.Fatal("actual HA boundary did not complete", ctx.Err())
		}
	}
	stop()
	for range 3 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	for _, owner := range owners {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
func prepareHAThreeKMSGroupFixture(t *testing.T, ctx context.Context) (haThreeGroupFixture, *kmsAppHarness) {
	t.Helper()
	source, _, _ := openBootstrappedRuntime(t)
	master, err := unlockMasterKey(ctx, source.config, source.store)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	harness := newKMSAppHarness(t)
	oldFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = oldFactory })
	kmsCfg := kmsAppTestConfig(t)
	fingerprint, err := masterkey.MasterKeyFingerprint(master)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := masterkey.NewKeySlotDescriptor(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.store.PutKeySlotDescriptor(ctx, descriptor); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, slot := range []struct {
		id      string
		purpose masterkey.KeySlotPurpose
	}{{kmsCfg.Storage.MasterKey.PrimarySlot, masterkey.KeySlotPrimary}, {kmsCfg.Storage.MasterKey.RecoverySlot, masterkey.KeySlotRecovery}} {
		pending, err := wrapPendingSlot(ctx, kmsCfg.Storage.MasterKey, harness.factory, source.instanceID, slot.id, slot.purpose, master)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, _, err = source.store.AddKeySlot(ctx, pending, descriptor.Revision, now)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, _, err = source.store.VerifyKeySlot(ctx, slot.id, descriptor.Revision, 1, kmsSlotUnwrapper{masterKey: kmsCfg.Storage.MasterKey, factory: harness.factory}, vaultCandidateVerifier{store: source.store}, now.Add(time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
	}
	source.config.Storage.MasterKey = kmsCfg.Storage.MasterKey
	return prepareHAThreeGroupFixture(t, source), harness
}

func TestHAGroupRotationActualThreeConfiguredMembersFileAndKMS(t *testing.T) {
	for _, mode := range []string{"file", "key_slots"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
			defer cancel()
			var f haThreeGroupFixture
			var h *kmsAppHarness
			if mode == "file" {
				r, _, _ := openBootstrappedRuntime(t)
				f = prepareHAThreeGroupFixture(t, r)
			} else {
				f, h = prepareHAThreeKMSGroupFixture(t, ctx)
			}
			prepareHAThreeGroupArtifacts(t, f)
			completeHAThreeGroupBoundary(t, f)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			var before [3]haMemoryStateBootstrap
			for n, cfg := range f.configs {
				guard, err := lock.Acquire(cfg.Storage.DataDir)
				if err != nil {
					t.Fatal(err)
				}
				manager := functionalHAMetadataManager(t, cfg)
				active, err := ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, manager, logger)
				if err != nil {
					t.Fatal("real three member activation", n, err)
				}
				if err := active.Close(); err != nil {
					t.Fatal(err)
				}
				owner, err := openHASelectedRuntimeFoundation(ctx, cfg, f.keys.Master, guard, manager, logger)
				if err != nil {
					t.Fatal(err)
				}
				if err := owner.prepareSelectedRoots(ctx); err != nil {
					t.Fatal(err)
				}
				before[n] = owner.bootstrap
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
				if err := guard.Close(); err != nil {
					t.Fatal(err)
				}
			}
			next := bytes.Repeat([]byte{133}, 32)
			var result HAGroupKeyRotationResult
			var err error
			if mode == "file" {
				target := filepath.Join(t.TempDir(), "next.key")
				if err := os.WriteFile(target, next, 0600); err != nil {
					t.Fatal(err)
				}
				result, err = rotateHAGroupMasterKeyWithPressureFactory(ctx, f.configs[:], target, "three-configured-group", nil, functionalHAGroupPressure)
			} else {
				o := kmsRotationOptions{operationID: "three-configured-group", factory: h.factory, random: bytes.NewReader(next), now: time.Now}
				result, err = rotateHAGroupKeyWithOptions(ctx, f.configs[:], "", o.operationID, nil, functionalHAGroupPressure, &o)
			}
			if err != nil || result.Members != 3 || result.NewFingerprint != keyFingerprint(next) || result.RecoveredPending {
				t.Fatal("actual full configured three-member rotation", result, err)
			}
			var primary replication.MemberState
			for n, cfg := range f.configs {
				actual, key, err := readMemberState(cfg, next)
				clear(key[:])
				if err != nil {
					t.Fatal(n, err)
				}
				if n == 0 {
					primary = actual
				}
				if len(actual.Peers) != 2 || actual.Role != []replication.Role{replication.RolePrimary, replication.RoleReplica, replication.RoleReplica}[n] || actual.Term != 7 || actual.DurableIndex != primary.DurableIndex || actual.ConfirmedIndex != actual.DurableIndex || actual.AppliedIndex != actual.DurableIndex || actual.OrderingHeadMAC != primary.OrderingHeadMAC {
					t.Fatal("one configured replica omitted from confirmation", n, actual, primary)
				}
				if _, old, err := readMemberState(cfg, f.keys.Master); err == nil {
					clear(old[:])
					t.Fatal("old ordinary master reopened member", n)
				}
				guard, err := lock.Acquire(cfg.Storage.DataDir)
				if err != nil {
					t.Fatal(err)
				}
				manager := functionalHAMetadataManager(t, cfg)
				fresh, err := openHASelectedRuntimeFoundation(ctx, cfg, next, guard, manager, logger)
				if err != nil {
					t.Fatal("actual fresh three-member factory", n, err)
				}
				if err := fresh.prepareSelectedRoots(ctx); err != nil {
					t.Fatal(err)
				}
				fresh.metadataStore, err = fresh.reopened.OpenHAMetadataStore(ctx)
				if err != nil {
					t.Fatal(err)
				}
				ring, err := fresh.metadataStore.VaultKeyring()
				if err != nil || ring.ActiveFingerprint != keyFingerprint(next) || ring.ActiveKeyVersion != result.NewKeyVersion || ring.PreviousFingerprint != "" {
					t.Fatal("latest vault data not selected", n, ring, err)
				}
				if fresh.bootstrap.Binding != before[n].Binding || fresh.bootstrap.Evidence != before[n].Evidence {
					t.Fatal("DEK change rewrote independent native identity", n)
				}
				if mode == "key_slots" {
					key, p, err := unlockHAMemoryCandidate(ctx, cfg, manager, h.factory)
					if err != nil || !bytes.Equal(key, next) {
						t.Fatal(err)
					}
					clear(key)
					if err := p.validateSelected(ctx, fresh); err != nil {
						t.Fatal(err)
					}
					if err := p.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if err := fresh.Close(); err != nil {
					t.Fatal(err)
				}
				if err := guard.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
