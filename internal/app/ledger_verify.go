package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/ledger"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/vault"
)

// VerifyLedger is the offline operator command behind `heimdall ledger
// verify`: a full walk of the committed WAL authenticating every epoch-4
// frame's MAC and hash chain, then a comparison against the trusted
// checkpoint the way the fail-closed startup gate already does. Unlike the
// startup gate, this reports rather than refuses — an operator asking "is my
// ledger intact" wants an answer even when the answer is no.
func VerifyLedger(ctx context.Context, cfg config.Config) (ledger.ChainReport, error) {
	if err := ctx.Err(); err != nil {
		return ledger.ChainReport{}, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return ledger.ChainReport{}, err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return ledger.ChainReport{}, err
	}
	defer store.Close()
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return ledger.ChainReport{}, err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return ledger.ChainReport{}, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return ledger.ChainReport{}, err
	}
	ledgerKey, err := loadLedgerHMACKey(store, secretVault, masterKey)
	if err != nil {
		return ledger.ChainReport{}, err
	}
	defer clear(ledgerKey)
	report, partial, err := ledger.VerifyChain(cfg.LedgerPath(), ledgerKey)
	if err != nil {
		return ledger.ChainReport{}, err
	}
	if partial {
		return ledger.ChainReport{}, errors.New("ledger has a partial tail; run `heimdall doctor` or start Heimdall to repair it before verifying")
	}
	if !report.ChainVerified {
		return report, nil
	}
	checkpoint, err := store.LedgerChainCheckpoint()
	if err != nil {
		return ledger.ChainReport{}, fmt.Errorf("load ledger chain checkpoint: %w", err)
	}
	// The checkpoint only advances at startup reconciliation, so a clean
	// shutdown after a long session leaves the on-disk chain well ahead of
	// it — that is the expected common case, not tampering. Only a
	// checkpoint that is ahead of the file (truncation) or that disagrees at
	// the same sequence (a rewrite) is a failure here.
	if checkpoint.Sequence > report.ChainSequence ||
		(checkpoint.Sequence == report.ChainSequence &&
			(checkpoint.Offset != report.ChainOffset || checkpoint.Hash != report.ChainHash)) {
		return ledger.ChainReport{}, errors.New("ledger chain does not match its trusted checkpoint")
	}
	return report, nil
}
