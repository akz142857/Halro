package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

const haMemoryActivationOpaqueSlots = 4

// Preserve only an explicitly retried, unpublished activation. Nothing in an
// old directory, cache or pending envelope is read or used as authority. A
// partially completed preservation stays opaque; the next retry uses a fresh
// fixed slot and preserves whatever reserved names remain.
func preserveHAMemoryActivationCandidates(ctx context.Context, cfg config.Config, m *HAMetadataMaintenance, ledgerKey []byte, guard *lock.Lock, manager *resourcebudget.Manager) (resourcebudget.Permit, error) {
	if guard == nil || manager == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return resourcebudget.Permit{}, authenticatedstate.ErrSealIneligible
	}
	if _, _, err := qualifyHAMemoryActivationSources(ctx, m, manager, ledgerKey); err != nil {
		return resourcebudget.Permit{}, err
	}
	if _, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, "active.seal")); !os.IsNotExist(err) {
		return resourcebudget.Permit{}, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if _, err := os.Lstat(haMemoryStateBootstrapPath(cfg)); !os.IsNotExist(err) {
		return resourcebudget.Permit{}, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	candidates := []struct {
		path, name string
		info       os.FileInfo
	}{
		{path: haMemoryStateDirectory(cfg), name: "roots"},
		{path: haMemoryStateBootstrapPath(cfg) + ".pending", name: "bootstrap.pending"},
		{path: filepath.Join(cfg.Storage.DataDir, "active.seal.pending"), name: "seal.pending"},
		{path: filepath.Join(cfg.Storage.DataDir, "memory-audit-1.cache"), name: "audit.cache"},
		{path: filepath.Join(cfg.Storage.DataDir, "memory-governance-1.cache"), name: "governance.cache"},
	}
	anyCandidate := false
	for i := range candidates {
		if err := validateRuntimeDerivedPath(ctx, cfg, candidates[i].path); err != nil {
			return resourcebudget.Permit{}, err
		}
		info, err := os.Lstat(candidates[i].path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return resourcebudget.Permit{}, err
		}
		// Foreign contents remain opaque; reserved namespace names still must
		// be private regular files/directories, never symlink traversal.
		if info.Mode().Perm()&0077 != 0 || !(info.Mode().IsRegular() || i == 0 && info.IsDir()) {
			return resourcebudget.Permit{}, ownedfile.ErrReplaced
		}
		candidates[i].info = info
		anyCandidate = true
	}
	if !anyCandidate {
		return resourcebudget.Permit{}, nil
	}
	var opaque string
	for i := 0; i < haMemoryActivationOpaqueSlots; i++ {
		path := filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("ha-memory-state.unqualified-%d", i))
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			opaque = path
			break
		} else if err != nil {
			return resourcebudget.Permit{}, err
		}
	}
	if opaque == "" {
		return resourcebudget.Permit{}, resourcebudget.ErrExhausted
	}
	fee, err := manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: 4096})
	if err != nil {
		return resourcebudget.Permit{}, err
	}
	if err := os.Mkdir(opaque, 0700); err != nil {
		return resourcebudget.Permit{}, errors.Join(err, fee.Release())
	}
	// The directory has become persistent. Every subsequent outcome retains
	// its actual inventory/fee; no cancellation or failed rename deletes it.
	parent, err := os.Lstat(opaque)
	if err != nil {
		return fee, err
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return fee, err
	}
	for _, candidate := range candidates {
		if candidate.info == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fee, err
		}
		actual, err := os.Lstat(candidate.path)
		if err != nil || !os.SameFile(candidate.info, actual) {
			return fee, errors.Join(ownedfile.ErrReplaced, err)
		}
		named, err := os.Lstat(opaque)
		if err != nil || !named.IsDir() || !os.SameFile(parent, named) {
			return fee, errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := renameMigrationWithoutReplace(candidate.path, filepath.Join(opaque, candidate.name)); err != nil {
			return fee, err
		}
		if err := durable.SyncDirectory(opaque); err != nil {
			return fee, err
		}
		if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
			return fee, err
		}
	}
	return fee, nil
}
