package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// The physical DB survives Manager retirement until real readers have joined.
// Standalone and Primary reconstruct authenticated native facts on every Open.
type runtimeGovernanceOwner struct {
	retirementErr error
	projection    *governance.GovernanceProjection
	diskPermit    *runtimeProjectionDiskPermit
	path          string
	file          os.FileInfo
}

func standaloneGovernanceProjectionPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "governance-projection.db")
}

// No State or historical checkpoint is loaded. The external authenticated
// anchor must still match an actual native frame: full frame authentication
// alone cannot detect deletion of a valid complete suffix.
func verifyGovernanceJournalAnchor(ctx context.Context, metadata *boltstore.Store, log *governance.Log, key []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	head := log.Summary()
	if err := log.VerifyHead(ctx, head); err != nil {
		return err
	}
	anchor, err := metadata.GovernanceJournalAnchor()
	hasAnchor := err == nil
	if err != nil && !errors.Is(err, boltstore.ErrNotFound) {
		return fmt.Errorf("load governance journal anchor: %w", err)
	}
	if !hasAnchor && head.Records > 0 {
		return errGovernanceAnchorMismatch
	}
	if hasAnchor && (!governance.VerifyJournalAnchorAuth(key, anchor.Sequence, anchor.Offset, anchor.Hash, anchor.Authentication) || anchor.Sequence > head.Records || anchor.Offset > head.Bytes) {
		return errGovernanceAnchorMismatch
	}
	matched := !hasAnchor
	actual, err := log.Replay(func(record governance.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hasAnchor && record.Sequence == anchor.Sequence {
			if record.Offset != anchor.Offset || record.Hash != anchor.Hash {
				return errGovernanceAnchorMismatch
			}
			matched = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !matched || actual != head {
		return errGovernanceAnchorMismatch
	}
	return log.VerifyHead(ctx, head)
}

// Called only before publication, with the data-directory lock and a quiescent
// native log. It never grants HA role permission or trusts a saved derivative.
func prepareStandaloneGovernance(ctx context.Context, cfg config.Config, metadata *boltstore.Store, log *governance.Log, key []byte, pressure *resourcePressureRuntime) (*runtimeGovernanceOwner, error) {
	return prepareRuntimeGovernance(ctx, cfg, metadata, log, key, nil, pressure)
}

func prepareRuntimeGovernance(ctx context.Context, cfg config.Config, metadata *boltstore.Store, log *governance.Log, key []byte, member *replicationRuntime, pressure *resourcePressureRuntime) (result *runtimeGovernanceOwner, resultErr error) {
	check := func() error { return ctx.Err() }
	if cfg.Replication != nil {
		if member == nil || member.role != replication.RolePrimary || member.publisher == nil {
			return nil, errors.New("Primary Governance requires authenticated member ownership")
		}
		before := member.publisher.Snapshot()
		check = func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if before.Term != before.PromisedTerm || before.NodeID != cfg.Replication.NodeID || before.ClusterID != cfg.Replication.ClusterID || !samePrimaryStartupAuthority(before, member.publisher.Snapshot()) {
				return errors.New("Primary Governance construction role/term/identity changed")
			}
			return nil
		}
	}
	if err := check(); err != nil {
		return nil, err
	}
	if err := validateRuntimeDerivedPath(ctx, cfg, standaloneGovernanceProjectionPath(cfg)); err != nil {
		return nil, err
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
	diskPermit, err := acquireGovernanceProjectionDisk(ctx, cfg, pressure)
	if err != nil {
		return nil, err
	}
	if err := verifyGovernanceJournalAnchor(ctx, metadata, log, key); err != nil {
		if diskPermit != nil {
			err = errors.Join(err, diskPermit.Release())
		}
		return nil, err
	}
	head := log.Summary()
	if err := check(); err != nil {
		if diskPermit != nil {
			err = errors.Join(err, diskPermit.Release())
		}
		return nil, err
	}
	path := standaloneGovernanceProjectionPath(cfg)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		if diskPermit != nil {
			err = errors.Join(err, diskPermit.Release())
		}
		return nil, fmt.Errorf("discard prior derived Governance file: %w", err)
	}
	owner := &runtimeGovernanceOwner{path: path, diskPermit: diskPermit}
	complete := false
	defer func() {
		if !complete {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				owner.retirementErr = errors.Join(owner.retirementErr, resultErr)
			}
			resultErr = errors.Join(resultErr, owner.close())
		}
	}()
	maxDiskBytes := int64(0)
	if cfg.ResourceBudget != nil {
		maxDiskBytes = cfg.ResourceBudget.GovernanceProjectionDiskBytes
	}
	p, err := governance.BuildGovernanceProjectionWithLimit(ctx, cfg.GovernancePath(), key, owner.path, head, maxDiskBytes)
	if err != nil {
		return nil, err
	}
	owner.projection = p
	owner.file, err = os.Lstat(owner.path)
	if err == nil {
		err = log.VerifyHead(ctx, head)
	}
	if err == nil && head.Records > 0 {
		err = check()
	}
	if err == nil && head.Records > 0 {
		actual, authentication := log.AuthenticatedHead()
		err = metadata.PutGovernanceJournalAnchor(actual.Records, actual.Bytes, actual.LastHash, authentication)
	}
	if err == nil {
		err = check()
	}
	if err == nil {
		err = log.VerifyHead(ctx, head)
	}
	if err != nil {
		return nil, err
	}
	if owner.diskPermit != nil {
		if err := owner.diskPermit.Reclassify(resourcebudget.Background); err != nil {
			return nil, err
		}
	}
	complete = true
	return owner, nil
}

func (o *runtimeGovernanceOwner) close() error {
	if o == nil {
		return nil
	}
	err := o.retirementErr
	if o.projection != nil {
		discardErr := o.projection.Discard()
		err = errors.Join(err, discardErr)
		if discardErr == nil {
			o.projection = nil
		}
	}
	if o.projection == nil && o.diskPermit != nil && o.retirementErr == nil {
		removed, statErr := derivedFileRemoved(o.path)
		err = errors.Join(err, statErr)
		if statErr == nil && removed {
			releaseErr := o.diskPermit.Release()
			err = errors.Join(err, releaseErr)
			if releaseErr == nil {
				o.diskPermit = nil
			}
		} else if statErr == nil {
			err = errors.Join(err, errors.New("Governance file remains; retain its Disk claim"))
		}
	}
	return err
}
