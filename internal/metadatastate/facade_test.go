package metadatastate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestMetadataFacadePriceRecoveryNoOpPagesPreserveActualRevision(t *testing.T) {
	b, s, _ := metadataFacadeFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	// More than one recovery page, mixing already quarantined and coherent
	// high-water rows. Neither kind needs a successor transaction.
	if _, err := b.Update(ctx, func(tx *Transaction) error {
		for i := 0; i < 70; i++ {
			id := fmt.Sprintf("dep_%03d", i)
			high := domain.DeploymentPricingHighWater{DeploymentID: id, LatestObservedPriceVersionID: "price_" + id, LatestSelectedAt: now, LatestObservedEffectiveFrom: now, Revision: 1}
			if i%2 == 0 {
				high.Quarantined, high.QuarantineReason = true, "restored_high_water_incoherent"
			} else {
				price, err := json.Marshal(domain.DeploymentPriceVersion{DeploymentID: id, EffectiveFrom: now})
				if err != nil {
					return err
				}
				if err := tx.Put([]string{"deployment_price_versions"}, []byte(high.LatestObservedPriceVersionID), price); err != nil {
					return err
				}
			}
			if err := high.Validate(); err != nil {
				return err
			}
			raw, err := json.Marshal(high)
			if err != nil {
				return err
			}
			if err := tx.Put([]string{"deployment_pricing_high_water"}, []byte(id), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverDeploymentPricePins(ctx, ledger.NewState()); err != nil {
		t.Fatal("unchanged page fabricated a successor revision", err)
	}
	if after, err := b.store.RootIdentity(ctx); err != nil || after != before {
		t.Fatal("read-only recovery changed the actual authenticated root", after, before, err)
	}
}

func metadataFacadeFixture(t *testing.T) (*Baseline, *boltstore.Store, BaselineOptions) {
	t.Helper()
	b, o, _ := linkedDeltaCandidate(t)
	if _, err := b.Update(context.Background(), func(tx *Transaction) error { return tx.Delete([]string{"projects"}, []byte("fixture")) }); err != nil {
		t.Fatal(err)
	}
	selected, err := b.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend, err := b.StandaloneFacade(context.Background(), o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, selected)
	if err != nil {
		t.Fatal(err)
	}
	s, err := boltstore.NewFacadeStore(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return b, s, o
}

func TestMetadataFacadeExistingProjectAndGatewayDAOUsesNativeRootInsteadOfBbolt(t *testing.T) {
	b, s, _ := metadataFacadeFixture(t)
	legacy, err := boltstore.Open(filepath.Join(t.TempDir(), "oracle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.AttachMetadataJournal(bytes.Repeat([]byte{83}, 32), "oracle"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	project := domain.Project{ID: "prj_facade", Name: "Facade project", Enabled: true, AllowedModels: []string{"model"}, CreatedAt: now, UpdatedAt: now}
	want, err := legacy.PutProject(ctx, project, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.PutProject(ctx, project, 0, nil)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, want, err)
	}
	got, err = s.GetProject(ctx, project.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, want, err)
	}
	key := domain.GatewayKey{ID: "key_facade", ProjectID: project.ID, Name: "test key", HashVersion: 1, KeyHash: sha256.Sum256([]byte("independent-key")), Enabled: true, CreatedAt: now}
	wantKey, err := legacy.PutGatewayKey(ctx, key, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotKey, err := s.PutGatewayKey(ctx, key, 0, nil)
	if err != nil || !reflect.DeepEqual(gotKey, wantKey) {
		t.Fatal(gotKey, wantKey, err)
	}
	if gotKey, err = s.FindGatewayKeyByHash(ctx, key.KeyHash); err != nil || !reflect.DeepEqual(gotKey, wantKey) {
		t.Fatal("hash index diverged", gotKey, wantKey, err)
	}
	head := b.position
	if _, err := s.PutProject(ctx, project, 10, nil); !errors.Is(err, boltstore.ErrRevisionConflict) {
		t.Fatal("revision conflict accepted", err)
	}
	if b.position != head || s.RecoveryError() != nil {
		t.Fatal("business refusal changed head or retired DAO", b.position, head, s.RecoveryError())
	}
	projects, err := s.ListProjects(ctx)
	if err != nil || len(projects) != 1 || !reflect.DeepEqual(projects[0], want) {
		t.Fatal(projects, err)
	}
	position, err := s.ReadMetadataAppliedPosition(ctx)
	if err != nil || position.Sequence != head.Sequence || position.Transaction == 0 {
		t.Fatal(position, head, err)
	}
	state, err := s.MetadataJournalState()
	if err != nil || state.Path != b.journalPath || state.Sequence != head.Sequence || state.Applied != head.Sequence {
		t.Fatal(state, head, err)
	}
	if _, err := os.Stat(filepath.Join(b.output, "metadata.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("facade created hidden bbolt image", err)
	}
}

func TestMetadataFacadeOutcomeDefinitionUsesSuccessorTransactionFence(t *testing.T) {
	b, s, _ := metadataFacadeFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	project, err := s.PutProject(ctx, domain.Project{ID: "prj_fence", Name: "Transaction fence", Enabled: true, CreatedAt: now, UpdatedAt: now}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	definition := domain.OutcomeDefinition{ID: "odef_fence", ProjectID: project.ID, Name: "accepted", Version: 1, DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
	first, err := s.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil)
	if err != nil {
		t.Fatal("valid scan-to-writer fence", err)
	}
	definition.Version = 2
	second, err := s.PutOutcomeDefinition(ctx, definition, project.Revision, first.Revision, nil)
	if err != nil || second.Revision != first.Revision+1 {
		t.Fatal(second, err)
	}
	if got, err := s.GetOutcomeDefinition(ctx, project.ID, definition.ID, 2); err != nil || !reflect.DeepEqual(got, second) {
		t.Fatal(got, err)
	}
	before, err := b.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	definition.Version = 3
	if _, err := s.PutOutcomeDefinition(ctx, definition, project.Revision, first.Revision, nil); !errors.Is(err, boltstore.ErrRevisionConflict) {
		t.Fatal("stale definition fence accepted", err)
	}
	if after, err := b.AppliedHead(); err != nil || after != before || s.RecoveryError() != nil {
		t.Fatal("definite refusal advanced or retired healthy owner", after, before, err)
	}
}

func TestMetadataFacadeRuntimeBootstrapReadsActualKeyEnvelopesAndCompatibility(t *testing.T) {
	b, s, _ := metadataFacadeFixture(t)
	if _, err := b.Update(context.Background(), func(tx *Transaction) error {
		for _, name := range []string{"ledger_hmac_envelope", "metadata_hmac_envelope"} {
			if err := tx.Delete([]string{"meta"}, []byte(name)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		put   func([]byte) error
		get   func() ([]byte, error)
		value []byte
	}{
		{s.PutLedgerHMACEnvelope, s.LedgerHMACEnvelope, []byte("authenticated-ledger-envelope")},
		{s.PutAuditHMACEnvelope, s.AuditHMACEnvelope, []byte("authenticated-audit-envelope")},
		{s.PutMetadataJournalHMACEnvelope, s.MetadataJournalHMACEnvelope, []byte("authenticated-metadata-envelope")},
		{s.PutVaultKeyCheck, s.VaultKeyCheck, []byte("authenticated-vault-check")},
	} {
		if err := entry.put(entry.value); err != nil {
			t.Fatal(err)
		}
		got, err := entry.get()
		if err != nil || !bytes.Equal(got, entry.value) {
			t.Fatal(got, err)
		}
		got[0] ^= 1
		again, err := entry.get()
		if err != nil || !bytes.Equal(again, entry.value) {
			t.Fatal("escaped key envelope aliases root", err)
		}
	}
	keyring := boltstore.VaultKeyring{FormatVersion: 1, ActiveKeyVersion: 1, ActiveFingerprint: "sha256:" + strings.Repeat("3", 64)}
	raw, err := json.Marshal(keyring)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Update(context.Background(), func(tx *Transaction) error { return tx.Put([]string{"meta"}, []byte("vault_keyring"), raw) }); err != nil {
		t.Fatal(err)
	}
	gotKeyring, err := s.VaultKeyring()
	if err != nil || !reflect.DeepEqual(gotKeyring, keyring) {
		t.Fatal(gotKeyring, err)
	}
	id, err := s.SeedInstanceID("must-not-replace-independent-instance")
	if err != nil || id != "ins_baseline_test" {
		t.Fatal(id, err)
	}
	info, err := s.Info()
	if err != nil || info.SchemaVersion != boltstore.CurrentSchemaVersion() || info.MetadataJournalEpoch != b.position.Epoch || info.MetadataJournalSequence != b.position.Sequence || info.TxID == 0 {
		t.Fatal(info, err)
	}
	gate, err := s.LedgerCompatibilityGate()
	if err != nil || gate.MinimumReaderVersion != info.MinimumLedgerReaderVersion || gate.FeatureEpoch != info.LedgerFeatureEpoch {
		t.Fatal(gate, info, err)
	}
}

func TestMetadataFacadeRefusesRawMaintenanceAndUnselectedReference(t *testing.T) {
	b, s, o := metadataFacadeFixture(t)
	selected, err := b.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wrong := selected
	wrong.Root.Digest[0] ^= 1
	before := o.Budget.Snapshot().Used
	if backend, err := b.StandaloneFacade(context.Background(), o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, wrong); err == nil || backend != nil {
		t.Fatal("unselected facade", backend, err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("reference rejection changed fee")
	}
	output := filepath.Join(t.TempDir(), "must-not-exist.db")
	if _, err := s.SnapshotContext(context.Background(), output); !errors.Is(err, boltstore.ErrFacadeMaintenance) {
		t.Fatal(err)
	}
	if err := s.CompactSnapshotContext(context.Background(), output); !errors.Is(err, boltstore.ErrFacadeMaintenance) {
		t.Fatal(err)
	}
	if _, err := s.AttachMetadataJournal(bytes.Repeat([]byte{1}, 32), "replace"); !errors.Is(err, boltstore.ErrFacadeMaintenance) {
		t.Fatal(err)
	}
	if _, err := s.ApplyReplicaMetadataThrough(context.Background(), b.position.Epoch, b.position.Sequence); !errors.Is(err, boltstore.ErrFacadeMaintenance) {
		t.Fatal(err)
	}
	if _, err := s.TrimMetadataJournal(b.position.Sequence); !errors.Is(err, boltstore.ErrFacadeMaintenance) {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("raw maintenance created candidate", err)
	}
	if _, err := b.AppendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("still-healthy"), Value: []byte("value")}}); err != nil {
		t.Fatal(err)
	}
}

type heldMetadataFacade struct {
	boltstore.FacadeBackend
	entered, release chan struct{}
}

func (f *heldMetadataFacade) Update(ctx context.Context, visit func(boltstore.FacadeTransaction) error) (metadatajournal.Record, error) {
	return f.FacadeBackend.Update(ctx, func(tx boltstore.FacadeTransaction) error {
		if err := visit(tx); err != nil {
			return err
		}
		close(f.entered)
		<-tx.(*Transaction).Context().Done()
		<-f.release
		return nil
	})
}

func TestMetadataFacadeStoreCloseCancelsAndJoinsActualBusinessTransaction(t *testing.T) {
	b, o, _ := linkedDeltaCandidate(t)
	selected, err := b.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend, err := b.StandaloneFacade(context.Background(), o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, selected)
	if err != nil {
		t.Fatal(err)
	}
	held := &heldMetadataFacade{FacadeBackend: backend, entered: make(chan struct{}), release: make(chan struct{})}
	s, err := boltstore.NewFacadeStore(context.Background(), held)
	if err != nil {
		t.Fatal(err)
	}
	before := o.Budget.Snapshot().Used
	finished := make(chan error, 1)
	go func() {
		_, err := s.PutProject(context.Background(), domain.Project{ID: "prj_close", Name: "Close", Enabled: true}, 0, nil)
		finished <- err
	}()
	<-held.entered
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Store Close crossed callback", err)
	case <-time.After(30 * time.Millisecond):
	}
	if o.Budget.Snapshot().Used.RAM < before.RAM+TransactionRAMBytes {
		t.Fatal("Store Close refunded running callback")
	}
	close(held.release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if b.position.Sequence != 0 {
		t.Fatal("closed callback appended", b.position)
	}
	if _, err := s.GetProject(context.Background(), "prj_close"); err == nil {
		t.Fatal("closed DAO read admitted")
	}
}

func TestMetadataFacadeIntegrityFailureIsNeverAnAbsentBusinessRow(t *testing.T) {
	b, s, _ := metadataFacadeFixture(t)
	pages := filepath.Join(b.output, "pages")
	info, err := os.Stat(pages)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(pages)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Clone(original)
	corrupt[len(corrupt)-1] ^= 1
	if err := os.WriteFile(pages, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pages, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(pages, original, 0600)
	if _, err := s.GetProject(context.Background(), "absent"); err == nil || errors.Is(err, boltstore.ErrNotFound) {
		t.Fatal("integrity treated as absence", err)
	}
	if s.RecoveryError() == nil || b.ready.Load() {
		t.Fatal("business facade remained admitted", s.RecoveryError(), b.ready.Load())
	}
	if _, err := s.PutProject(context.Background(), domain.Project{ID: "prj_bad", Name: "Bad", Enabled: true}, 0, nil); !errors.Is(err, boltstore.ErrMetadataRecoveryRequired) {
		t.Fatal("business write after integrity failure", err)
	}
}

func TestMetadataFacadeBusyNativeOwnerIsUnavailableWithoutFalseRecovery(t *testing.T) {
	b, s, o := metadataFacadeFixture(t)
	ref, err := b.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend, err := b.StandaloneFacade(context.Background(), o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, ref)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := b.appendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("busy-actual-frame"), Value: []byte("value")}}, func(phase string) {
			if phase == "native-durable" {
				close(entered)
				<-release
			}
		})
		finished <- err
	}()
	<-entered
	if err := backend.HealthError(); !errors.Is(err, authenticatedstate.ErrBusy) {
		t.Fatal("locked actual native owner reported healthy", err)
	}
	if _, err := s.PutProject(context.Background(), domain.Project{ID: "prj_contention", Name: "Contended", Enabled: true}, 0, nil); !errors.Is(err, authenticatedstate.ErrBusy) {
		t.Fatal("contended DAO admitted", err)
	}
	if err := s.RecoveryError(); !errors.Is(err, authenticatedstate.ErrBusy) {
		t.Fatal("busy actual owner reported healthy/recovery", err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := backend.HealthError(); err != nil {
		t.Fatal("finished actual job unavailable", err)
	}
	if err := s.RecoveryError(); err != nil {
		t.Fatal("ordinary contention latched permanent recovery", err)
	}
	if _, err := s.PutProject(context.Background(), domain.Project{ID: "prj_after_contention", Name: "After contention", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataFacadeQualificationsCannotChangeIdentityOrNativeMode(t *testing.T) {
	b, _, o := metadataFacadeFixture(t)
	selected, err := b.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := o.Budget.Snapshot().Used
	for _, shape := range []string{"identity", "generation", "manager", "installed replication"} {
		identity, generation, manager := o.Identity, o.KeyGeneration, o.Budget
		switch shape {
		case "identity":
			identity[0] ^= 1
		case "generation":
			generation++
		case "manager":
			manager = baselineBudget(t)
		case "installed replication":
			// The actual bound-native API refuses an unqualified HA hook before
			// mutating mode. This refusal must not create an HA-owned facade.
			if err := b.journal.SetAfterDurable(func(metadatajournal.DurableBatch) (uint64, error) { return 1, nil }, func(context.Context, uint64) error { return nil }); err == nil {
				t.Fatal("unqualified bound HA hook installed")
			}
			if o.Budget.Snapshot().Used != before || !b.ready.Load() {
				t.Fatal("bound HA refusal changed owner/fees")
			}
			continue
		}
		if backend, err := b.StandaloneFacade(context.Background(), manager, identity, o.InstanceID, generation, selected); err == nil || backend != nil {
			t.Fatal(shape, backend, err)
		}
		if o.Budget.Snapshot().Used != before || !b.ready.Load() {
			t.Fatal("qualification changed actual healthy owner/fees", shape, o.Budget.Snapshot(), b.ready.Load())
		}
	}
}
