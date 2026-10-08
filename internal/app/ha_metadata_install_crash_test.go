package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haInstallCrashParameters struct {
	Config                config.Config
	Keys                  HAMetadataKeys
	Source, Phase, Marker string
}

func TestHAMetadataInstallActualSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_HA_INSTALL_CRASH_PARAMETERS")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p haInstallCrashParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(p.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	_, err = installHAMetadataBaselineWithOptions(context.Background(), p.Config, p.Keys, p.Source, guard, functionalHAMetadataManager(t, p.Config), HAMetadataInstallOptions{}, func(phase string) error {
		if phase == p.Phase {
			if err := os.WriteFile(p.Marker, []byte(phase), 0600); err != nil {
				return err
			}
			select {}
		}
		return nil
	})
	t.Fatal("child did not stop at actual durable phase", err)
}

func TestHAMetadataInstallActualSIGKILLReimportsOrRequalifies(t *testing.T) {
	f, source := prepareHAMetadataInstallSource(t)
	for _, phase := range []string{"install-stage-created", "install-file-2-synced", "install-qualified-closed", "install-renamed", "install-published"} {
		t.Run(phase, func(t *testing.T) {
			cfg := cloneHAMetadataInstallTarget(t, f.configs[1])
			p := haInstallCrashParameters{Config: cfg, Keys: f.keys, Source: source, Phase: phase, Marker: filepath.Join(t.TempDir(), "actual-phase")}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			parameters := filepath.Join(t.TempDir(), "parameters.json")
			if err := os.WriteFile(parameters, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestHAMetadataInstallActualSIGKILLHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "HALRO_HA_INSTALL_CRASH_PARAMETERS="+parameters)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			deadline := time.Now().Add(30 * time.Second)
			for {
				if marker, err := os.ReadFile(p.Marker); err == nil && string(marker) == phase {
					break
				}
				select {
				case err := <-done:
					t.Fatal("child exited before actual phase", err, output.String())
				default:
				}
				if time.Now().After(deadline) {
					cmd.Process.Kill()
					<-done
					t.Fatal("actual phase timed out", output.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err == nil {
				t.Fatal("child did not receive SIGKILL")
			}
			final := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory)
			stage := final + ".preparing"
			prepublished := phase != "install-renamed" && phase != "install-published"
			var stageInfo os.FileInfo
			var captured [5][]byte
			if prepublished {
				stageInfo, err = os.Lstat(stage)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 5; i++ {
					captured[i], err = os.ReadFile(haMetadataInstalledFile(stage, i))
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}
			}
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			manager := functionalHAMetadataManager(t, cfg)
			installed, err := InstallHAMetadataBaselineWithOptions(context.Background(), cfg, f.keys, source, guard, manager, HAMetadataInstallOptions{Restart: true})
			if err != nil {
				t.Fatal("explicit real restart failed", err)
			}
			if err := installed.Close(); err != nil {
				t.Fatal(err)
			}
			if prepublished {
				preserved := final + ".unqualified-0"
				info, err := os.Lstat(preserved)
				if err != nil || !os.SameFile(stageInfo, info) {
					t.Fatal("opaque namespace was not preserved exactly", err)
				}
				for i, old := range captured {
					if old == nil {
						continue
					}
					raw, err := os.ReadFile(haMetadataInstalledFile(preserved, i))
					if err != nil || !bytes.Equal(old, raw) {
						t.Fatal("cross-process candidate bytes changed", i, err)
					}
				}
			}
			if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("restart left active preparation", err)
			}
			for i := 0; i < 5; i++ {
				want, err := os.ReadFile(haMetadataInstalledFile(source, i))
				if err != nil {
					t.Fatal(err)
				}
				actual, err := os.ReadFile(haMetadataInstalledFile(final, i))
				if err != nil || !bytes.Equal(want, actual) {
					t.Fatal("actual independently qualified final differs", i, err)
				}
			}
			if manager.Snapshot().Used.Disk <= cfg.ResourceBudget.MetadataStoreDiskBytes+cfg.ResourceBudget.MetadataJournalDiskBytes+maxHAMetadataCarrierBytes {
				t.Fatal("real inventory did not retain existing bytes")
			}
		})
	}
}

func TestHAMetadataInstallRestartPreservesForeignAndRefusesExhaustion(t *testing.T) {
	f, source := prepareHAMetadataInstallSource(t)
	for _, exhausted := range []bool{false, true} {
		t.Run(fmt.Sprint(exhausted), func(t *testing.T) {
			cfg := cloneHAMetadataInstallTarget(t, f.configs[1])
			final := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory)
			stage := final + ".preparing"
			if err := os.Mkdir(stage, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stage, "foreign"), []byte("opaque-fact"), 0600); err != nil {
				t.Fatal(err)
			}
			if exhausted {
				for i := 0; i < 4; i++ {
					if err := os.Mkdir(fmt.Sprintf("%s.unqualified-%d", final, i), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			installed, err := InstallHAMetadataBaselineWithOptions(context.Background(), cfg, f.keys, source, guard, functionalHAMetadataManager(t, cfg), HAMetadataInstallOptions{Restart: true})
			preserved := final + ".unqualified-0"
			if exhausted {
				if err == nil {
					t.Fatal("exhausted fixed slots admitted more growth")
				}
				preserved = stage
				if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("exhaustion published")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := installed.Close(); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(filepath.Join(preserved, "foreign"))
			if err != nil || string(raw) != "opaque-fact" {
				t.Fatal("foreign bytes read as authority or deleted", err)
			}
		})
	}
}
