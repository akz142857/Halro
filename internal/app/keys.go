package app

import (
	"context"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/id"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type CreatedGatewayKey struct {
	KeyID      string `json:"key_id"`
	ProjectID  string `json:"project_id"`
	GatewayKey string `json:"gateway_key"`
}

func CreateProjectKey(ctx context.Context, cfg config.Config, projectID, name string) (CreatedGatewayKey, error) {
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return CreatedGatewayKey{}, err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return CreatedGatewayKey{}, err
	}
	defer store.Close()
	if _, err := store.GetProject(ctx, projectID); err != nil {
		return CreatedGatewayKey{}, fmt.Errorf("load project: %w", err)
	}
	plaintext, key, err := auth.GenerateGatewayKey(projectID, name, nil)
	if err != nil {
		return CreatedGatewayKey{}, err
	}
	key, err = store.PutGatewayKey(ctx, key, 0)
	if err != nil {
		return CreatedGatewayKey{}, fmt.Errorf("store gateway key: %w", err)
	}
	if err := appendOfflineKeyAudit(ctx, cfg, store, "gateway_key.create", key.ID); err != nil {
		return CreatedGatewayKey{}, err
	}
	return CreatedGatewayKey{KeyID: key.ID, ProjectID: projectID, GatewayKey: plaintext}, nil
}

func DisableProjectKey(ctx context.Context, cfg config.Config, keyID string) error {
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
	key, err := store.GetGatewayKey(ctx, keyID)
	if err != nil {
		return fmt.Errorf("load gateway key: %w", err)
	}
	key.Enabled = false
	if _, err := store.PutGatewayKey(ctx, key, key.Revision); err != nil {
		return fmt.Errorf("disable gateway key: %w", err)
	}
	return appendOfflineKeyAudit(ctx, cfg, store, "gateway_key.disable", key.ID)
}

// appendOfflineKeyAudit records a CLI key mutation in the same trusted chain the admin
// API writes to. Issuing or revoking a gateway credential must leave a trace no matter
// which surface performed it, so a failure here fails the command.
func appendOfflineKeyAudit(ctx context.Context, cfg config.Config, store *boltstore.Store, action, keyID string) error {
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
	eventID, err := id.New("aud")
	if err != nil {
		return err
	}
	if _, err := auditLog.Append(ctx, audit.Event{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorType: "local_cli",
		Action: action, TargetType: "gateway_key", TargetID: keyID, Outcome: "success",
	}); err != nil {
		return err
	}
	return checkpointAudit(store, auditLog.Summary())
}
