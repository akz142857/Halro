package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// ResumeMemoryRuntimeActivation repairs only the publication boundary of an
// explicitly prepared encrypted bootstrap. It selects and fully audits all
// seven saved roots against that independent bootstrap before linking it. An
// incomplete preparation with no authenticated common seal cannot be promoted.
func ResumeMemoryRuntimeActivation(ctx context.Context, cfg config.Config, guard *lock.Lock, manager *resourcebudget.Manager) (receipt authenticatedstate.SealPublicationReceipt, resultErr error) {
	if cfg.ResourceBudget == nil || cfg.Replication != nil || manager == nil || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return receipt, authenticatedstate.ErrSealIneligible
	}
	path := memoryStateBootstrapPath(cfg)
	pending := path + ".pending"
	captured, err := os.Lstat(pending)
	if err != nil {
		return receipt, err
	}
	old, err := boltstore.OpenReadOnlyWithMappingBudget(ctx, cfg.MetadataPath(), metadataLegacyFileCeiling(cfg), manager)
	if err != nil {
		return receipt, err
	}
	defer func() { resultErr = errors.Join(resultErr, old.Close()) }()
	mk, err := unlockMasterKey(ctx, cfg, old)
	if err != nil {
		return receipt, err
	}
	defer clear(mk)
	v, err := vault.New(mk)
	if err != nil {
		return receipt, err
	}
	defer v.Close()
	if err := verifyVaultKeyCheck(old, v); err != nil {
		return receipt, err
	}
	b, err := readMemoryStateBootstrapAt(ctx, cfg, v, manager, pending)
	if err != nil || b == nil {
		return receipt, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	// A same-inode active alias is already the committed encrypted marker.
	// Retire only its authenticated duplicate before the generic native-path
	// collision checks and inventory; an unrelated active file is preserved.
	repairedAlias := false
	if active, activeErr := os.Lstat(path); activeErr == nil {
		named, err := os.Lstat(pending)
		if err != nil || !os.SameFile(captured, active) || !os.SameFile(captured, named) {
			return receipt, errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := os.Remove(pending); err != nil {
			return receipt, err
		}
		if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
			return receipt, err
		}
		repairedAlias = true
	} else if !errors.Is(activeErr, os.ErrNotExist) {
		return receipt, activeErr
	}
	s := &runtimeSelectedState{bootstrap: *b}
	defer func() {
		resultErr = errors.Join(resultErr, s.close())
		if resultErr != nil {
			receipt = authenticatedstate.SealPublicationReceipt{}
		}
	}()
	// The immutable retained key slot is used solely to decrypt provenance.
	if err := old.Close(); err != nil {
		return receipt, err
	}
	if err := openSelectedRuntimeGeneration(ctx, cfg, guard, manager, nil, s, nil); err != nil {
		return receipt, err
	}
	if err := s.generation.WaitStartupAudit(ctx); err != nil {
		return receipt, err
	}
	encodeFee, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 64 << 10})
	if err != nil {
		return receipt, err
	}
	defer func() { resultErr = errors.Join(resultErr, encodeFee.Release()) }()
	encoded, err := authenticatedstate.EncodeSealCandidate(s.generation.Seal, s.bootstrap.LedgerKey, statecut.HardLimits())
	if err != nil {
		return receipt, err
	}
	digest := sha256.Sum256(encoded)
	clear(encoded)
	if err := s.close(); err != nil {
		return receipt, &memorySelectedStateFailure{Cause: err, Owner: s}
	}
	if repairedAlias {
		seal := s.generation.Seal
		return authenticatedstate.SealPublicationReceipt{Generation: seal.Generation, Revision: seal.Revision, Digest: digest}, nil
	}
	named, err := os.Lstat(pending)
	if err != nil || !os.SameFile(captured, named) {
		return receipt, errors.Join(ownedfile.ErrReplaced, err)
	}
	active, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Link(pending, path); err != nil {
			return receipt, err
		}
		if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
			return receipt, err
		}
	} else if err != nil || !os.SameFile(captured, active) {
		return receipt, errors.Join(ownedfile.ErrReplaced, err)
	}
	active, err = os.Lstat(path)
	named, pendingErr := os.Lstat(pending)
	if err != nil || pendingErr != nil || !os.SameFile(captured, active) || !os.SameFile(captured, named) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return receipt, errors.Join(ownedfile.ErrReplaced, err, pendingErr)
	}
	// This is explicit recovery of a proven committed alias, not deletion rights
	// obtained from a capsule reader or a fabricated creation receipt.
	if err := os.Remove(pending); err != nil {
		return receipt, err
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return receipt, err
	}
	seal := s.generation.Seal
	return authenticatedstate.SealPublicationReceipt{Generation: seal.Generation, Revision: seal.Revision, Digest: digest}, nil
}
