package app

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// SealReport is what `halro ledger seal` prints.
type SealReport struct {
	Rolled            bool            `json:"rolled"`
	ActiveGeneration  uint64          `json:"active_generation"`
	Sealed            *ledger.Segment `json:"sealed,omitempty"`
	Compacted         *ledger.Segment `json:"compacted,omitempty"`
	SealedGenerations int             `json:"sealed_generations"`
}

// SealLedger rolls the active generation on demand.
//
// The maintenance tick does this by size when sealing is enabled, which is the
// path an unattended instance uses. This is the other half an operator needs:
// they are about to copy the data directory, or hand an auditor an archive, and
// they want the boundary drawn now rather than whenever the file next crosses a
// threshold. Copying, not moving — the sealed history has to stay whole, and a
// generation taken out of it is refused at the next open. It is offline — it takes the
// data directory lock — because it rewrites which file the log appends to, and
// running it against a live instance would race the process that owns it.
func SealLedger(ctx context.Context, cfg config.Config, compress bool) (SealReport, error) {
	return sealLedger(ctx, cfg, compress, func(*SealReport, *ledger.Log) error { return nil })
}

// The final observer runs with the verified Log and offline lock still owned.
// All native owners physically close before this function returns to a publisher.
func sealLedger(ctx context.Context, cfg config.Config, compress bool, observe func(*SealReport, *ledger.Log) error) (report SealReport, err error) {
	ctx, _, retireWorkspace, commandScopeErr := ledgerMaintenanceContext(ctx, cfg, false)
	if commandScopeErr != nil {
		var commandScopeZero0 SealReport
		return commandScopeZero0, commandScopeErr
	}
	defer func() {
		err = errors.Join(err, retireWorkspace(!errors.Is(err, errLedgerMaintenanceRetirement) && !errors.Is(err, ledger.ErrSegmentRetirement)))
		if err != nil {
			report = SealReport{}
		}
	}()

	if err := ctx.Err(); err != nil {
		return SealReport{}, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return SealReport{}, err
	}
	var nativeDisk *commandNativeDiskOwner
	defer func() {
		err = nativeDisk.finish(err)
		if err != nil {
			report = SealReport{}
		}
	}()
	defer func() {
		if closeErr := dataLock.Close(); closeErr != nil {
			err = errors.Join(err, errLedgerMaintenanceRetirement, closeErr)
		}
		if err != nil {
			report = SealReport{}
		}
	}()
	nativeDisk, err = prepareCommandNativeDisk(ctx, cfg, dataLock)
	if err != nil {
		return SealReport{}, err
	}
	store, err := openCommandMetadataStore(ctx, cfg, cfg.MetadataPath())
	if err != nil {
		return SealReport{}, err
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			err = errors.Join(err, errLedgerMaintenanceRetirement, closeErr)
		}
		if err != nil {
			report = SealReport{}
		}
	}()
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return SealReport{}, err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return SealReport{}, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return SealReport{}, err
	}
	ledgerKey, err := loadLedgerHMACKey(store, secretVault, masterKey)
	if err != nil {
		return SealReport{}, err
	}
	indexDiskOwner, indexLimit, err := newOneShotLedgerSegmentIndexDiskOwner(ctx, cfg)
	if err != nil {
		clear(ledgerKey)
		return SealReport{}, err
	}
	var log *ledger.Log
	if indexDiskOwner != nil {
		defer func() {
			err = retireCommandLedgerIndex(indexDiskOwner, log, err)
			if err != nil {
				report = SealReport{}
			}
		}()
	}
	log, err = ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), commandNativeOptions(ctx, ledger.Options{
		ChainKey: ledgerKey, MaxSegmentDirectoryIndexBytes: indexLimit,
	}))
	clear(ledgerKey)
	if err != nil {
		return SealReport{}, err
	}
	defer func() {
		if closeErr := log.Close(); closeErr != nil {
			err = errors.Join(err, errLedgerMaintenanceRetirement, closeErr)
		}
		if err != nil {
			report = SealReport{}
		}
	}()

	result, err := log.Roll()
	if err != nil {
		return SealReport{}, err
	}
	report = SealReport{Rolled: result.Rolled, ActiveGeneration: result.Active}
	if result.Rolled {
		sealed := result.Sealed
		report.Sealed = &sealed
		if compress {
			compacted, err := log.CompactContext(ctx, sealed.Generation)
			if err != nil {
				return SealReport{}, err
			}
			report.Compacted = &compacted
		}
	}
	// Always, whether or not anything rolled: the command's real product is the
	// assurance that the archive still authenticates, and an operator running
	// it before moving files off the box is asking exactly that question.
	verification, err := log.VerifySealedSummary(0)
	if err != nil {
		return SealReport{}, fmt.Errorf("verify sealed generations: %w", err)
	}
	if verification.Generations == 0 && result.Rolled {
		return SealReport{}, errors.New("a generation was sealed but the archive reports none")
	}
	if verification.Generations > uint64(math.MaxInt) {
		return SealReport{}, errors.New("sealed generation count exceeds report capacity")
	}
	report.SealedGenerations = int(verification.Generations)
	// The chain checkpoint has to learn where the head moved to. Left behind, it
	// names an offset in a file that is no longer the active one, and the next
	// start would read that as a chain that had gone backwards.
	if err := reconcileLedgerChainCheckpoint(store, log); err != nil {
		return SealReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return SealReport{}, err
	}
	if err := observe(&report, log); err != nil {
		return SealReport{}, err
	}
	return report, nil
}
