//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// E1 point 4 (board §40, §40.9): a selected backup materializes the Metadata
// root into a bbolt image in the staging directory beside its output. With the
// filesystem full except for a swept headroom at the start of that step, each
// backup either succeeds and verifies or fails whole: no staging or image is
// left, the step returns every fee it took, and the selected Metadata root is
// byte-for-byte unchanged. At least one headroom must meet ENOSPC inside the
// materialization itself.
func TestSelectedBackupMetadataImageAtActualENOSPC(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" || os.Getenv("HALRO_TERMINAL_ACTUAL_OS_PRESSURE") != "1" {
		t.Skip("requires explicit dedicated Linux filesystem <=512 MiB and the actual OS sampler")
	}
	var statfs unix.Statfs_t
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" || unix.Statfs(root, &statfs) != nil || statfs.Type != unix.EXT4_SUPER_MAGIC || uint64(statfs.Blocks)*uint64(statfs.Bsize) > 512<<20 {
		t.Fatal("refusing to exhaust filesystem outside explicit dedicated guard", statfs)
	}
	directory, err := os.MkdirTemp(root, "halro-e1-backup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	legacy, _, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(c *config.Config) {
		c.Storage.DataDir = filepath.Join(directory, "data")
		c.Storage.MasterKey.File = filepath.Join(directory, "master.key")
	})
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	limits := selectedRuntimeBudgetFixture()
	cfg.ResourceBudget = &limits
	pressure := physicalTerminalActualOSPressurePolicy()
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{AllowGeneratedSetupToken: true})
	if err != nil {
		t.Fatal("selected Runtime", err)
	}
	if r.selectedState == nil {
		t.Fatal("fixture is not a selected Runtime")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupKey := bytes.Repeat([]byte{0x45}, 32)
	outputDir := filepath.Join(directory, "out")
	if err := os.Mkdir(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadataRoot := filepath.Join(memoryStateDirectory(cfg), "metadata-root")
	digest := func() map[string][32]byte {
		files := map[string][32]byte{}
		if err := filepath.WalkDir(metadataRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			files[path] = sha256.Sum256(b)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := digest()
	if len(before) == 0 {
		t.Fatal("no selected Metadata root files", metadataRoot)
	}
	staged := func() []string {
		names, err := filepath.Glob(filepath.Join(outputDir, ".halro-backup-*"))
		if err != nil {
			t.Fatal(err)
		}
		return names
	}

	fillerPath := filepath.Join(directory, "foreign-filler")
	reached := 0
	for _, headroom := range []int64{0, 1, 2, 4, 8, 16, 32, 64, 128, 256} {
		archive := filepath.Join(outputDir, "selected.hmbk")
		var feesBefore, feesAfter resourcebudget.Cost
		var stepErr error
		done := false
		hook := selectedBackupMetadataPhase(func(phase string, budget *resourcebudget.Manager, err error) error {
			switch phase {
			case "materialize":
				feesBefore = budget.Snapshot().Used
				if _, err := fillForeignUntilENOSPC(fillerPath, int(statfs.Bsize)); err != nil {
					return errors.Join(errors.New("fill"), err)
				}
				info, err := os.Stat(fillerPath)
				if err != nil {
					return err
				}
				return os.Truncate(fillerPath, max(0, info.Size()-headroom*int64(statfs.Bsize)))
			case "materialize-done":
				feesAfter, stepErr, done = budget.Snapshot().Used, err, true
			}
			return nil
		})
		_, createErr := CreateBackupWithOptions(context.WithValue(ctx, selectedBackupMetadataPhaseKey{}, hook), cfg, configPath, archive, backupKey, CreateBackupOptions{})
		t.Log("headroom blocks", headroom, "ok", createErr == nil, "step reached", done, "step enospc", errors.Is(stepErr, syscall.ENOSPC), createErr)
		if err := os.Remove(fillerPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if left := staged(); len(left) != 0 {
			t.Fatal("backup left staging or image", headroom, left)
		}
		if got := digest(); len(got) != len(before) {
			t.Fatal("selected Metadata root changed", headroom)
		} else {
			for path, sum := range before {
				if got[path] != sum {
					t.Fatal("selected Metadata root changed", headroom, path)
				}
			}
		}
		if done && stepErr != nil && feesAfter != feesBefore {
			t.Fatal("failed materialization kept fees", headroom, feesBefore, feesAfter)
		}
		if createErr == nil {
			if _, err := VerifyBackup(archive, backupKey); err != nil {
				t.Fatal("backup at headroom does not verify", headroom, err)
			}
			if err := os.Remove(archive); err != nil {
				t.Fatal(err)
			}
		} else if _, err := os.Lstat(archive); !os.IsNotExist(err) {
			t.Fatal("failed backup published an archive", headroom, err)
		}
		if done && errors.Is(stepErr, syscall.ENOSPC) && errors.Is(createErr, syscall.ENOSPC) {
			reached++
		}
	}
	if reached == 0 {
		t.Fatal("no headroom met ENOSPC inside the Metadata materialization")
	}
	archive := filepath.Join(outputDir, "after-space.hmbk")
	if _, err := CreateBackupWithOptions(ctx, cfg, configPath, archive, backupKey, CreateBackupOptions{}); err != nil {
		t.Fatal("backup after space returned", err)
	}
	if _, err := VerifyBackup(archive, backupKey); err != nil {
		t.Fatal("backup verify after space returned", err)
	}
	t.Log("E1-P4 headrooms that met ENOSPC inside materialization", reached)
}
