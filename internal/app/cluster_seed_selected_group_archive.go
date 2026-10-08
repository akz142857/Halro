package app

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"os"
	"path/filepath"
)

// A completed source key operation is path/node bound to its original group.
// The independent approved tree permits copying its bytes, never treating that
// receipt as a target operation. Preserve those fixed controls as opaque data.
func preserveSelectedSeedGroupControls(ctx context.Context, cfg config.Config, archive string) error {
	directory := filepath.Join(archive, "seed-group-controls")
	created := false
	for slot := range haGroupRotationIntentSlots {
		for _, suffix := range [2]string{"", ".complete"} {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := haGroupRotationIntentPath(cfg.Storage.DataDir, slot) + suffix
			_, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !created {
				if err = os.Mkdir(directory, 0700); err != nil {
					return err
				}
				created = true
			}
			if err = renameMigrationWithoutReplace(path, filepath.Join(directory, filepath.Base(path))); err != nil {
				return err
			}
			if err = durable.SyncDirectory(directory); err != nil {
				return err
			}
		}
	}
	if err := preserveSelectedSeedOpaqueFile(ctx, haGroupHistoryPath(cfg), directory, &created, haGroupHistoryLimit); err != nil {
		return err
	}
	if created {
		return durable.SyncDirectory(cfg.Storage.DataDir)
	}
	return nil
}

func selectedSeedGroupControlName(name string) bool {
	for slot := range haGroupRotationIntentSlots {
		base := filepath.Base(haGroupRotationIntentPath("", slot))
		if name == base || name == base+".complete" {
			return true
		}
	}
	return false
}
