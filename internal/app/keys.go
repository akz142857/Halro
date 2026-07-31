package app

import (
	"context"
	"fmt"

	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/config"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
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
	return nil
}
