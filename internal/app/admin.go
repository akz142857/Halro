package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type BootstrapAdminOptions struct {
	OperationID string
	IfNeeded    bool
}

type BootstrapAdminResult string

var (
	ErrAdminBootstrapAmbiguous = errors.New("ambiguous_partial_bootstrap")
	ErrAdminBootstrapConflict  = errors.New("bootstrap_operation_conflict")
)

const (
	BootstrapAdminCreated          BootstrapAdminResult = "created"
	BootstrapAdminAlreadyCompleted BootstrapAdminResult = "already_completed"
)

func BootstrapAdmin(ctx context.Context, cfg config.Config, username string, password []byte) error {
	_, err := BootstrapAdminWithOptions(ctx, cfg, username, password, BootstrapAdminOptions{})
	return err
}

func BootstrapAdminWithOptions(
	ctx context.Context,
	cfg config.Config,
	username string,
	password []byte,
	options BootstrapAdminOptions,
) (BootstrapAdminResult, error) {
	operationID := options.OperationID
	if operationID == "" {
		if options.IfNeeded {
			return "", errors.New("--operation-id is required with --if-needed")
		}
		var err error
		operationID, err = id.New("bootstrapop")
		if err != nil {
			return "", err
		}
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return "", err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return "", err
	}
	defer store.Close()
	user, err := adminauth.NewUser(username, password, domain.AdminRoleAdministrator, time.Now().UTC())
	if err != nil {
		return "", err
	}
	defer clear(user.PasswordHash)
	defer clear(user.PasswordSalt)
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return "", err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return "", err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return "", err
	}
	auditKey, err := loadAuditHMACKey(store, secretVault, masterKey)
	if err != nil {
		return "", err
	}
	defer clear(auditKey)
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		return "", err
	}
	defer auditLog.Close()
	if err := reconcileAuditCheckpoint(store, auditLog.Summary()); err != nil {
		return "", err
	}
	eventID, err := id.New("aud")
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	intent := domain.AdminAuditIntent{
		EventID: eventID, OccurredAt: now, ActorType: "local_cli", ActorID: username,
		Action: "admin.bootstrap", TargetType: "admin_user", TargetID: username,
		Metadata: map[string]string{"operation_id": operationID, "source": "offline"},
	}
	completion := boltstore.AdminBootstrapCompletion{
		OperationID: operationID, Username: username, AuditEventID: eventID, CommittedAt: now,
	}
	_, storedCompletion, created, err := store.CreateFirstAdminWithAuditIntent(ctx, user, completion, intent)
	if errors.Is(err, boltstore.ErrAdminInitialized) {
		return "", fmt.Errorf("%w: an administrator exists without a matching completion marker", ErrAdminBootstrapAmbiguous)
	}
	if errors.Is(err, boltstore.ErrAdminBootstrapConflict) {
		return "", fmt.Errorf("%w: an administrator exists for a different operation or username", ErrAdminBootstrapConflict)
	}
	if err != nil {
		return "", fmt.Errorf("store admin user and audit intent: %w", err)
	}
	if !created && !options.IfNeeded {
		return "", errors.New("an admin user already exists")
	}
	if err := deliverOfflineAdminAuditIntents(ctx, store, auditLog); err != nil {
		return "", fmt.Errorf("deliver administrator bootstrap audit: %w", err)
	}
	if err := verifyAdminBootstrapAuditRecord(auditLog, storedCompletion); err != nil {
		return "", fmt.Errorf("%w: %v", ErrAdminBootstrapAmbiguous, err)
	}
	if storedCompletion.OperationID != operationID || storedCompletion.Username != username {
		return "", errors.New("administrator bootstrap completion does not match the requested operation")
	}
	if created {
		return BootstrapAdminCreated, nil
	}
	return BootstrapAdminAlreadyCompleted, nil
}

func verifyAdminBootstrapAuditRecord(auditLog *audit.Log, completion boltstore.AdminBootstrapCompletion) error {
	found := false
	_, err := auditLog.Replay(func(record audit.Record) error {
		if record.Event.EventID != completion.AuditEventID {
			return nil
		}
		if record.Event.Action != "admin.bootstrap" || record.Event.TargetType != "admin_user" ||
			record.Event.TargetID != completion.Username || record.Event.Outcome != "success" ||
			record.Event.Metadata["operation_id"] != completion.OperationID {
			return errors.New("administrator bootstrap completion points to a mismatched audit event")
		}
		found = true
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("administrator bootstrap completion has no matching audit event")
	}
	return nil
}

func deliverOfflineAdminAuditIntents(ctx context.Context, store *boltstore.Store, auditLog *audit.Log) error {
	intents, err := store.ListPendingAdminAuditIntents(ctx)
	if err != nil {
		return err
	}
	return deliverOfflineAdminAuditIntentsWith(ctx, intents,
		func(ctx context.Context, intent domain.AdminAuditIntent) error {
			metadata := make(map[string]any, len(intent.Metadata))
			for key, value := range intent.Metadata {
				metadata[key] = value
			}
			if len(metadata) == 0 {
				metadata = nil
			}
			_, err := auditLog.Append(ctx, audit.Event{
				EventID: intent.EventID, OccurredAt: intent.OccurredAt, ActorType: intent.ActorType,
				ActorID: intent.ActorID, Action: intent.Action, TargetType: intent.TargetType,
				TargetID: intent.TargetID, Outcome: "success", CorrelationID: intent.CorrelationID,
				Metadata: metadata,
			})
			return err
		},
		func() error { return checkpointAudit(store, auditLog.Summary()) },
		func(ctx context.Context, eventID string) error { return store.DeleteAdminAuditIntent(ctx, eventID) },
	)
}

func deliverOfflineAdminAuditIntentsWith(
	ctx context.Context,
	intents []domain.AdminAuditIntent,
	appendIntent func(context.Context, domain.AdminAuditIntent) error,
	checkpoint func() error,
	deleteIntent func(context.Context, string) error,
) error {
	for _, intent := range intents {
		if err := appendIntent(ctx, intent); err != nil {
			return err
		}
		if err := checkpoint(); err != nil {
			return err
		}
		if err := deleteIntent(ctx, intent.EventID); err != nil {
			return err
		}
	}
	return nil
}

// ResetAdminPassword is an offline break-glass operation. It invalidates all
// sessions for the selected local user and records the action in the trusted
// audit chain without ever accepting the old password.
func ResetAdminPassword(
	ctx context.Context,
	cfg config.Config,
	username string,
	password []byte,
) error {
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer store.Close()
	current, err := store.GetAdminUser(ctx, username)
	if err != nil {
		return errors.New("admin user was not found")
	}
	replacement, err := adminauth.NewUser(current.Username, password, current.Role, time.Now().UTC())
	if err != nil {
		return err
	}
	defer clear(replacement.PasswordHash)
	defer clear(replacement.PasswordSalt)
	replacement.CreatedAt = current.CreatedAt
	replacement.Locale = current.Locale
	replacement.Appearance = domain.NormalizeAppearance(current.Appearance)
	replacement.SessionGeneration = current.SessionGeneration + 1

	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return err
	}
	auditKey, err := loadAuditHMACKey(store, secretVault, masterKey)
	if err != nil {
		return err
	}
	defer clear(auditKey)
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		return err
	}
	defer auditLog.Close()
	if err := reconcileAuditCheckpoint(store, auditLog.Summary()); err != nil {
		return err
	}
	if _, err := store.PutAdminUser(ctx, replacement, current.Revision); err != nil {
		return fmt.Errorf("replace admin password: %w", err)
	}
	if err := store.DeleteAdminSessionsForUser(ctx, current.Username); err != nil {
		return fmt.Errorf("invalidate admin sessions: %w", err)
	}
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	if _, err := auditLog.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "local_cli",
		Action: "admin.password.reset", TargetType: "admin_user", TargetID: current.Username,
		Outcome: "success",
	}); err != nil {
		return err
	}
	return checkpointAudit(store, auditLog.Summary())
}

// ResetAdminMFA is an offline break-glass operation. It removes all second
// factors and invalidates every session for the selected administrator.
func ResetAdminMFA(ctx context.Context, cfg config.Config, username string) error {
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.GetAdminUser(ctx, username); err != nil {
		return errors.New("admin user was not found")
	}
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return err
	}
	defer secretVault.Close()
	if err = verifyVaultKeyCheck(store, secretVault); err != nil {
		return err
	}
	auditKey, err := loadAuditHMACKey(store, secretVault, masterKey)
	if err != nil {
		return err
	}
	defer clear(auditKey)
	auditLog, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		return err
	}
	defer auditLog.Close()
	if err = reconcileAuditCheckpoint(store, auditLog.Summary()); err != nil {
		return err
	}
	if _, err = store.ResetAdminMFAIdentity(ctx, username); err != nil {
		return err
	}
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	if _, err = auditLog.Append(ctx, audit.Event{EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "local_cli", Action: "admin.mfa.reset_offline", TargetType: "admin_user", TargetID: username, Outcome: "success"}); err != nil {
		return err
	}
	return checkpointAudit(store, auditLog.Summary())
}
