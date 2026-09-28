package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

// VerifyMemberTransitionSnapshot checks a frozen copy of one member's data
// directory. It reads only; the caller is responsible for an atomic snapshot
// or stopping the member before copying, and for archiving the file inventory.
func VerifyMemberTransitionSnapshot(ctx context.Context, cfg config.Config, snapshotDir string) (replication.TransitionSnapshotReport, error) {
	report, _, err := readMemberTransitionSnapshot(ctx, cfg, snapshotDir, nil)
	return report, err
}

// ReadMemberTransitionSnapshotPage authenticates a frozen member copy before
// reading its committed tail from an exact journal cursor.
func ReadMemberTransitionSnapshotPage(ctx context.Context, cfg config.Config, snapshotDir string, cursor replication.DurableTransitionCursor) (replication.TransitionSnapshotReport, replication.DurableTransitionPage, error) {
	return readMemberTransitionSnapshot(ctx, cfg, snapshotDir, &cursor)
}

func readMemberTransitionSnapshot(ctx context.Context, cfg config.Config, snapshotDir string, cursor *replication.DurableTransitionCursor) (replication.TransitionSnapshotReport, replication.DurableTransitionPage, error) {
	var report replication.TransitionSnapshotReport
	var page replication.DurableTransitionPage
	err := withFrozenMemberTransitionSnapshot(ctx, cfg, snapshotDir, func(snapshotCfg config.Config, state replication.MemberState, key []byte) error {
		var err error
		if cursor != nil {
			report, page, err = replication.ReadVerifiedTransitionSnapshotPage(snapshotCfg.ReplicationStatePath(), key, state, *cursor)
		} else {
			report, err = replication.VerifyTransitionSnapshot(snapshotCfg.ReplicationStatePath(), key, state)
		}
		return err
	})
	return report, page, err
}

func withFrozenMemberTransitionSnapshot(ctx context.Context, cfg config.Config, snapshotDir string,
	inspect func(config.Config, replication.MemberState, []byte) error) error {
	if cfg.Replication == nil || !filepath.IsAbs(snapshotDir) || filepath.Clean(snapshotDir) != snapshotDir {
		return errors.New("member transition snapshot requires replication configuration and a clean absolute snapshot directory")
	}
	info, err := os.Lstat(snapshotDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("member transition snapshot must be a private directory")
	}
	snapshotReal, err := filepath.EvalSymlinks(snapshotDir)
	if err != nil {
		return err
	}
	liveReal, err := filepath.EvalSymlinks(cfg.Storage.DataDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// An archived copy can be verified on a different host where the
		// original data directory is no longer mounted.
		liveReal = filepath.Clean(cfg.Storage.DataDir)
	}
	if snapshotReal == liveReal {
		return errors.New("member transition snapshot must be separate from the live data directory")
	}
	snapshotCfg := cfg
	snapshotCfg.Storage.DataDir = snapshotDir
	if err := replication.RequireMemberRuntime(snapshotDir, true, "verify transition snapshot"); err != nil {
		return err
	}
	masterKey, err := unlockMemberMasterKey(ctx, snapshotCfg)
	if err != nil {
		return err
	}
	defer clear(masterKey)
	state, clusterKey, err := readMemberState(snapshotCfg, masterKey)
	if err != nil {
		return err
	}
	defer clear(clusterKey[:])
	return inspect(snapshotCfg, state, clusterKey[:])
}
