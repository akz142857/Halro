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

func BootstrapAdmin(
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
	adminCount, err := store.AdminUserCount(ctx)
	if err != nil {
		return err
	}
	if adminCount != 0 {
		return errors.New("an admin user already exists")
	}
	user, err := adminauth.NewUser(username, password, domain.AdminRoleAdministrator, time.Now().UTC())
	if err != nil {
		return err
	}
	defer clear(user.PasswordHash)
	defer clear(user.PasswordSalt)
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
	if _, err := store.CreateFirstAdmin(ctx, user); err != nil {
		if errors.Is(err, boltstore.ErrAdminInitialized) {
			return errors.New("an admin user already exists")
		}
		return fmt.Errorf("store admin user: %w", err)
	}
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	if _, err := auditLog.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "local_cli",
		Action: "admin.bootstrap", TargetType: "admin_user", TargetID: username,
		Outcome: "success",
	}); err != nil {
		return err
	}
	return checkpointAudit(store, auditLog.Summary())
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
