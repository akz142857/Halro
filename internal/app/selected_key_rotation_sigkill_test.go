package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/vault"
)

func TestSelectedKeyRotationSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_SELECTED_ROTATION_CHILD_CONFIG")
	if path == "" {
		t.Skip("subprocess only")
	}
	cfg, err := config.Load(path, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	phase, marker := os.Getenv("HALRO_SELECTED_ROTATION_CHILD_PHASE"), os.Getenv("HALRO_SELECTED_ROTATION_CHILD_MARKER")
	sample := pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })
	beforeSeal := false
	ctx := authenticatedstate.WithPhysicalPreparationObserver(context.Background(), func(actual string) error {
		if !beforeSeal || actual != os.Getenv("HALRO_SELECTED_ROTATION_CHILD_PHYSICAL_PHASE") {
			return nil
		}
		return selectedRotationChildPause(marker, actual)
	})
	_, err = rotateSelectedMasterKey(ctx, cfg, os.Getenv("HALRO_SELECTED_ROTATION_CHILD_KEY"), func(actual string) error {
		if actual == "selected_before_seal" {
			beforeSeal = true
		}
		if actual != phase {
			return nil
		}
		return selectedRotationChildPause(marker, actual)
	}, sample)
	t.Fatal("child escaped requested kill boundary", err)
}

func selectedRotationChildPause(marker, phase string) error {
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(phase); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	for {
		time.Sleep(time.Minute)
	}
}

func TestSelectedKeyRotationSIGKILLResumesActualRuntime(t *testing.T) {
	for _, phase := range []string{"selected_after_intent", "selected_after_build_root", "selected_after_build_epoch", "selected_before_seal", "selected_after_seal", "selected_after_bootstrap", "selected_after_unlock", "selected_after_key", "selected_after_native_archive", "selected_before_old_root_retirement", "selected_after_old_root_retirement"} {
		t.Run(phase, func(t *testing.T) {
			stressPhase(t, phase, "selected_after_intent", "selected_after_seal", "selected_after_key", "selected_after_old_root_retirement")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cfg, _, cfgPath, sample := selectedLegacyExportFixture(t, ctx, true)
			next := filepath.Join(t.TempDir(), "next.key")
			if err := vault.InitMasterKey(next); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "reached")
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSelectedKeyRotationSIGKILLChild$", "-test.count=1")
			command.Env = append(os.Environ(), "HALRO_SELECTED_ROTATION_CHILD_CONFIG="+cfgPath, "HALRO_SELECTED_ROTATION_CHILD_PHASE="+phase, "HALRO_SELECTED_ROTATION_CHILD_MARKER="+marker, "HALRO_SELECTED_ROTATION_CHILD_KEY="+next)
			command.Stdout, command.Stderr = io.Discard, io.Discard
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Lstat(marker); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatal("child exited before kill boundary", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err == nil {
				t.Fatal("kill did not terminate child")
			}
			// The create-to-receipt gap grants no child deletion permission. Even
			// an extra foreign file in this incomplete root remains intact while
			// the same explicit rotation exclusively builds a fresh finite slot.
			opaqueRoot, opaqueJournal := memoryStateMetadataPaths(cfg, 1)
			opaque := map[string][32]byte{}
			if phase == "selected_before_old_root_retirement" {
				// The killed process had joined every borrowed source FD and
				// durably announced retirement. Reproduce a stop after one actual
				// known source unlink; the next process has no child inode receipt.
				opaqueRoot, _ = memoryStateMetadataPaths(cfg, 0)
				entries, err := os.ReadDir(opaqueRoot)
				if err != nil || len(entries) != 3 {
					t.Fatal("source namespace is not fully captured", err)
				}
				removed := ""
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), "pages") {
						removed = entry.Name()
					}
				}
				if removed == "" {
					t.Fatal("actual first retirement page pack is missing")
				}
				if err := os.Remove(filepath.Join(opaqueRoot, removed)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(opaqueRoot, "foreign.data"), []byte("preserve partial retired namespace"), 0600); err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Name() == removed {
						continue
					}
					path := filepath.Join(opaqueRoot, entry.Name())
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					opaque[path] = sha256.Sum256(raw)
				}
				foreign := filepath.Join(opaqueRoot, "foreign.data")
				opaque[foreign] = sha256.Sum256([]byte("preserve partial retired namespace"))
			}
			if phase == "selected_after_build_root" || phase == "selected_after_build_epoch" {
				if err := os.WriteFile(filepath.Join(opaqueRoot, "foreign.data"), []byte("preserve opaque facts"), 0600); err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(opaqueRoot)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					path := filepath.Join(opaqueRoot, entry.Name())
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					opaque[path] = sha256.Sum256(raw)
				}
				if raw, err := os.ReadFile(opaqueJournal); err == nil {
					opaque[opaqueJournal] = sha256.Sum256(raw)
				}
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			if r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample}); r != nil || !errors.Is(err, ErrSelectedKeyRotationPending) {
				if r != nil {
					r.Close()
				}
				t.Fatal("killed pending operation permitted ordinary startup", err)
			}
			result, err := rotateSelectedMasterKey(ctx, cfg, next, nil, sample)
			if err != nil || !result.RecoveredPending {
				t.Fatal("actual killed operation did not recover", result, err)
			}
			r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
				r.Close()
				t.Fatal("ordinary Runtime startup audit after killed rotation", err)
			}
			if r.selectedState == nil || r.selectedState.bootstrap.VaultVersion != 2 {
				t.Fatal("wrong actual reopened generation")
			}
			if phase == "selected_before_old_root_retirement" {
				history, capsule := selectedRotationHistoryPaths(cfg, 0)
				i, err := loadSelectedRotationHistory(cfg, capsule, r.vault, nil, r.selectedState.bootstrap)
				if err != nil || !i.SourceRootRetirementStarted || i.OldRootRetired || i.OpaqueMetadataSlots&1 == 0 {
					t.Fatal("uncertain partial source was claimed retired", err)
				}
				if err := verifySelectedRotationArchivedNative(ctx, cfg, history, i, r.selectedState.bootstrap.LedgerKey, r.resourcePressure.logicalBudget()); err != nil {
					t.Fatal("raw old facts lost", err)
				}
				clearRotationIntentKeys(i)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			for path, digest := range opaque {
				raw, err := os.ReadFile(path)
				if err != nil || sha256.Sum256(raw) != digest {
					t.Fatal("opaque candidate or foreign file changed", path, err)
				}
			}
		})
	}
}
