package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haPrepareCrashParameters struct {
	Config                  config.Config
	Keys                    HAMetadataKeys
	Archive                 string
	BackupKey               []byte
	Instance, Phase, Marker string
}

func TestHAMetadataPrepareActualSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_HA_PREPARE_CRASH_PARAMETERS")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p haPrepareCrashParameters
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(p.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	archive, err := backup.OpenVerified(context.Background(), p.Archive, p.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	o := haPrepareRestartOptions(p.Config, haMetadataMaintenanceFixture{instance: p.Instance}, t)
	_, err = prepareHAMetadataBaseline(context.Background(), p.Config, p.Keys, archive, filepath.Join(p.Config.Storage.DataDir, HAMetadataBaselineDirectory), guard, o, HAMetadataPrepareOptions{}, true, func(phase string) error {
		if phase == p.Phase {
			if err := os.WriteFile(p.Marker, []byte(phase), 0600); err != nil {
				return err
			}
			select {}
		}
		return nil
	})
	t.Fatal("actual producer did not stop at durable phase", err)
}

func haPrepareFileDigests(t *testing.T, directory string, excludeCandidate bool) map[string][32]byte {
	t.Helper()
	digests := make(map[string][32]byte)
	if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if excludeCandidate && strings.HasPrefix(rel, HAMetadataBaselineDirectory) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digests[rel] = sha256.Sum256(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return digests
}

func TestHAMetadataPrepareActualSIGKILLReauthenticatesFreshSource(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	archivePath, backupKey := haPrepareRestartArchive(t, f)
	for _, phase := range []string{"prepare-source-qualified", "prepare-before-workspace-create", "prepare-workspace-created", "prepare-candidate-created", "prepare-carrier-ready", "prepare-carrier-synced"} {
		t.Run(phase, func(t *testing.T) {
			cfg := cloneHAMetadataInstallTarget(t, f.configs[0])
			original := haPrepareFileDigests(t, cfg.Storage.DataDir, true)
			p := haPrepareCrashParameters{Config: cfg, Keys: f.keys, Archive: archivePath, BackupKey: backupKey, Instance: f.instance, Phase: phase, Marker: filepath.Join(t.TempDir(), "actual-phase")}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			parameters := filepath.Join(t.TempDir(), "parameters.json")
			if err := os.WriteFile(parameters, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestHAMetadataPrepareActualSIGKILLHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "HALRO_HA_PREPARE_CRASH_PARAMETERS="+parameters)
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
					t.Fatal("child exited before phase", err, output.String())
				default:
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					<-done
					t.Fatal("phase timed out", output.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err == nil {
				t.Fatal("child was not killed")
			}
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGKILL {
				t.Fatal("actual SIGKILL not established", cmd.ProcessState)
			}
			workspace := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory)
			interrupted, statErr := os.Lstat(workspace)
			var oldFiles map[string][32]byte
			if statErr == nil {
				oldFiles = haPrepareFileDigests(t, workspace, false)
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal(statErr)
			}
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			archive, err := backup.OpenVerified(context.Background(), archivePath, backupKey)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			o := haPrepareRestartOptions(cfg, f, t)
			prepared, err := PrepareHAMetadataBaselineWithOptions(context.Background(), cfg, f.keys, archive, workspace, guard, o, HAMetadataPrepareOptions{RestartSource: true})
			if err != nil {
				t.Fatal("explicit source restart failed", err)
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
			if interrupted != nil {
				preserved := workspace + ".source-unqualified-0"
				info, err := os.Lstat(preserved)
				if err != nil || !os.SameFile(interrupted, info) {
					t.Fatal("interrupted creator namespace not preserved", err)
				}
				if !reflect.DeepEqual(oldFiles, haPrepareFileDigests(t, preserved, false)) {
					t.Fatal("opaque candidate bytes changed")
				}
			} else if _, err := os.Lstat(workspace + ".source-unqualified-0"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invented preservation slot", err)
			}
			if !reflect.DeepEqual(original, haPrepareFileDigests(t, cfg.Storage.DataDir, true)) {
				t.Fatal("producer changed original native/member/ordering facts")
			}
			// Reopen through the real consumer: independently qualified carrier,
			// actual native prefix and all authenticated Metadata root rows.
			owner, err := OpenHAMetadataMaintenance(context.Background(), cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), slog.Default())
			if err != nil {
				t.Fatal("fresh restarted source cannot open actual maintenance", err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
