package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	corekms "github.com/akz142857/Halro/internal/kms"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
	"gopkg.in/yaml.v3"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func selectedRotationKMSFixture(t *testing.T, ctx context.Context, harness *kmsAppHarness) (config.Config, resourcebudget.SampleSource) {
	t.Helper()

	cfg := enableLedgerSegmentIndexBudget(kmsAppTestConfig(t))
	if err := initializeKMS(ctx, cfg, kmsInitializationOptions{factory: harness.factory}); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true})
	if err != nil {
		t.Fatal(err)
	}
	instanceID := r.instanceID
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	cfg = configWithoutBudget(cfg)
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
	return cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })
}

func TestSelectedKeyRotationKMSRealRuntimeAndPendingResume(t *testing.T) {
	for _, phase := range []string{"", "selected_after_intent", "selected_after_build_root", "selected_before_seal", "selected_after_unlock", "selected_after_native_archive"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			harness := newKMSAppHarness(t)
			ctx = context.WithValue(ctx, selectedRotationKMSFactoryKey{}, kmsWrapperFactory(harness.factory))
			cfg, sample := selectedRotationKMSFixture(t, ctx, harness)
			r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal(err)
			}
			project := domain.Project{ID: "prj_kms_rotation_latest", Name: "selected latest KMS facts", Enabled: true}
			if _, err := r.store.PutProject(ctx, project, 0, nil); err != nil {
				t.Fatal(err)
			}
			oldKey, err := unlockMasterKey(ctx, cfg, r.store)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(oldKey)
			oldGeneration := r.selectedState.bootstrap.KeyGeneration
			oldNative := append([]byte(nil), r.selectedState.bootstrap.LedgerKey...)
			defer clear(oldNative)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("stop selected KMS rotation")
			options := kmsRotationOptions{factory: harness.factory, operationID: "selected-kms-rotation", random: bytes.NewReader(bytes.Repeat([]byte{0x9a}, 32)), hook: func(actual string) error {
				if phase != "" && actual == phase {
					return injected
				}
				return nil
			}}
			result, err := rotateSelectedKMSMasterKey(ctx, cfg, options, sample)
			if phase != "" {
				if !errors.Is(err, injected) {
					t.Fatal("did not interrupt", err)
				}
				options.hook = nil
				result, err = rotateSelectedKMSMasterKey(ctx, cfg, options, sample)
				if !result.RecoveredPending {
					t.Fatal("did not resume", result, err)
				}
			}
			if err != nil || result.NewKeyVersion != 2 {
				t.Fatal(result, err)
			}
			// Retrying a completed operation must inspect the current actual
			// authority without generating or wrapping another DEK.
			options.hook = nil
			beforeCalls := harness.callCount()
			wrapCount := func() int {
				count := 0
				for _, wrapper := range harness.wrappers {
					for _, call := range wrapper.Calls() {
						if call.Operation == corekms.OperationWrap {
							count++
						}
					}
				}
				return count
			}
			beforeWraps := wrapCount()
			completed, err := rotateSelectedKMSMasterKey(ctx, cfg, options, sample)
			if err != nil || !completed.RecoveredPending || completed.NewKeyVersion != 2 {
				t.Fatal("completed operation was not idempotent", completed, err)
			}
			if harness.callCount() <= beforeCalls {
				t.Fatal("completed retry skipped actual configured slot unlock")
			}
			if wrapCount() != beforeWraps {
				t.Fatal("completed retry wrapped another DEK")
			}
			r, err = OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
				t.Fatal("ordinary Runtime startup audit after KMS rotation", err)
			}
			if r.selectedState.bootstrap.KeyGeneration != oldGeneration || !bytes.Equal(oldNative, r.selectedState.bootstrap.LedgerKey) {
				t.Fatal("KMS rotation changed native authority")
			}
			if _, err := r.store.GetProject(ctx, project.ID); err != nil {
				t.Fatal("lost latest KMS fact", err)
			}
			descriptor, err := r.store.KeySlotDescriptor(ctx)
			if err != nil || !descriptor.ProductionReady() || descriptor.ActiveGeneration != 2 {
				t.Fatal(descriptor, err)
			}
			for _, purpose := range []masterkey.KeySlotPurpose{masterkey.KeySlotPrimary, masterkey.KeySlotRecovery} {
				key, err := unlockKMSMasterKey(ctx, cfg, r.store, purpose, harness.factory)
				if err != nil || bytes.Equal(key, oldKey) {
					t.Fatal("target slot not verified", purpose, err)
				}
				clear(key)
			}
		})
	}
}
