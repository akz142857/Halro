package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

func VerifyAudit(ctx context.Context, cfg config.Config) (result audit.Summary, resultErr error) {
	ctx, _, commandScopeErr := commandBudgetContext(ctx, cfg)
	if commandScopeErr != nil {
		var commandScopeZero0 audit.Summary
		return commandScopeZero0, commandScopeErr
	}

	if err := ctx.Err(); err != nil {
		return audit.Summary{}, err
	}
	dataLock, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		return audit.Summary{}, err
	}
	ctx = withCommandDataLock(ctx, dataLock)
	defer func() {
		resultErr = finishCommandDataLock(dataLock, resultErr)
		if resultErr != nil {
			result = audit.Summary{}
		}
	}()
	if err := assertConfiguredMetadataSchemaCurrent(ctx, cfg); err != nil {
		return audit.Summary{}, err
	}
	store, err := openCommandMetadataReadOnly(ctx, cfg, cfg.MetadataPath())
	if err != nil {
		return audit.Summary{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return audit.Summary{}, err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return audit.Summary{}, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return audit.Summary{}, err
	}
	auditKey, err := loadAuditHMACKey(store, secretVault, masterKey)
	if err != nil {
		return audit.Summary{}, err
	}
	defer clear(auditKey)
	completion, completionErr := store.AdminBootstrapCompletion(ctx)
	if completionErr != nil && !errors.Is(completionErr, boltstore.ErrNotFound) {
		return audit.Summary{}, fmt.Errorf("load administrator bootstrap completion: %w", completionErr)
	}
	foundBootstrap := false
	checkpoint, err := store.AuditCheckpoint()
	if err != nil {
		return audit.Summary{}, fmt.Errorf("load audit checkpoint: %w", err)
	}
	foundCheckpoint := checkpoint.Records == 0 && checkpoint.Bytes == 0 && checkpoint.LastHash == ([32]byte{})
	summary, err := inspectCommandAudit(ctx, cfg, auditKey, func(record audit.Record) error {
		if record.Sequence == checkpoint.Records {
			foundCheckpoint = record.Offset == checkpoint.Bytes && record.Hash == checkpoint.LastHash
		}
		if completionErr != nil || record.Event.EventID != completion.AuditEventID {
			return nil
		}
		if record.Event.Action != "admin.bootstrap" || record.Event.TargetType != "admin_user" ||
			record.Event.TargetID != completion.Username || record.Event.Outcome != "success" ||
			record.Event.Metadata["operation_id"] != completion.OperationID {
			return errors.New("administrator bootstrap completion points to a mismatched audit event")
		}
		foundBootstrap = true
		return nil
	})
	if err != nil {
		return audit.Summary{}, err
	}
	if completionErr == nil && !foundBootstrap {
		return audit.Summary{}, errors.New("administrator bootstrap completion has no matching audit event")
	}
	if store.AuthenticatedMetadata() {
		// Node-local checkpoint rows may lag the selected common cut. The
		// complete scan must match both this historical row and the independent
		// common Audit position, checked by inspectCommandAudit.
		if !foundCheckpoint {
			return audit.Summary{}, errors.New("audit log does not match its trusted checkpoint prefix")
		}
		return summary, nil
	}
	if checkpoint.Records != summary.Records || checkpoint.Bytes != summary.Bytes ||
		checkpoint.LastHash != summary.LastHash {
		return audit.Summary{}, errors.New("audit log does not match its trusted checkpoint")
	}
	return summary, nil
}
