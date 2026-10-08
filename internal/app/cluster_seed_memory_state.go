package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
)

var errSeedMemoryStateAuthority = errors.New("legacy member seed/backup cannot carry memory-state: independent HA content-root and ordering authority is required")

// The legacy seed declaration hashes neither the authenticated content packs nor
// their bootstrap/selection controls. Its directory rename cannot grant those
// omitted files authority. Reject presence before opening or repairing native
// sources; do not parse a candidate header to manufacture the missing grant.
func refuseLegacySeedMemoryState(ctx context.Context, cfg config.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	present, err := memoryMigrationNamespacePresent(cfg)
	if err != nil {
		return err
	}
	if present {
		return errSeedMemoryStateAuthority
	}
	paths := []string{memoryStateBootstrapPath(cfg), filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory), filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory+".preparing"), filepath.Join(cfg.Storage.DataDir, "ha-memory-state"), filepath.Join(cfg.Storage.DataDir, "ha-memory-state.bootstrap"), filepath.Join(cfg.Storage.DataDir, "ha-memory-state.bootstrap.pending")}
	for i := 0; i < 4; i++ {
		paths = append(paths, filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("%s.unqualified-%d", HAMetadataBaselineDirectory, i)))
		paths = append(paths, filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("%s.source-unqualified-%d", HAMetadataBaselineDirectory, i)))
		paths = append(paths, filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("ha-memory-state.unqualified-%d", i)))
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return errSeedMemoryStateAuthority
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return ctx.Err()
}
