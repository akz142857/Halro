package app

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

func TestHAGroupVaultRewriteActualTLSAtomicProgressReplicaAndIdempotentRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	standalone, _, _ := openBootstrappedRuntime(t)
	oldKey, err := unlockMasterKey(ctx, standalone.config, standalone.store)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(oldKey)
	old, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	secret := []byte("same-group-provider-secret")
	credential := domain.Credential{ID: "cred_group_dek", Name: "Group", Type: domain.ProviderOpenAI, Audience: "https://api.openai.com", KeyVersion: 1, CreatedAt: time.Now().UTC()}
	credential.Ciphertext, err = old.EncryptCredential(credential.ID, string(credential.Type), credential.Audience, secret)
	if err != nil {
		t.Fatal(err)
	}
	credential, err = standalone.store.PutCredential(ctx, credential, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := prepareHAMetadataMaintenanceFixtureFromRuntime(t, standalone)
	fixture, owners, metadata := prepareHASelectedTransportFixtureFromMetadataFixture(t, ctx, f)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	joined := false
	for _, owner := range owners {
		go func(owner *haSelectedRuntimeFoundation) { done <- owner.serveSelectedTransport(runCtx) }(owner)
	}
	t.Cleanup(func() {
		stop()
		if !joined {
			for range 2 {
				if e := <-done; e != nil && !errors.Is(e, context.Canceled) {
					t.Error(e)
				}
			}
		}
	})
	for !owners[0].replication.manager.PeerConnections()[fixture.configs[1].Replication.NodeID] {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	nextKey := bytes.Repeat([]byte{89}, 32)
	next, err := vault.New(nextKey)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	ring, err := metadata[0].VaultKeyring()
	if err != nil {
		t.Fatal(err)
	}
	options, err := selectedVaultRewrite(ctx, old, next, ring.ActiveKeyVersion+1, keyFingerprint(nextKey), fixture.keys.Audit, fixture.keys.Ledger, fixture.keys.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	options.Keyring.PreviousFingerprint = ring.ActiveFingerprint
	options.Keyring.RotationOperationID = "group-vault-real-001"
	options.Keyring.RecoveryEnvelope, err = encryptRotationBridge(old, nextKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata[0].PreflightJournaledVaultRewrite(options); err != nil {
		t.Fatal("actual bounded secret preflight", err)
	}
	baseline := owners[0].replication.publisher.Snapshot().DurableIndex
	bad := options
	bad.Transform = func(c domain.Credential) (domain.Credential, error) { c.ID = "foreign"; return c, nil }
	if _, err := metadata[0].RewriteVaultMaterialJournaledStep(bad); !errors.Is(err, boltstore.ErrVaultRewriteIncomplete) {
		t.Fatal("foreign row transform admitted", err)
	}
	if _, found, err := metadata[0].JournaledVaultRewriteProgress(ctx); err != nil || found {
		t.Fatal("refused row committed progress", found, err)
	}
	if owners[0].replication.publisher.Snapshot().DurableIndex != baseline {
		t.Fatal("refused row appended native ordering")
	}
	if _, err := metadata[1].RewriteVaultMaterialJournaledStep(options); err == nil {
		t.Fatal("actual Replica accepted vault mutation")
	}
	var steps int
	for {
		progress, err := metadata[0].RewriteVaultMaterialJournaledStep(options)
		if err != nil {
			t.Fatal("actual native/Ordering vault step", steps, err)
		}
		index := owners[0].replication.publisher.Snapshot().DurableIndex
		if err := owners[0].replication.coordinator.WaitConfirmed(ctx, index); err != nil {
			t.Fatal(err)
		}
		for owners[1].replication.publisher.Snapshot().AppliedIndex < index {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(5 * time.Millisecond):
			}
		}
		primary, pf, err := metadata[0].JournaledVaultRewriteProgress(ctx)
		if err != nil || !pf {
			t.Fatal("actual progress source", err)
		}
		replica, rf, err := metadata[1].JournaledVaultRewriteProgress(ctx)
		if err != nil || !rf || !bytes.Equal(primary.After, replica.After) || primary.Stage != replica.Stage || primary.OperationID != replica.OperationID {
			t.Fatal("Replica progress differs from actual same native frame", primary, replica, err)
		}
		if record, err := owners[0].replication.journal.Record(index); err != nil || record.Kind != replication.KindData || record.Store != replication.StoreMetadata {
			t.Fatal("step bypassed actual Metadata Ordering", record, err)
		}
		if steps == 0 {
			foreign := options
			foreign.Keyring.RotationOperationID = "foreign-op"
			if _, err := metadata[0].RewriteVaultMaterialJournaledStep(foreign); !errors.Is(err, boltstore.ErrVaultRewriteIncomplete) {
				t.Fatal("foreign resume operation admitted", err)
			}
		}
		steps++
		if steps > 64 {
			t.Fatal("finite fixture rewrite did not finish")
		}
		if progress.Stage == 6 {
			break
		}
	}
	for n, store := range metadata {
		got, err := store.GetCredential(ctx, credential.ID)
		if err != nil {
			t.Fatal(n, err)
		}
		if got.KeyVersion != credential.KeyVersion+1 || got.Revision != credential.Revision {
			t.Fatal("credential version increment is not exact", got)
		}
		plain, err := next.DecryptCredential(got.ID, string(got.Type), got.Audience, got.Ciphertext)
		if err != nil || !bytes.Equal(plain, secret) {
			t.Fatal("actual Replica/new Vault secret", n, err)
		}
		clear(plain)
		if p, err := old.DecryptCredential(got.ID, string(got.Type), got.Audience, got.Ciphertext); err == nil {
			clear(p)
			t.Fatal("old DEK still decrypts actual credential")
		}
		ring, err := store.VaultKeyring()
		if err != nil || ring.ActiveFingerprint != keyFingerprint(nextKey) || len(ring.RecoveryEnvelope) != 0 || ring.PreviousFingerprint != "" {
			t.Fatal("final keyring", n, ring, err)
		}
	}
	index := owners[0].replication.publisher.Snapshot().DurableIndex
	if p, err := metadata[0].RewriteVaultMaterialJournaledStep(options); err != nil || p.Stage != 6 {
		t.Fatal("completed retry", p, err)
	}
	if owners[0].replication.publisher.Snapshot().DurableIndex != index {
		t.Fatal("completed retry duplicated native facts")
	}
	stop()
	for range 2 {
		if e := <-done; e != nil && !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	}
	joined = true
}
