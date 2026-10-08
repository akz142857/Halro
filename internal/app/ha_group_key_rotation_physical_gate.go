package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Existence grants no physical ownership. Until the actual group maintenance
// path can retire and rebind its complete vector, even a disabled configuration
// must refuse retained receipts before an opener can repair any source/root.
func rejectHAGroupPhysicalReceipts(cfg config.Config, guard *lock.Lock) error {
	if cfg.Replication == nil || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return authenticatedstate.ErrSealIneligible
	}
	roots := [5]string{
		filepath.Join(haMemoryStateDirectory(cfg), "ledger"),
		filepath.Join(haMemoryStateDirectory(cfg), "usage"),
		filepath.Join(haMemoryStateDirectory(cfg), "governance"),
		filepath.Join(haMemoryStateDirectory(cfg), "directory"),
		filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata-root"),
	}
	var paths [16]string
	at := 0
	for _, root := range roots {
		for bank := 0; bank < 2; bank++ {
			paths[at] = filepath.Join(root, fmt.Sprintf("terminal.physical.%d", bank))
			at++
		}
	}
	for _, native := range [2]string{cfg.AuditPath(), cfg.GovernancePath()} {
		for bank := 0; bank < 2; bank++ {
			paths[at] = fmt.Sprintf("%s.physical-window-%d", native, bank)
			at++
		}
	}
	paths[at], paths[at+1] = cfg.ReplicationStatePath()+".physical-bank", cfg.ReplicationStatePath()+".physical.json"
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		return fmt.Errorf("HA group key rotation retains physical preparation at %s; original qualified group retirement is required: %w", path, authenticatedstate.ErrRecovery)
	}
	return nil
}
