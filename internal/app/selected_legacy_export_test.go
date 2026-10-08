package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
	"gopkg.in/yaml.v3"
)

func selectedLegacyExportFixture(t *testing.T, ctx context.Context, withRequest bool) (config.Config, BootstrapResult, string, resourcebudget.SampleSource) {
	t.Helper()
	r, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	cfg, instanceID := r.config, r.instanceID
	if withRequest {
		request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_before_legacy_export_migration", "chat")
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := r.accounting.ReserveAttempt(ctx, request, 20, 3)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 7, Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "source.yaml")
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "premigration.hmbk")
	archiveKey := bytes.Repeat([]byte{0x51}, 32)
	if _, err := CreateBackup(ctx, cfg, configPath, archivePath, archiveKey); err != nil {
		t.Fatal(err)
	}
	limits, pressure := selectedRuntimeBudgetFixture(), runtimePressureFixture()
	pressure.MaxAge = config.Duration(time.Minute)
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	manager, err := resourcebudget.New(limits.Limits(pressure.MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	old, err := openCommandMetadataReadOnly(ctx, configWithoutBudget(cfg), cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	master, err := unlockMasterKey(ctx, cfg, old)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	v, err := vault.New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	ledgerKey, err := loadLedgerHMACKey(old, v, master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(ledgerKey)
	journalKey, err := loadMetadataJournalHMACKey(old, v, master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(journalKey)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := backup.OpenVerified(ctx, archivePath, archiveKey)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	identity := sha256.Sum256([]byte("selected latest legacy export independent identity"))
	directory := memoryStateDirectory(cfg)
	if err := PrepareMemoryRuntimeMigrationDirectory(ctx, cfg, MemoryMigrationDirectoryOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: 1, SourceJournalKey: journalKey}, a, ledgerKey, manager, guard); err != nil {
		t.Fatal(err)
	}
	p, err := metadatastate.PrepareFormat2FromBackup(ctx, a, ledgerKey, journalKey, cfg.Storage.DataDir, directory, guard, metadatastate.BaselineOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: 1, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: 8 << 20}, limits.MetadataJournalDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	activated, err := ActivatePreparedMemoryRuntime(ctx, cfg, p, guard, manager, ledgerKey, journalKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := activated.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, credentials, configPath, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })
}

func TestSelectedLegacyExportPreservesLatestFactsAndIndependentRotation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, credentials, configPath, source := selectedLegacyExportFixture(t, ctx, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	options := OpenOptions{resourceSampleSource: source, AllowGeneratedSetupToken: true}
	r, err := OpenWithOptions(ctx, cfg, logger, options)
	if err != nil {
		t.Fatal(err)
	}
	if r.selectedState == nil {
		t.Fatal("source did not select activated generation")
	}
	liveOutput := filepath.Join(t.TempDir(), "busy-source")
	if result, err := exportSelectedLegacyWithSampleSource(ctx, cfg, configPath, liveOutput, source); err == nil || result.RuntimeVerified {
		t.Fatal("export bypassed real live Runtime exclusion", result, err)
	}
	if _, err := os.Lstat(filepath.Join(liveOutput, "verified-reader.json")); !os.IsNotExist(err) {
		t.Fatal("busy source produced a verified candidate", err)
	}
	project := domain.Project{ID: "prj_latest_export", Name: "created after migration", Enabled: true}
	if _, err := r.store.PutProject(ctx, project, 0, nil); err != nil {
		t.Fatal(err)
	}
	master, err := unlockMasterKey(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	v, err := vault.New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	secret := []byte("latest selected provider secret")
	credential := domain.Credential{ID: "cred_latest_export", Name: "Latest selected secret", Type: domain.ProviderOpenAI, Audience: "https://api.openai.com", KeyVersion: 1, CreatedAt: time.Now().UTC()}
	credential.Ciphertext, err = v.EncryptCredential(credential.ID, string(credential.Type), credential.Audience, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.PutCredential(ctx, credential, 0, nil); err != nil {
		t.Fatal(err)
	}
	outcome := assertSelectedRuntimeGovernance(t, ctx, r, credentials.ProjectID, credentials.KeyID)
	request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_after_migration_export", "chat")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := r.accounting.ReserveAttempt(ctx, request, 20, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 11, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		name          string
		configuration config.Config
		output        string
	}{
		{name: "source itself", configuration: cfg, output: cfg.Storage.DataDir},
		{name: "source descendant", configuration: cfg, output: filepath.Join(cfg.Storage.DataDir, "candidate")},
		{name: "HA", configuration: func() config.Config { c := cfg; c.Replication = &config.Replication{}; return c }(), output: filepath.Join(t.TempDir(), "ha")},
		{name: "KMS", configuration: func() config.Config { c := cfg; c.Storage.MasterKey.Mode = config.MasterKeyModeKeySlots; return c }(), output: filepath.Join(t.TempDir(), "kms")},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			if _, err := exportSelectedLegacyWithSampleSource(ctx, invalid.configuration, configPath, invalid.output, source); err == nil {
				t.Fatal("unqualified candidate accepted")
			}
		})
	}
	output := filepath.Join(t.TempDir(), "latest-current-reader")
	result, err := exportSelectedLegacyWithSampleSource(ctx, cfg, configPath, output, source)
	if err != nil {
		t.Fatal("latest export", err)
	}
	if !result.RuntimeVerified || result.BackupID == "" || result.SchemaVersion == 0 || result.LedgerSequence == 0 {
		t.Fatal(result)
	}
	target, err := config.Load(result.ConfigPath, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.Storage.MasterKey.File == cfg.Storage.MasterKey.File || target.Storage.DataDir == cfg.Storage.DataDir {
		t.Fatal("candidate shares live source ownership")
	}
	verify := func(target config.Config, expectedSecret []byte) {
		candidate, err := OpenWithOptions(ctx, target, logger, options)
		if err != nil {
			t.Fatal(err)
		}
		defer candidate.Close()
		if project, err := candidate.store.GetProject(ctx, project.ID); err != nil || project.Name != "created after migration" {
			t.Fatal("latest metadata lost", project, err)
		}
		stored, err := candidate.store.GetCredential(ctx, credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := candidate.vault.DecryptCredential(stored.ID, string(stored.Type), stored.Audience, stored.Ciphertext)
		defer clear(plain)
		if err != nil || !bytes.Equal(plain, expectedSecret) {
			t.Fatal("latest credential lost", err)
		}
		if summary, found, err := candidate.usage.disk.ReadRequest(ctx, request.RequestID); err != nil || !found || summary.CostMicrosUSD != 11 {
			t.Fatal("latest ledger/usage lost", summary, found, err)
		}
		snapshot, err := candidate.governance.manager.ReadOutcomePage(ctx, governance.OutcomePageQuery{ProjectID: credentials.ProjectID, WorkUnitID: outcome.WorkUnitID, DefinitionID: outcome.DefinitionID, Rows: 10, Bytes: 1 << 20})
		if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].ID != outcome.ID {
			t.Fatal("latest governance lost", snapshot, err)
		}
		if err := candidate.Close(); err != nil {
			t.Fatal(err)
		}
	}
	verify(target, secret)
	newKey := bytes.Repeat([]byte{0x65}, 32)
	newPath := filepath.Join(t.TempDir(), "rotation.key")
	if err := os.WriteFile(newPath, newKey, 0600); err != nil {
		t.Fatal(err)
	}
	rotationCtx, err := selectedExportCommandContext(ctx, target, source)
	if err != nil {
		t.Fatal(err)
	}
	interruption := errors.New("simulated power interruption")
	if _, err := rotateMasterKeyWithHook(rotationCtx, target, newPath, func(point string) error {
		if point == "after_metadata_publish" {
			return interruption
		}
		return nil
	}); !errors.Is(err, interruption) {
		t.Fatal("actual rotation interruption not exercised", err)
	}
	rotationCtx, err = selectedExportCommandContext(ctx, target, source)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := RotateMasterKey(rotationCtx, target, newPath)
	if err != nil || !rotated.RecoveredPending {
		t.Fatal("candidate rotation did not recover", rotated, err)
	}
	verify(target, secret)
	verify(cfg, secret)
	originalKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil || !bytes.Equal(originalKey, master) {
		t.Fatal("candidate rotation changed source key", err)
	}
	clear(originalKey)
	if _, err := exportSelectedLegacyWithSampleSource(ctx, cfg, configPath, output, source); err == nil {
		t.Fatal("existing export silently reused obsolete snapshot")
	}
}

func TestSelectedLegacyExportEmptyMigrationReopens(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, _, _, source := selectedLegacyExportFixture(t, ctx, false)
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{resourceSampleSource: source, AllowGeneratedSetupToken: true})
	if err != nil {
		t.Fatal("empty selected migration must reopen", err)
	}
	if r.selectedState == nil {
		t.Fatal("empty migration fell back to old metadata")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
