package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func installSelectedSeedSnapshot(ctx context.Context, cfg config.Config, stage string, m SeedManifest) (result replication.MemberState, resultErr error) {
	resultErr = withSelectedSeedSampling(ctx, cfg, func(ctx context.Context) error {
		var err error
		result, err = installSelectedSeedSnapshotSampled(ctx, cfg, stage, m)
		return err
	})
	return result, resultErr
}

func installSelectedSeedSnapshotSampled(ctx context.Context, cfg config.Config, stage string, m SeedManifest) (result replication.MemberState, resultErr error) {
	ctx, _, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return result, err
	}
	stage = filepath.Clean(stage)
	target := filepath.Clean(cfg.Storage.DataDir)
	parent := filepath.Dir(target)
	if cfg.Replication == nil || cfg.ResourceBudget == nil || !filepath.IsAbs(stage) || stage == target || filepath.Dir(stage) != parent || m.Selected == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	pinfo, err := os.Lstat(parent)
	if err != nil || !pinfo.IsDir() || pinfo.Mode()&os.ModeSymlink != 0 {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	sinfo, err := os.Lstat(stage)
	if err != nil || !sinfo.IsDir() || sinfo.Mode()&os.ModeSymlink != 0 || !sameSeedFilesystem(pinfo, sinfo) {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	publication, err := lock.AcquireInitialization(target)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = retireSeedGuard(publication, resultErr) }()
	if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return result, errors.Join(os.ErrExist, err)
	}
	guard, err := lock.Acquire(stage)
	if err != nil {
		return result, err
	}
	joined := false
	defer func() {
		if !joined {
			resultErr = retireSeedGuard(guard, resultErr)
		}
	}()
	stageCfg := cfg
	stageCfg.Storage.DataDir = stage
	if err = checkSeedTreeContext(ctx, stage, false); err != nil {
		return result, err
	}
	pressure, err := commandPressure(ctx, cfg, nil)
	if err != nil {
		return result, err
	}
	manager := pressure.logicalBudget()
	routing := stageCfg
	repl := *cfg.Replication
	repl.NodeID = m.SourceNode
	repl.Peers = nil
	for _, peer := range m.Selected.SourceMember.Peers {
		repl.Peers = append(repl.Peers, config.ReplicationPeer{Name: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	routing.Replication = &repl
	master, unlock, err := unlockSelectedSeedMaster(ctx, routing, manager)
	if err != nil {
		return result, err
	}
	defer clear(master)
	if unlock != nil {
		defer func() { resultErr = errors.Join(resultErr, unlock.Close()) }()
	}
	sourceMember, key, err := readMemberState(routing, master)
	if err != nil {
		return result, err
	}
	defer clear(key[:])
	actualMember, memberErr := replication.MarshalState(sourceMember, key[:])
	expectedMember, expectedErr := replication.MarshalState(m.Selected.SourceMember, key[:])
	if memberErr != nil || expectedErr != nil || !bytes.Equal(actualMember, expectedMember) {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, memberErr, expectedErr)
	}
	if err = validateSelectedSeedDeclaration(cfg, m, key[:]); err != nil {
		return result, err
	}
	workspace, err := os.MkdirTemp(parent, "halro-seed-catalog-")
	if err != nil {
		return result, err
	}
	tree, err := hashSelectedSeedTree(ctx, cfg, stage, workspace, manager)
	err = errors.Join(err, os.Remove(workspace))
	if err != nil {
		return result, err
	}
	if tree != m.Selected.Tree {
		return result, fmt.Errorf("selected seed copied tree differs from actual approved source: %w", authenticatedstate.ErrSealIneligible)
	}
	result, err = buildSelectedSeedReplica(ctx, stageCfg, cfg, m, master, key[:], guard, manager)
	if err != nil {
		return result, err
	}
	if err = checkSeedTreeContext(ctx, stage, true); err != nil {
		return result, err
	}
	if err = guard.Close(); err != nil {
		return result, err
	}
	joined = true
	actual, err := os.Lstat(stage)
	named, ne := os.Lstat(parent)
	if err != nil || ne != nil || !os.SameFile(sinfo, actual) || !os.SameFile(pinfo, named) || !sameSeedFilesystem(actual, named) {
		return result, errors.Join(authenticatedstate.ErrSealIneligible, err, ne)
	}
	if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return result, errors.Join(os.ErrExist, err)
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = renameMigrationWithoutReplace(stage, target); err != nil {
		return result, err
	}
	if err = durable.SyncDirectory(parent); err != nil {
		return result, err
	}
	return result, selectedSeedFault(ctx, "namespace_published")
}

func unlockSelectedSeedMaster(ctx context.Context, cfg config.Config, manager *resourcebudget.Manager) ([]byte, *haMemoryUnlockPayload, error) {
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		return unlockHAMemoryCandidate(ctx, cfg, manager, defaultKMSWrapperFactory)
	}
	key, err := unlockMemberMasterKey(ctx, cfg)
	return key, nil, err
}
