package app

import (
	"context"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/masterkey"
)

func unlockMasterKey(ctx context.Context, cfg config.Config) ([]byte, error) {
	unlocker, err := masterkey.NewUnlocker(cfg.Storage.MasterKey)
	if err != nil {
		return nil, err
	}
	return unlocker.Unlock(ctx)
}

func fileMasterKeyStore(cfg config.Config) (*masterkey.FileStore, error) {
	return masterkey.FileStoreFromConfig(cfg.Storage.MasterKey)
}
