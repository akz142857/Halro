package app

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

// A finite default for an offline diagnostic derivative, not a production data
// capacity promise. Budget mode uses the operator's paid positive file ceiling.
const doctorLedgerReferenceDiskBytes int64 = 64 << 20

type doctorLedgerObservation struct {
	Head                                ledger.Watermark
	Partial, LeasesVerified             bool
	Pending                             ledger.PendingLeaseStatistics
	RecoveredReleased, RecoveredSettled uint64
	ChainStatus, ChainDetail            string
}

// Doctor already owns the offline data lock. This fresh private derivative is
// diagnostic only: it grants no role/confirmation, mutation or startup authority.
// All whole-job work/file fees precede source IO and survive uncertain cleanup.
func inspectDoctorLedger(ctx context.Context, cfg config.Config, metadata *boltstore.Store, secretVault *vault.Vault, masterKey []byte) (observation doctorLedgerObservation, resultErr error) {
	credit, err := acquireBackupProjectionBudget(ctx, cfg, backupLedgerProjection)
	if err != nil {
		return observation, err
	}
	var cleanupErr error
	defer func() {
		if cleanupErr != nil {
			doctorRetirementDebt(ctx, cleanupErr)
		}
		resultErr = errors.Join(resultErr, credit.retire(withRetirementDebt(cleanupErr, resultErr)), ctx.Err())
		if resultErr != nil {
			observation = doctorLedgerObservation{}
		}
	}()
	observation.ChainStatus, observation.ChainDetail = "unverified", "cryptographic verification was skipped; run `halro ledger verify`"
	var key []byte
	if metadata != nil && secretVault != nil && len(masterKey) > 0 {
		key, err = loadLedgerHMACKey(metadata, secretVault, masterKey)
		if err != nil {
			observation.ChainDetail = "ledger chain key is unavailable: " + err.Error()
		}
		defer clear(key)
	}
	consume := func(record ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch record.Event.Outcome {
		case "recovered_not_started":
			if observation.RecoveredReleased == math.MaxUint64 {
				return ledger.ErrProjectionPageLimit
			}
			observation.RecoveredReleased++
		case "recovered_started_unknown_result":
			if observation.RecoveredSettled == math.MaxUint64 {
				return ledger.ErrProjectionPageLimit
			}
			observation.RecoveredSettled++
		}
		return nil
	}
	if len(key) != 32 {
		observation.Head, observation.Partial, err = ledger.InspectReplayContext(ctx, cfg.LedgerPath(), consume)
		return observation, err
	}
	if _, _, err := verifySelectedCommandLedgerPrefix(ctx, cfg, key); err != nil {
		return observation, err
	}
	report, partial, err := ledger.InspectReplayAuthenticatedContext(ctx, cfg.LedgerPath(), key, consume)
	if err != nil {
		return observation, err
	}
	observation.Head, observation.Partial = report.Head, partial
	observation.ChainStatus, observation.ChainDetail = doctorLedgerChainStatus(metadata, report, partial)
	if partial || observation.ChainStatus == "fail" {
		return observation, nil
	}
	directory, err := os.MkdirTemp("", "halro-doctor-ledger-*")
	if err != nil {
		return observation, err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return observation, err
	}
	defer func() {
		cleanupErr = errors.Join(cleanupErr, removeDoctorReferenceDirectory(directory, identity))
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	limit := credit.limit
	if limit == 0 {
		limit = doctorLedgerReferenceDiskBytes
	}
	projection, err := ledger.BuildLedgerProjectionWithLimit(ctx, cfg.LedgerPath(), key, filepath.Join(directory, "ledger.db"), report, limit)
	if err != nil {
		return observation, err
	}
	defer func() {
		cleanupErr = errors.Join(cleanupErr, projection.Discard())
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	observation.Pending, err = projection.ReadPendingLeaseStats(ctx, time.Now())
	if err == nil {
		observation.LeasesVerified = true
	}
	return observation, err
}
