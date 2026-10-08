package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	bbolt "go.etcd.io/bbolt"
)

// Build a schema-41 archive from the same native business facts, without the
// new empty fence bucket. The public encrypted Restore path must migrate that
// archive before publication while preserving its existing logical pricing hash.
func TestRestoreSchema41PricingArchiveMigratesEmptyRevisionFence(t *testing.T) {
	restoreLegacyPricingArchive(t, 41)
}

func TestRestoreSchema42PricingArchiveMigratesPendingCount(t *testing.T) {
	restoreLegacyPricingArchive(t, 42)
}

func TestRestoreSchema43PricingArchiveMigratesProposalRevision(t *testing.T) {
	restoreLegacyPricingArchive(t, 43)
}

func TestRestoreSchema44PricingArchiveMigratesProposalOrder(t *testing.T) {
	restoreLegacyPricingArchive(t, 44)
}

func restoreLegacyPricingArchive(t *testing.T, version uint64) {
	ctx := context.Background()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	boot, err := Bootstrap(ctx, cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "schema41", BillingMode: domain.BillingModeFree,
	}, []byte("schema41-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if version >= 42 {
		store, err := openMetadataForTest(t, cfg)
		if err != nil {
			t.Fatal(err)
		}
		intents := seedRuntimePricingPending(t, store, boot.DeploymentID, 3)
		if err := store.MarkPricingAuditIntentDelivered(ctx, intents[0].EventID); err != nil {
			t.Fatal(err)
		}
		if version >= 43 {
			dep, err := store.GetDeployment(ctx, boot.DeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Truncate(time.Second)
			p := domain.DeploymentPriceProposal{ID: "restore_proposal", DeploymentID: dep.ID, ProviderID: dep.ProviderID, ProviderModel: dep.ProviderModel, Region: dep.Region,
				BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1, InputMicrosPerMillion: 1, Match: domain.PriceProposalMatchExact, Status: domain.PriceProposalPending, CreatedBy: "system", CreatedAt: at, FetchedAt: at, ExpiresAt: at.Add(time.Hour), Revision: 1,
				Source: domain.PriceSource{Type: domain.PriceSourceOfficialURL, Assurance: domain.PriceAssuranceAsserted, ReceivedAt: at, RetrievedAt: &at, URI: "https://example.test/pricing", Reference: "restore", ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
			p.Digest, err = p.ComputeDigest()
			if err != nil {
				t.Fatal(err)
			}
			intent := domain.PricingAuditIntent{EventID: "restore_proposal_event", OccurredAt: at, ActorID: "system", Action: "deployment_price.proposal_create", TargetType: "deployment_price_proposal", TargetID: p.ID, RequestSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
			intent.RecordSource(p.Source)
			if _, _, _, err := store.CreateDeploymentPriceProposal(ctx, p, intent, "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x63}, 32)
	currentPath := filepath.Join(root, "current.hmbk")
	m, err := CreateBackup(ctx, cfg, configPath, currentPath, key)
	if err != nil {
		t.Fatal(err)
	}
	extract := filepath.Join(root, "extract")
	if _, err := backup.Extract(currentPath, key, extract); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(filepath.Join(extract, "data", "metadata.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		if version <= 42 {
			if err := tx.DeleteBucket([]byte("pricing_audit_control")); err != nil {
				return err
			}
		}
		if version <= 43 {
			if err := tx.DeleteBucket([]byte("pricing_proposal_revision")); err != nil {
				return err
			}
		}
		for _, name := range []string{"pricing_proposal_order", "pricing_proposal_count"} {
			if err := tx.DeleteBucket([]byte(name)); err != nil {
				return err
			}
		}
		if version == 41 {
			if err := tx.DeleteBucket([]byte("deployment_price_revision")); err != nil {
				return err
			}
		}
		var encoded [8]byte
		for v := version + 1; v <= boltstore.CurrentSchemaVersion(); v++ {
			binary.BigEndian.PutUint64(encoded[:], v)
			if err := tx.Bucket([]byte("migration_history")).Delete(encoded[:]); err != nil {
				return err
			}
		}
		binary.BigEndian.PutUint64(encoded[:], version)
		return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), encoded[:])
	})
	if err == nil {
		err = db.View(func(tx *bbolt.Tx) error { m.Metadata.TxID = uint64(tx.ID()); return nil })
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	files := make([]backup.SourceFile, 0, len(m.Files))
	for _, file := range m.Files {
		files = append(files, backup.SourceFile{ArchivePath: file.Path, LocalPath: filepath.Join(extract, file.Path)})
	}
	m.Metadata.SchemaVersion = version
	legacyPath := filepath.Join(root, fmt.Sprintf("schema%d.hmbk", version))
	legacy, err := backup.Create(backup.CreateOptions{
		OutputPath: legacyPath, BackupKey: key, Files: files, Metadata: m.Metadata,
		LedgerWatermark: m.LedgerWatermark, CheckpointWatermark: m.CheckpointWatermark,
		UsageManifestVersion: m.UsageManifestVersion, LedgerFeatureEpoch: m.LedgerFeatureEpoch,
		MinimumLedgerReaderVersion: m.MinimumLedgerReaderVersion,
		LedgerChainHeadSequence:    m.LedgerChainHeadSequence, LedgerChainHeadOffset: m.LedgerChainHeadOffset,
		LedgerChainHeadHash: m.LedgerChainHeadHash, LedgerChainVerified: m.LedgerChainVerified,
		GovernanceSequence: m.GovernanceSequence, GovernanceOffset: m.GovernanceOffset,
		GovernanceHeadHash: m.GovernanceHeadHash, GovernanceFormatVersion: m.GovernanceFormatVersion,
		PricingStateSHA256: m.PricingStateSHA256, PendingIntentSHA256: m.PendingIntentSHA256,
		PendingIntents: m.PendingIntents, MasterKeyFingerprint: m.MasterKeyFingerprint, Build: m.Build,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := RestoreBackup(ctx, cfg, legacyPath, key, legacy.BackupID)
	if err != nil || result.SchemaVersionBefore != version || result.SchemaVersionAfter != boltstore.CurrentSchemaVersion() {
		t.Fatal(result, err)
	}
	store, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wantPending := uint64(0)
	if version >= 42 {
		wantPending = 2
	}
	if version >= 43 {
		wantPending = 3
	}
	count, err := store.PendingPricingAuditIntentCount(ctx)
	if err != nil || count != wantPending {
		t.Fatal(count, wantPending, err)
	}
	if version >= 43 {
		page, err := store.DeploymentPriceProposalsPage(ctx, boot.DeploymentID, "", 10)
		wantRevision := uint64(0)
		if version == 44 {
			wantRevision = 1
		}
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != "restore_proposal" || page.Revision != wantRevision {
			t.Fatal(page, err)
		}
	}
	state, err := store.PricingBackupState()
	if err != nil || state.StateSHA256 != m.PricingStateSHA256 {
		t.Fatal(state, err)
	}
	price, err := store.SelectDeploymentPriceVersion(ctx, boot.DeploymentID, time.Now().UTC())
	if err != nil || price.DeploymentID != boot.DeploymentID {
		t.Fatal(price.ID, err)
	}
}
