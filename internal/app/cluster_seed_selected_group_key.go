package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"os"
)

// The stable cluster-key carrier is node bound and contains no root/role
// authority. Rebinding first checks the original source node/key/incarnation;
// encoding then verifies the actual new target Member with that exact key.
func bindSelectedSeedClusterKey(ctx context.Context, cfg config.Config, m SeedManifest, master, key []byte, suffix string) error {
	_, err := os.Lstat(haGroupClusterKeyPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	source := cfg
	repl := *cfg.Replication
	repl.NodeID = m.SourceNode
	source.Replication = &repl
	original, incarnation, found, err := readHAGroupClusterKeyCandidate(source, master)
	defer clear(original[:])
	defer clear(incarnation[:])
	if err != nil || !found || incarnation != sha256.Sum256([]byte(m.Incarnation)) || len(key) != 32 {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	var targetKey [32]byte
	copy(targetKey[:], key)
	defer clear(targetKey[:])
	if original != targetKey {
		return authenticatedstate.ErrSealIneligible
	}
	raw, err := encodeHAGroupClusterKeyCandidate(cfg, targetKey, master)
	if err != nil {
		return err
	}
	defer clear(raw)
	path := haGroupClusterKeyPath(cfg)
	if err = renameMigrationWithoutReplace(path, path+suffix); err != nil {
		return err
	}
	if err = writeSelectedSeedBoundFile(ctx, path, raw); err != nil {
		return err
	}
	return durable.SyncDirectory(cfg.Storage.DataDir)
}
