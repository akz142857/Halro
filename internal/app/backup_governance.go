package app

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// Caller owns the offline data/stage directory and an unpublished quiescent
// native log. Validate domain history with fresh finite disk batches, never
// loading a saved Governance State/checkpoint or publishing the scratch DB.
func validateBackupGovernanceProjection(ctx context.Context, cfg config.Config, metadata *boltstore.Store, log *governance.Log, source string, key []byte, expected governance.Summary, parent string) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.ResourceBudget != nil && cfg.ResourceBudget.MemoryStateRequired {
		if log.Summary() != expected {
			return errors.New("backup Governance native head does not match expected prefix")
		}
		if err := verifyBackupGovernanceAuthority(ctx, cfg, metadata, log, key); err != nil {
			return err
		}
		pressure, err := commandPressure(ctx, cfg, nil)
		if err != nil {
			return err
		}
		return validatePackedBackupGovernanceProjection(ctx, cfg, log, key, expected, pressure, parent, nil)
	}
	credit, err := acquireBackupProjectionBudget(ctx, cfg, backupGovernanceProjection)
	if err != nil {
		return err
	}
	var cleanupErr error
	defer func() { resultErr = errors.Join(resultErr, credit.retire(withRetirementDebt(cleanupErr, resultErr))) }()
	if log.Summary() != expected {
		return errors.New("backup Governance native head does not match expected prefix")
	}
	if err := verifyBackupGovernanceAuthority(ctx, cfg, metadata, log, key); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(parent, ".halro-governance-check-*")
	if err != nil {
		return err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return err
	}
	// This private namespace contains one owned DB. Never recursively delete
	// unknown replacements; a nonempty directory is a cleanup error.
	defer func() {
		cleanupErr = errors.Join(cleanupErr, removeOwnedReferenceDirectory(directory, identity))
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	p, err := governance.BuildGovernanceProjectionWithLimit(ctx, source, key, filepath.Join(directory, "governance.db"), expected, credit.limit)
	if err != nil {
		return err
	}
	defer func() {
		cleanupErr = errors.Join(cleanupErr, p.Discard())
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	head, err := p.ReadHead(ctx)
	if err != nil {
		return err
	}
	if head != expected || log.Summary() != expected {
		return errors.New("backup Governance prefix changed during validation")
	}
	return ctx.Err()
}

func verifyBackupGovernanceAuthority(ctx context.Context, cfg config.Config, metadata *boltstore.Store, log *governance.Log, key []byte) error {
	if !metadata.AuthenticatedMetadata() {
		return verifyGovernanceJournalAnchor(ctx, metadata, log, key)
	}
	scope, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if scope == nil || scope.selected == nil || scope.selected.state.generation.Owners.NativeGovernance != log || !scope.selected.guard.CoversNativePath(cfg.GovernancePath()) {
		return authenticatedstate.ErrSealIneligible
	}
	seal, found, err := scope.selected.state.publisher.Current()
	if err != nil || !found {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	var saved authenticatedstate.SealPosition
	var root authenticatedstate.SealRoot
	for _, p := range seal.Positions[:seal.PositionCount] {
		if p.Component == authenticatedstate.SealComponentGovernance {
			saved = p
		}
	}
	for _, r := range seal.Roots[:seal.RootCount] {
		if r.Kind == authenticatedstate.SealRootNative && r.Component == authenticatedstate.SealComponentGovernance {
			root = r
		}
	}
	ref, err := log.SealReference(ctx, scope.pressure.logicalBudget(), governance.Summary{Records: saved.Sequence, Bytes: int64(saved.Offset), LastHash: saved.Hash})
	if err != nil || ref.Position != saved || ref.Root != root {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	return log.VerifyHead(ctx, log.Summary())
}
