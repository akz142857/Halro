package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/vault"
)

func TestSelectedKeyRotationFile(t *testing.T) {
	testSelectedKeyRotationFile(t, 0)
}

func TestSelectedKeyRotationFileBoundedLegacyMetadata(t *testing.T) {
	testSelectedKeyRotationFile(t, 1<<20)
}

func testSelectedKeyRotationFile(t *testing.T, legacyCeiling int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, _, _, sample := selectedLegacyExportFixture(t, ctx, true)
	if legacyCeiling > 0 {
		cfg.ResourceBudget.MemoryStateRequired = true
		cfg.ResourceBudget.LegacyMetadataFileDiskBytes = legacyCeiling
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := OpenWithOptions(ctx, cfg, log, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := unlockMasterKey(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(oldKey)
	v, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("selected rotation latest provider secret")
	c := domain.Credential{ID: "cred_selected_rotation_latest", Name: "latest", Type: domain.ProviderOpenAI, Audience: "https://api.openai.com", KeyVersion: 1, CreatedAt: time.Now().UTC()}
	c.Ciphertext, err = v.EncryptCredential(c.ID, string(c.Type), c.Audience, secret)
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
	if _, err := r.store.PutCredential(ctx, c, 0, nil); err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "prj_selected_rotation_latest", Name: "created after migration", Enabled: true}
	if _, err := r.store.PutProject(ctx, project, 0, nil); err != nil {
		t.Fatal(err)
	}
	oldNative := append([]byte(nil), r.selectedState.bootstrap.LedgerKey...)
	defer clear(oldNative)
	keyGeneration := r.selectedState.bootstrap.KeyGeneration
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	nextPath := filepath.Join(t.TempDir(), "new.key")
	if err := vault.InitMasterKey(nextPath); err != nil {
		t.Fatal(err)
	}
	result, err := rotateSelectedMasterKey(ctx, cfg, nextPath, nil, sample)
	if err != nil {
		t.Fatal(err)
	}
	if result.OldKeyVersion != 1 || result.NewKeyVersion != 2 {
		t.Fatal(result)
	}
	r, err = OpenWithOptions(ctx, cfg, log, OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal("ordinary Runtime startup audit after File rotation", err)
	}
	if r.selectedState == nil || r.selectedState.bootstrap.MetadataSlot != 1 || r.selectedState.bootstrap.KeyGeneration != keyGeneration || !bytes.Equal(oldNative, r.selectedState.bootstrap.LedgerKey) {
		t.Fatal("rotation changed native authority")
	}
	if _, err := r.store.GetProject(ctx, project.ID); err != nil {
		t.Fatal("latest project lost", err)
	}
	rotated, err := r.store.GetCredential(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	nextKey, err := vault.LoadMasterKey(nextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(nextKey)
	nv, err := vault.New(nextKey)
	if err != nil {
		t.Fatal(err)
	}
	defer nv.Close()
	plain, err := nv.DecryptCredential(rotated.ID, string(rotated.Type), rotated.Audience, rotated.Ciphertext)
	if err != nil || !bytes.Equal(plain, secret) || rotated.KeyVersion != 2 {
		t.Fatal("latest credential not rotated", err)
	}
	clear(plain)
	ov, _ := vault.New(oldKey)
	defer ov.Close()
	if _, err := ov.DecryptCredential(rotated.ID, string(rotated.Type), rotated.Audience, rotated.Ciphertext); err == nil {
		t.Fatal("old DEK decrypts current selected ciphertext")
	}
}

func TestSelectedKeyRotationPendingPublicationResumes(t *testing.T) {
	for _, phase := range []string{"selected_after_intent", "selected_after_build_root", "selected_after_build_epoch", "selected_before_seal", "selected_after_seal", "selected_after_bootstrap", "selected_after_unlock", "selected_after_key", "selected_after_native_archive", "selected_before_old_root_retirement", "selected_after_old_root_retirement", "selected_after_runtime_verification"} {
		t.Run(phase, func(t *testing.T) {
			stressPhase(t, phase, "selected_after_intent", "selected_after_seal", "selected_after_key", "selected_after_runtime_verification")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cfg, _, _, sample := selectedLegacyExportFixture(t, ctx, true)
			nextPath := filepath.Join(t.TempDir(), "next.key")
			if err := vault.InitMasterKey(nextPath); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("interrupt authorized rotation")
			if _, err := rotateSelectedMasterKey(ctx, cfg, nextPath, func(actual string) error {
				if actual == phase {
					return injected
				}
				return nil
			}, sample); !errors.Is(err, injected) {
				t.Fatal("did not stop at requested phase", err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			if r, err := OpenWithOptions(ctx, cfg, log, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample}); !errors.Is(err, ErrSelectedKeyRotationPending) || r != nil {
				if r != nil {
					r.Close()
				}
				t.Fatal("pending rotation allowed Runtime", err)
			}
			result, err := rotateSelectedMasterKey(ctx, cfg, nextPath, nil, sample)
			if err != nil || !result.RecoveredPending {
				t.Fatal("explicit same rotation did not resume", result, err)
			}
			r, err := OpenWithOptions(ctx, cfg, log, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal(err)
			}
			if r.selectedState == nil || r.selectedState.bootstrap.VaultVersion != 2 {
				t.Fatal("wrong resumed generation")
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSelectedKeyRotationHistorySlotsBoundFourRotations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, _, _, sample := selectedLegacyExportFixture(t, ctx, true)
	for generation := 0; generation < selectedRotationHistorySlots; generation++ {
		next := filepath.Join(t.TempDir(), "next.key")
		if err := vault.InitMasterKey(next); err != nil {
			t.Fatal(err)
		}
		result, err := rotateSelectedMasterKey(ctx, cfg, next, nil, sample)
		if err != nil || result.NewKeyVersion != uint64(generation+2) {
			t.Fatal(generation, result, err)
		}
		for slot := 0; slot <= generation; slot++ {
			journal, authority := selectedRotationHistoryPaths(cfg, uint8(slot))
			for _, path := range []string{journal, authority} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("lost old native fact owner", path, err)
				}
			}
		}
	}
	before, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(before)
	next := filepath.Join(t.TempDir(), "rejected.key")
	if err := vault.InitMasterKey(next); err != nil {
		t.Fatal(err)
	}
	if _, err := rotateSelectedMasterKey(ctx, cfg, next, nil, sample); err == nil {
		t.Fatal("fifth rotation admitted")
	}
	if _, err := os.Lstat(selectedRotationIntentPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight rejection wrote intent", err)
	}
	after, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("preflight changed active key", err)
	}
	clear(after)
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal("bounded history source no longer reopens", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedKeyRotationPreparationSlotsExhaustBeforeMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, _, _, sample := selectedLegacyExportFixture(t, ctx, true)
	next := filepath.Join(t.TempDir(), "next.key")
	if err := vault.InitMasterKey(next); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("interrupt before candidate creation")
	for attempt := 1; attempt < selectedRotationMetadataSlots; attempt++ {
		if _, err := rotateSelectedMasterKey(ctx, cfg, next, func(phase string) error {
			if phase == "selected_after_intent" {
				return injected
			}
			return nil
		}, sample); !errors.Is(err, injected) {
			t.Fatal("preparation did not stop", attempt, err)
		}
	}
	before, err := os.ReadFile(selectedRotationIntentPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	active, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(active)
	if _, err := rotateSelectedMasterKey(ctx, cfg, next, nil, sample); err == nil {
		t.Fatal("unbounded preparation admitted")
	}
	after, err := os.ReadFile(selectedRotationIntentPath(cfg))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("exhaustion changed authorization", err)
	}
	clear(before)
	clear(after)
	actual, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil || !bytes.Equal(active, actual) {
		t.Fatal("exhaustion changed source key", err)
	}
	clear(actual)
}
