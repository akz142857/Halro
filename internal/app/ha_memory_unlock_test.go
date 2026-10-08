package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func prepareHAKMSMetadataFixture(t *testing.T, ctx context.Context) (haMetadataMaintenanceFixture, *kmsAppHarness) {
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
	return prepareHAMetadataMaintenanceFixtureFromRuntime(t, source), harness
}

func TestHAMemoryUnlockActualCloseDebtRetainsOriginalFee(t *testing.T) {
	cfg := testConfig(t)
	limits := selectedRuntimeBudgetFixture()
	cfg.ResourceBudget = &limits
	manager := functionalHAMetadataManager(t, cfg)
	if err := os.MkdirAll(haMemoryStateDirectory(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	file, receipt, err := ownedfile.Create(haMemoryUnlockPath(cfg), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 4 * haMemoryUnlockLimit, Disk: haMemoryUnlockLimit})
	if err != nil {
		t.Fatal(err)
	}
	o := &haMemoryUnlockOwner{file: file, owner: receipt, fee: fee, raw: bytes.Repeat([]byte{1}, 1024)}
	before := manager.Snapshot().Used
	// A foreign close is known to the test but not this actual IO owner. Its
	// failed Close must keep the original FD reference and entire work debt.
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := o.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("unknown close was retired", err)
		}
		if o.file != file || len(o.raw) != 1024 || manager.Snapshot().Used != before {
			t.Fatal("unknown close dropped owner or fee")
		}
	}
}

func TestDefaultHASelectedKMSIndependentUnlockCarrierAndActualHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	metadataFixture, harness := prepareHAKMSMetadataFixture(t, ctx)
	fixture, old, _ := prepareHASelectedTransportFixtureFromMetadataFixture(t, ctx, metadataFixture)
	// The normal Runtime must work with a legacy Metadata directory trap and
	// no plaintext Master Key file, using the actual carrier producer output.
	for _, cfg := range fixture.configs {
		manager := functionalHAMetadataManager(t, cfg)
		before := manager.Snapshot().Used
		key, p, err := unlockHAMemoryCandidate(ctx, cfg, manager, harness.factory)
		if err != nil || !bytes.Equal(key, fixture.keys.Master) || p == nil {
			t.Fatal("actual independent candidate", err)
		}
		if cfg.Replication.NodeID == fixture.configs[0].Replication.NodeID {
			p.Descriptor.Revision++
			if err := p.validateSelected(ctx, old[0]); err == nil {
				t.Fatal("independent selected Metadata accepted a stale descriptor")
			}
			p.Descriptor.Revision--
			if err := p.validateSelected(ctx, old[0]); err != nil {
				t.Fatal("actual current descriptor refused", err)
			}
		}
		clear(key)
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if manager.Snapshot().Used != before {
			t.Fatal("known candidate read retained memory")
		}
	}
	cfg := fixture.configs[0]
	path := haMemoryUnlockPath(cfg)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := functionalHAMetadataManager(t, cfg)
	before := manager.Snapshot().Used
	calls := harness.callCount()
	wrongNode := cfg
	rc := *cfg.Replication
	rc.NodeID = "foreign-member"
	wrongNode.Replication = &rc
	if key, _, err := unlockHAMemoryCandidate(ctx, wrongNode, manager, harness.factory); err == nil || len(key) != 0 {
		t.Fatal("foreign member candidate accepted", err)
	}
	if harness.callCount() != calls {
		t.Fatal("foreign local member reached KMS")
	}
	wrongAllowed := cfg
	wrongAllowed.Storage.MasterKey.AllowedKMSKeys = append([]config.AllowedKMSKey(nil), cfg.Storage.MasterKey.AllowedKMSKeys...)
	wrongAllowed.Storage.MasterKey.AllowedKMSKeys[0].KeyID += "-foreign"
	if key, _, err := unlockHAMemoryCandidate(ctx, wrongAllowed, manager, harness.factory); err == nil || len(key) != 0 {
		t.Fatal("independent KMS allowlist mismatch accepted", err)
	}
	if harness.callCount() != calls {
		t.Fatal("allowlist mismatch reached KMS")
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if key, _, err := unlockHAMemoryCandidate(ctx, cfg, manager, harness.factory); err == nil || len(key) != 0 {
		t.Fatal("symlink candidate accepted", err)
	}
	if harness.callCount() != calls {
		t.Fatal("symlink reached KMS")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	p, envelope, err := decodeHAMemoryUnlock(original)
	if err != nil {
		t.Fatal(err)
	}
	p.Incarnation += "-tamper"
	envelope.Payload, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if key, _, err := unlockHAMemoryCandidate(ctx, cfg, manager, harness.factory); err == nil || len(key) != 0 {
		t.Fatal("tampered carrier MAC accepted", err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	clear(p.VaultCheck)
	if manager.Snapshot().Used != before {
		t.Fatal("refused independent candidates leaked fees")
	}
	proveDefaultHASelectedRuntime(t, ctx, fixture, old)
}
