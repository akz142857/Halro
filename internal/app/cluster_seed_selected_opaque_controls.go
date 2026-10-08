package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/replication"
)

var selectedSeedReceivedControls = [5]string{
	"cluster/state.json.received.json",
	"cluster/state.json.received.json.unqualified-0",
	"cluster/state.json.received.json.unqualified-1",
	"cluster/state.json.received.json.unqualified-2",
	"cluster/state.json.received.json.unqualified-3",
}

func selectedSeedOpaqueControlLimit(path string) (int64, bool) {
	if path == "ha-group-key-history" {
		return haGroupHistoryLimit, true
	}
	for _, name := range selectedSeedReceivedControls {
		if path == name {
			return replication.PortableReceivedDiskBytes, true
		}
	}
	return 0, false
}

// These exact bounded names were included in the independently approved tree.
// Their contents stay opaque: source Node/Ordering births never authorize the
// new Member. Remove the source controls from the active namespace before any
// target StatePublisher can consume them, preserving them in the source archive.
func preserveSelectedSeedReceivedControls(ctx context.Context, cfg config.Config, archive string) error {
	directory := filepath.Join(archive, "seed-received-controls")
	created := false
	for _, name := range selectedSeedReceivedControls {
		if err := preserveSelectedSeedOpaqueFile(ctx, filepath.Join(cfg.Storage.DataDir, name), directory, &created, replication.PortableReceivedDiskBytes); err != nil {
			return err
		}
	}
	return nil
}

func preserveSelectedSeedOpaqueFile(ctx context.Context, path, directory string, created *bool, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return errors.Join(errSeedMemoryStateAuthority, err)
	}
	if !*created {
		if err := os.Mkdir(directory, 0700); err != nil {
			return err
		}
		*created = true
	}
	if err := renameMigrationWithoutReplace(path, filepath.Join(directory, filepath.Base(path))); err != nil {
		return err
	}
	return errors.Join(durable.SyncDirectory(directory), durable.SyncDirectory(filepath.Dir(path)), durable.SyncDirectory(filepath.Dir(directory)))
}
