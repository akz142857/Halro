package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// SealReport is what `halro ledger seal` prints.
type SealReport struct {
	Rolled            bool             `json:"rolled"`
	ActiveGeneration  uint64           `json:"active_generation"`
	Sealed            *ledger.Segment  `json:"sealed,omitempty"`
	Compacted         *ledger.Segment  `json:"compacted,omitempty"`
	SealedGenerations int              `json:"sealed_generations"`
	Verified          []ledger.Segment `json:"verified,omitempty"`
}

// SealLedger rolls the active generation on demand.
//
// The maintenance tick does this by size when sealing is enabled, which is the
// path an unattended instance uses. This is the other half an operator needs:
// they are about to move a generation off the box, or copy the data directory,
// or hand an auditor an archive, and they want the boundary drawn now rather
// than whenever the file next crosses a threshold. It is offline — it takes the
// data directory lock — because it rewrites which file the log appends to, and
// running it against a live instance would race the process that owns it.
func SealLedger(ctx context.Context, cfg config.Config, compress bool) (SealReport, error) {
	if err := ctx.Err(); err != nil {
		return SealReport{}, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return SealReport{}, err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return SealReport{}, err
	}
	defer store.Close()
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
	log, err := ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{ChainKey: ledgerKey})
	clear(ledgerKey)
	if err != nil {
		return SealReport{}, err
	}
	defer log.Close()

	result, err := log.Roll()
	if err != nil {
		return SealReport{}, err
	}
	report := SealReport{Rolled: result.Rolled, ActiveGeneration: result.Active}
	if result.Rolled {
		sealed := result.Sealed
		report.Sealed = &sealed
		if compress {
			compacted, err := log.Compact(sealed.Generation)
			if err != nil {
				return SealReport{}, err
			}
			report.Compacted = &compacted
		}
	}
	// Always, whether or not anything rolled: the command's real product is the
	// assurance that the archive still authenticates, and an operator running
	// it before moving files off the box is asking exactly that question.
	reports, err := log.VerifySealed(0)
	if err != nil {
		return SealReport{}, fmt.Errorf("verify sealed generations: %w", err)
	}
	if len(reports) == 0 && result.Rolled {
		return SealReport{}, errors.New("a generation was sealed but the archive reports none")
	}
	report.SealedGenerations = len(reports)
	report.Verified = log.Segments()
	// The chain checkpoint has to learn where the head moved to. Left behind, it
	// names an offset in a file that is no longer the active one, and the next
	// start would read that as a chain that had gone backwards.
	if err := reconcileLedgerChainCheckpoint(store, log); err != nil {
		return SealReport{}, err
	}
	return report, nil
}
