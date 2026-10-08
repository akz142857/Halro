package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// VerifyLedger is the offline operator command behind `halro ledger
// verify`: a full walk of the committed WAL authenticating every epoch-4
// frame's MAC and hash chain, then a comparison against the trusted
// checkpoint the way the fail-closed startup gate already does. Unlike the
// startup gate, this reports rather than refuses — an operator asking "is my
// ledger intact" wants an answer even when the answer is no.
func VerifyLedger(ctx context.Context, cfg config.Config) (result ledger.ChainReport, resultErr error) {
	ctx, _, commandScopeErr := commandBudgetContext(ctx, cfg)
	if commandScopeErr != nil {
		var commandScopeZero0 ledger.ChainReport
		return commandScopeZero0, commandScopeErr
	}

	if err := ctx.Err(); err != nil {
		return ledger.ChainReport{}, err
	}
	dataLock, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		return ledger.ChainReport{}, err
	}
	ctx = withCommandDataLock(ctx, dataLock)
	defer func() {
		resultErr = finishCommandDataLock(dataLock, resultErr)
		if resultErr != nil {
			result = ledger.ChainReport{}
		}
	}()
	if err := assertConfiguredMetadataSchemaCurrent(ctx, cfg); err != nil {
		return ledger.ChainReport{}, err
	}
	store, err := openCommandMetadataReadOnly(ctx, cfg, cfg.MetadataPath())
	if err != nil {
		return ledger.ChainReport{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
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
	if report, selected, err := verifySelectedCommandLedgerPrefix(ctx, cfg, ledgerKey); selected {
		if err != nil {
			return ledger.ChainReport{}, err
		}
		checkpoint, err := store.LedgerChainCheckpoint()
		if err != nil {
			return ledger.ChainReport{}, err
		}
		return report, verifyLedgerCheckpoint(report, checkpoint)
	}
	// Sealed generations first, and every one of them: the chain runs through
	// them into the active file, so verifying only what is still being appended
	// to would answer a smaller question after every roll — and eventually,
	// once the active file has just rolled, almost none of one.
	sealed, err := ledger.VerifySegmentsSummary(filepath.Dir(cfg.LedgerPath()), ledgerKey, 0)
	if err != nil {
		return ledger.ChainReport{}, err
	}
	report, partial, err := ledger.VerifyChain(cfg.LedgerPath(), ledgerKey)
	if err != nil {
		return ledger.ChainReport{}, err
	}
	report.SealedGenerations = sealed.Generations
	report.SealedAuthenticated = sealed.Authenticated
	report.ChecksumOnly += sealed.ChecksumOnly
	if partial {
		return ledger.ChainReport{}, errors.New("ledger has a partial tail; run `halro doctor` or start Halro to repair it before verifying")
	}
	checkpoint, err := store.LedgerChainCheckpoint()
	if err != nil {
		return ledger.ChainReport{}, fmt.Errorf("load ledger chain checkpoint: %w", err)
	}
	if err := verifyLedgerCheckpoint(report, checkpoint); err != nil {
		return ledger.ChainReport{}, err
	}
	return report, nil
}

func verifyLedgerCheckpoint(report ledger.ChainReport, checkpoint boltstore.LedgerChainCheckpoint) error {
	// An unverified chain is only benign on a ledger that never had one. Once
	// the checkpoint is non-zero, "no authenticated frames" is the signature of
	// a wiped or downgraded WAL, and reporting it as a clean result would make
	// this command certify exactly what it exists to detect.
	if !report.ChainVerified {
		if checkpoint.Sequence > 0 {
			return errors.New("ledger chain does not match its trusted checkpoint")
		}
		return nil
	}
	// The checkpoint only advances at startup reconciliation, so a clean
	// shutdown after a long session leaves the on-disk chain well ahead of
	// it — that is the expected common case, not tampering. Only a
	// checkpoint that is ahead of the file (truncation) or that disagrees at
	// the same sequence (a rewrite) is a failure here.
	//
	// The offset is only comparable inside one generation: a roll leaves the
	// sequence and the chain head exactly where they were and moves the head
	// into a fresh file at offset zero.
	if checkpoint.Sequence > report.ChainSequence || checkpoint.Generation > report.Head.Generation {
		return errors.New("ledger chain does not match its trusted checkpoint")
	}
	if checkpoint.Sequence == report.ChainSequence &&
		(checkpoint.Hash != report.ChainHash ||
			(checkpoint.Generation == report.Head.Generation && checkpoint.Offset != report.ChainOffset)) {
		return errors.New("ledger chain does not match its trusted checkpoint")
	}
	return nil
}
