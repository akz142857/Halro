package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func replicaRuntimeProjectionPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "replica-ledger-projection.db")
}

// Called before transport or public readers start, under the data-directory
// lock. A prior derived file supplies no facts/authority. The generation alone
// owns both DBs; Runtime aliases borrow them until query and transport drain.
func prepareReplicaRuntimeOwner(ctx context.Context, cfg config.Config, member *replicationRuntime, native *ledger.Log, metadata *boltstore.Store, pressure *resourcePressureRuntime) (result *replicaAccountingGeneration, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Replication == nil || member == nil || member.role != replication.RoleReplica || member.publisher == nil || native == nil || metadata == nil {
		return nil, errors.New("Replica startup requires authenticated member/native/Metadata owners")
	}
	state := member.publisher.Snapshot()
	if state.Role != replication.RoleReplica || state.ClusterID != cfg.Replication.ClusterID || state.NodeID != cfg.Replication.NodeID {
		return nil, errors.New("Replica startup configuration differs from authenticated member")
	}
	releaseWorkspace, err := acquireProjectionWorkspace(ctx, cfg, pressure)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, releaseWorkspace())
		}
	}()
	diskPermit, err := acquireLedgerProjectionDisk(ctx, cfg, pressure)
	if err != nil {
		return nil, err
	}
	usageDiskPermit, err := acquireUsageProjectionDisk(ctx, cfg, pressure)
	if err != nil {
		return nil, errors.Join(err, diskPermit.Release())
	}
	diskPermitHandedOff := false
	usageDiskPermitHandedOff := false
	defer func() {
		if !diskPermitHandedOff {
			resultErr = errors.Join(resultErr, diskPermit.Release())
		}
		if !usageDiskPermitHandedOff {
			resultErr = errors.Join(resultErr, usageDiskPermit.Release())
		}
	}()
	output := replicaRuntimeProjectionPath(cfg)
	for _, path := range []string{output, output + ".usage"} {
		if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
			return nil, err
		}
	}
	for _, path := range []string{output, output + ".usage"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	maxDiskBytes := int64(0)
	usageMaxDiskBytes := int64(0)
	if cfg.ResourceBudget != nil {
		maxDiskBytes = cfg.ResourceBudget.LedgerProjectionDiskBytes
		usageMaxDiskBytes = cfg.ResourceBudget.UsageProjectionDiskBytes
	}
	diskPermitHandedOff = true
	usageDiskPermitHandedOff = true
	return member.prepareReplicaAccountingGenerationWithDiskLimits(ctx, native, metadata, output, maxDiskBytes, diskPermit, usageMaxDiskBytes, usageDiskPermit)
}
