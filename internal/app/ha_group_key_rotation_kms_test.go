package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

func prepareHAGroupKMSColdMembers(t *testing.T, ctx context.Context) (haMetadataMaintenanceFixture, []*haSelectedRuntimeFoundation, *kmsAppHarness) {
	t.Helper()
	f, h := prepareHAKMSMetadataFixture(t, ctx)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	completeHAMemoryActivationBoundary(t, f)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var owners []*haSelectedRuntimeFoundation
	for _, cfg := range f.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		manager := functionalHAMetadataManager(t, cfg)
		a, err := ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, manager, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		owner, err := openHASelectedRuntimeFoundation(ctx, cfg, f.keys.Master, guard, manager, logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := owner.prepareSelectedRoots(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := owner.Close(); err != nil {
				t.Error(err)
			}
			if err := guard.Close(); err != nil {
				t.Error(err)
			}
		})
		owners = append(owners, owner)
	}
	return f, owners, h
}

func TestHAGroupKMSRotationActualTLSBothSlotsSevenRootsAndFreshCandidate(t *testing.T) {
	proveHAGroupKMSRotation(t, false)
}
func TestHAGroupKMSRotationActualDefaultRuntimeAndHTTPBusiness(t *testing.T) {
	proveHAGroupKMSRotation(t, true)
}
func proveHAGroupKMSRotation(t *testing.T, business bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, owners, h := prepareHAGroupKMSColdMembers(t, ctx)
	var source [2]haMemoryStateBootstrap
	for n, o := range owners {
		source[n] = o.bootstrap
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
		if err := o.owners.Components.Guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	next := bytes.Repeat([]byte{112}, 32)
	options := kmsRotationOptions{operationID: "kms-real-group-001", factory: h.factory, random: bytes.NewReader(next), now: time.Now}
	result, err := rotateHAGroupKeyWithOptions(ctx, f.configs[:], "", options.operationID, nil, functionalHAGroupPressure, &options)
	if err != nil {
		t.Fatal("actual KMS native/Ordering/all-common transaction", err)
	}
	if result.Members != 2 || result.NewFingerprint != keyFingerprint(next) || result.NewKeyVersion != result.OldKeyVersion+1 {
		t.Fatal(result)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for n, cfg := range f.configs {
		if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
			t.Fatal("KMS scope changed")
		}
		manager := functionalHAMetadataManager(t, cfg)
		actual, p, err := unlockHAMemoryCandidate(ctx, cfg, manager, h.factory)
		if err != nil {
			t.Fatal(n, err)
		}
		if !bytes.Equal(actual, next) {
			t.Fatal("candidate did not unwrap actual new key", n)
		}
		member, key, err := readMemberState(cfg, actual)
		clear(key[:])
		if err != nil {
			t.Fatal(err)
		}
		if member.Role != []replication.Role{replication.RolePrimary, replication.RoleReplica}[n] || member.DurableIndex != member.AppliedIndex || member.ConfirmedIndex != member.AppliedIndex {
			t.Fatal(member)
		}
		if _, key, err := readMemberState(cfg, f.keys.Master); err == nil {
			clear(key[:])
			t.Fatal("old DEK admitted new candidate")
		}
		for _, purpose := range []masterkey.KeySlotPurpose{masterkey.KeySlotPrimary, masterkey.KeySlotRecovery} {
			found := false
			for _, slot := range p.Descriptor.Slots {
				if slot.Purpose != purpose || slot.State != masterkey.KeySlotActive {
					continue
				}
				found = true
				recovered, err := (kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: h.factory}).Unwrap(ctx, slot)
				if err != nil || !bytes.Equal(recovered, next) {
					t.Fatal("actual trusted slot", purpose, err)
				}
				if err := (envelopeCandidateVerifier{envelope: p.VaultCheck}).VerifyCandidate(ctx, recovered); err != nil {
					t.Fatal(err)
				}
				clear(recovered)
			}
			if !found {
				t.Fatal("missing production slot", purpose)
			}
		}
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := openHASelectedRuntimeFoundation(ctx, cfg, actual, guard, manager, logger)
		if err != nil {
			t.Fatal("ordinary fresh factory", n, err)
		}
		if err := fresh.prepareSelectedRoots(ctx); err != nil {
			t.Fatal(err)
		}
		fresh.metadataStore, err = fresh.reopened.OpenHAMetadataStore(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.validateSelected(ctx, fresh); err != nil {
			t.Fatal("candidate differs from actual selected Metadata", err)
		}
		if fresh.bootstrap.Binding != source[n].Binding || fresh.bootstrap.Evidence != source[n].Evidence {
			t.Fatal("DEK change forged native/baseline generation")
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		clear(actual)
		if err := fresh.Close(); err != nil {
			t.Fatal(err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if business {
		for _, cfg := range f.configs {
			if err := os.Rename(cfg.MetadataPath(), cfg.MetadataPath()+".preserved-group-kms"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(cfg.MetadataPath(), 0700); err != nil {
				t.Fatal(err)
			}
		}
		f.keys.Master = bytes.Clone(next)
		proveDefaultHASelectedRuntime(t, ctx, f, [2]*haSelectedRuntimeFoundation{owners[0], owners[1]})
	}

}
