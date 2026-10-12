//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// E1 point 4 (board §40, §40.9, §40.13, §40.14): a selected backup materializes the Metadata
// root into a bbolt image in the staging directory beside its output. With the
// filesystem full except for a swept headroom at the start of that step, each
// backup either succeeds and verifies or fails whole: no staging or image is
// left, the step returns every fee it took, and the selected state reopens
// healthy with its Metadata and audit intact. ENOSPC inside the
// materialization itself is TestMetadataImageAtActualENOSPCRemovesImageAndFees.
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	probe := domain.Project{ID: "prj_e1_backup_probe", Name: "E1 backup probe", Enabled: true}
	r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true})
	if err != nil {
		t.Fatal("selected Runtime", err)
	}
	if r.selectedState == nil {
		t.Fatal("fixture is not a selected Runtime")
	}
	if probe, err = r.store.PutProject(ctx, probe, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// A successful backup also writes the Metadata root (its audit
	// checkpoint), so the selected state is judged by reopening it (§40.13).
	reopenHealthy := func(headroom int64) {
		r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true})
		if err != nil {
			t.Fatal("selected Runtime after failed backup", headroom, err)
		}
		got, getErr := r.store.GetProject(ctx, probe.ID)
		status := r.status.Load()
		if err := r.Close(); err != nil {
			t.Fatal("close after failed backup", headroom, err)
		}
		if getErr != nil || got.Name != probe.Name || got.Revision != probe.Revision || status != ledger.AccountingHealthy {
			t.Fatal("selected state after failed backup", headroom, got, getErr, status)
		}
		if _, err := VerifyAudit(ctx, cfg); err != nil {
			t.Fatal("audit after failed backup", headroom, err)
		}
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
		// Keep the filler between headrooms; refilling ~500 MiB with O_SYNC
		// each time outran the timeout (§40.14). 64 MiB is room to reopen.
		if info, err := os.Stat(fillerPath); err == nil {
			if err := os.Truncate(fillerPath, max(0, info.Size()-64<<20)); err != nil {
				t.Fatal(err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if left := staged(); len(left) != 0 {
			t.Fatal("backup left staging or image", headroom, left)
		}
		if createErr != nil {
			reopenHealthy(headroom)
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
	// ENOSPC inside the materialization is the component test's; here the
	// actual sampler refuses first (§40.14).
	if err := os.Remove(fillerPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
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
