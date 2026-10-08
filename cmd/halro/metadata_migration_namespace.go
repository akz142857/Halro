package main

import (
	"context"
	"path/filepath"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func prepareRuntimeMigrationDirectory(ctx context.Context, cfg config.Config, a metadataMigrationArguments, archive *backup.VerifiedArchive, key []byte, manager *resourcebudget.Manager, guard *lock.Lock) error {
	if !a.activateRuntime || a.output != filepath.Join(cfg.Storage.DataDir, "memory-state") {
		return metadatastate.ErrSource
	}
	journalKey, err := metadataMigrationKeyFile(a.sourceJournalKey)
	if err != nil {
		return err
	}
	defer clear(journalKey)
	return app.PrepareMemoryRuntimeMigrationDirectory(ctx, cfg, app.MemoryMigrationDirectoryOptions{Identity: a.identity, InstanceID: a.instanceID, KeyGeneration: a.keyGeneration, SourceJournalKey: journalKey, Restart: a.restartPreparation}, archive, key, manager, guard)
}
