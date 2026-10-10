//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

// Migration into the HA memory state at actual ENOSPC. At each activation
// phase a foreign file fills the isolated ext4, so every later write meets
// actual ENOSPC. The activation must either finish inside space it already
// holds or fail with that ENOSPC, never another error, and leave no owned
// temporary. Once the fill is freed, activation must complete (a restart when
// the failure left interrupted state, as after a crash) and the result must
// pass the independent seven-root and native audit read. Default activation
// must refuse state the failure interrupted, as after a crash.
func TestHAMemoryActivationAtActualENOSPCResumes(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires owned isolated ext4 fixture")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil || st.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 required", err)
	}
	t.Setenv("TMPDIR", root)
	for _, phase := range []string{"sources-qualified", "namespace-created", "seven-roots-built", "bootstrap-pending-synced", "common-seal-published", "bootstrap-activated"} {
		t.Run(phase, func(t *testing.T) {
			f := prepareHAMetadataMaintenanceFixture(t)
			prepareHAMetadataMaintenanceArtifacts(t, f)
			completeHAMemoryActivationBoundary(t, f)
			cfg := f.configs[0]
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			fill := filepath.Join(root, fmt.Sprintf("foreign-activation-fill-%s", phase))
			defer os.Remove(fill)
			filled := false
			a, err := activateHAMemoryRuntime(ctx, cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), logger, func(name string) error {
				if name != phase {
					return nil
				}
				filled = true
				return exhaustHAPhysicalFixture(fill, haPhysicalFixtureFillBound)
			})
			if !filled {
				t.Fatal("activation never reached the phase", err)
			}
			failed := err != nil
			if failed && !errors.Is(err, syscall.ENOSPC) {
				t.Fatal("activation at actual ENOSPC failed otherwise", err)
			}
			if !failed {
				if err := a.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if left := haPhysicalOwnedTemporaries(t, cfg.Storage.DataDir); len(left) != 0 {
				t.Fatal("activation at actual ENOSPC left owned temporaries", left)
			}
			if err := os.Remove(fill); err != nil {
				t.Fatal(err)
			}
			restarted := false
			if failed {
				manager := functionalHAMetadataManager(t, cfg)
				a, err = ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, manager, logger)
				// As after a crash, default activation must not overwrite the
				// interrupted state a failure after the namespace leaves.
				if err == nil && phase != "sources-qualified" {
					a.Close()
					t.Fatal("default activation overwrote state interrupted at ENOSPC")
				}
				if err != nil {
					restarted = true
					a, err = ActivateHAMemoryRuntimeWithOptions(ctx, cfg, f.keys, guard, manager, logger, HAMemoryActivationOptions{Restart: true})
				}
				if err != nil {
					t.Fatal("activation after space returned", err)
				}
				if err := a.Close(); err != nil {
					t.Fatal(err)
				}
			}
			fresh, reopened, err := openHAMemoryActivationRead(ctx, cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), logger, haMemoryStateBootstrapPath(cfg))
			if err != nil {
				t.Fatal("independent seven-root/native audit after ENOSPC", err)
			}
			if err := errors.Join(reopened.Close(), fresh.Close()); err != nil {
				t.Fatal(err)
			}
			t.Log(phase, "failed at ENOSPC", failed, "needed restart", restarted)
		})
	}
}
