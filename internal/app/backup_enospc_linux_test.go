//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// Fill only an explicitly selected isolated small tmpfs, after the real command
// has prepared its source/stage/index owners. The encrypted output then meets
// actual ENOSPC despite having enough logical quota. No provider is invoked.
func TestBudgetedBackupActualLinuxENOSPCRetiresOnlyOwnCiphertext(t *testing.T) {
	root := os.Getenv("HALRO_BACKUP_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated small Linux tmpfs")
	}
	named, err := os.Lstat(root)
	if !filepath.IsAbs(root) || err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
		t.Fatal("ENOSPC root must be an absolute real directory", err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.TMPFS_MAGIC || uint64(fs.Blocks)*uint64(fs.Bsize) > 32<<20 {
		t.Fatal("refusing to fill a non-tmpfs or a mount over 32MiB")
	}
	cfg := budgetedBackupOperationFixture(t)
	// Keep the production preflight active; the mount is intentionally tiny.
	// Exhaustion is introduced only after it passed, immediately before output IO.
	cfg.ResourcePressure.DiskEnterBytes, cfg.ResourcePressure.DiskExitBytes = 4096, 8192
	cfg.ResourcePressure.InodesEnter, cfg.ResourcePressure.InodesExit = 1, 4
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	archive := filepath.Join(outputDir, "refused.hmbk")
	foreign := filepath.Join(outputDir, "keep.foreign")
	foreignBytes := []byte("unrelated output must survive")
	if err := os.WriteFile(foreign, foreignBytes, 0600); err != nil {
		t.Fatal(err)
	}
	fillerPath := filepath.Join(t.TempDir(), "fill.tmpfs")
	var filled int64
	var nativeBefore [32]byte
	entered := false
	manifest, createErr := createBackupWithWriter(ctx, cfg, configPath, archive, bytes.Repeat([]byte{0x69}, 32), CreateBackupOptions{}, func(options backup.CreateOptions) (backup.Manifest, error) {
		entered = true
		native, err := os.ReadFile(cfg.LedgerPath())
		if err != nil {
			return backup.Manifest{}, err
		}
		nativeBefore = sha256.Sum256(native)
		reserve := options.ReserveOutputDisk
		if reserve == nil {
			return backup.Manifest{}, errors.New("actual encrypted output omitted shared reservation")
		}
		options.ReserveOutputDisk = func(size int64) (ledger.NativeFileDiskReservation, error) {
			token, err := reserve(size)
			if err != nil {
				return nil, err
			}
			filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return token, err
			}
			chunk := make([]byte, 64<<10)
			for {
				n, writeErr := filler.Write(chunk)
				filled += int64(n)
				if writeErr != nil {
					if !errors.Is(writeErr, syscall.ENOSPC) {
						return token, errors.Join(writeErr, filler.Close())
					}
					break
				}
				if filled > 32<<20 {
					return token, errors.Join(errors.New("dedicated mount exceeded guarded limit"), filler.Close())
				}
			}
			if err := errors.Join(filler.Sync(), filler.Close()); err != nil {
				return token, err
			}
			if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
				return token, errors.Join(err, errors.New("fixture did not exhaust actual blocks"))
			}
			return token, nil
		}
		result, err := backup.Create(options)
		// The library must close/unlink the captured failed output before space
		// returns. Restore space only for the command's original sources to close.
		if !errors.Is(err, syscall.ENOSPC) {
			t.Error("actual ciphertext did not encounter ENOSPC", err)
		}
		if tmp, globErr := filepath.Glob(filepath.Join(outputDir, ".halro-backup-*.tmp")); globErr != nil || len(tmp) != 0 {
			t.Error("known failed output retained a private inode", tmp, globErr)
		}
		return result, errors.Join(err, os.Remove(fillerPath))
	})
	if !entered || filled == 0 || !errors.Is(createErr, syscall.ENOSPC) || !reflect.DeepEqual(manifest, backup.Manifest{}) {
		t.Fatal("actual command accepted failed ciphertext", entered, filled, manifest, createErr)
	}
	var debt *backupOutputDiskFailure
	if !errors.As(createErr, &debt) || debt.owner == nil || !debt.owner.workRetired || !debt.owner.quota.closed || debt.owner.quota.consumed() != 0 || errors.Is(createErr, ownedfile.ErrRetirement) {
		t.Fatal("known failed output lost retirement/accounting outcome", createErr)
	}
	if used := pressure.logicalBudget().Snapshot(); used.Used.Disk < 2*cfg.ResourceBudget.BackupWorkspaceDiskBytes || used.Classes[resourcebudget.Recovery].RAM >= 1<<20 {
		t.Fatal("failed job lost retained ceiling or kept closed mappings/workspace", used)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatal("failed ciphertext was published", err)
	}
	if got, err := os.ReadFile(foreign); err != nil || !bytes.Equal(got, foreignBytes) {
		t.Fatal("failure removed unrelated output", err)
	}
	if native, err := os.ReadFile(cfg.LedgerPath()); err != nil || sha256.Sum256(native) != nativeBefore {
		t.Fatal("output ENOSPC changed original native accounting", err)
	}
	if partials, err := filepath.Glob(filepath.Join(outputDir, ".halro-backup-*")); err != nil || len(partials) != 0 {
		t.Fatal("known temporary backup objects were not retired", partials, err)
	}
	t.Logf("actual tmpfs ENOSPC: filled=%d, free_blocks_at_failure=%d, output_error=%v; source/stage/output shared one Manager; failed ciphertext retired before filler removal, native/foreign preserved", filled, fs.Bavail, createErr)
}
