package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

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

// MemberTransitionReadbackReport compares two separately read, MAC-verified
// frozen copies. It is local byte-equality evidence, not an immutable storage
// receipt or proof that the copies occupy different failure domains.
type MemberTransitionReadbackReport struct {
	Version              int                                  `json:"version"`
	Status               string                               `json:"status"`
	SourceDir            string                               `json:"source_dir"`
	ReadbackDir          string                               `json:"readback_dir"`
	InventorySHA256      string                               `json:"inventory_sha256"`
	SourceVerification   replication.TransitionSnapshotReport `json:"source_verification"`
	ReadbackVerification replication.TransitionSnapshotReport `json:"readback_verification"`
}

// VerifyMemberTransitionSnapshotReadback re-authenticates each frozen copy
// with the member Master Key, compares its exact file inventory and re-reads
// both copies. Both directories must be frozen before this read-only operation.
func VerifyMemberTransitionSnapshotReadback(ctx context.Context, cfg config.Config, sourceDir, readbackDir string) (MemberTransitionReadbackReport, error) {
	if sourceDir == readbackDir {
		return MemberTransitionReadbackReport{}, errors.New("member archive readback must be a separate frozen copy")
	}
	source, err := VerifyMemberTransitionSnapshot(ctx, cfg, sourceDir)
	if err != nil {
		return MemberTransitionReadbackReport{}, fmt.Errorf("verify source member snapshot: %w", err)
	}
	readback, err := VerifyMemberTransitionSnapshot(ctx, cfg, readbackDir)
	if err != nil {
		return MemberTransitionReadbackReport{}, fmt.Errorf("verify readback member snapshot: %w", err)
	}
	if !reflect.DeepEqual(source, readback) {
		return MemberTransitionReadbackReport{}, errors.New("member archive readback differs from source file inventory or committed history")
	}
	sourceCfg, readbackCfg := cfg, cfg
	sourceCfg.Storage.DataDir = sourceDir
	readbackCfg.Storage.DataDir = readbackDir
	for _, file := range source.Files {
		sourceInfo, err := os.Stat(filepath.Join(sourceCfg.ClusterDirectoryPath(), file.Name))
		if err != nil {
			return MemberTransitionReadbackReport{}, err
		}
		readbackInfo, err := os.Stat(filepath.Join(readbackCfg.ClusterDirectoryPath(), file.Name))
		if err != nil {
			return MemberTransitionReadbackReport{}, err
		}
		if os.SameFile(sourceInfo, readbackInfo) {
			return MemberTransitionReadbackReport{}, fmt.Errorf("member archive readback file %q is the source file or a hard link", file.Name)
		}
	}
	sourceAgain, err := VerifyMemberTransitionSnapshot(ctx, cfg, sourceDir)
	if err != nil || !reflect.DeepEqual(source, sourceAgain) {
		return MemberTransitionReadbackReport{}, errors.New("source member snapshot changed during archive readback verification")
	}
	readbackAgain, err := VerifyMemberTransitionSnapshot(ctx, cfg, readbackDir)
	if err != nil || !reflect.DeepEqual(readback, readbackAgain) {
		return MemberTransitionReadbackReport{}, errors.New("readback member snapshot changed during verification")
	}
	return MemberTransitionReadbackReport{Version: 1, Status: "member_readback_mac_and_bytes_match_local_only",
		SourceDir: sourceDir, ReadbackDir: readbackDir, InventorySHA256: source.InventorySHA256,
		SourceVerification: source, ReadbackVerification: readback}, nil
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
