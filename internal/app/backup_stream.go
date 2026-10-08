package app

import (
	"context"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
)

// CreateBackupSummary selects format5 without collecting historical Files.
// The older collecting CreateBackup interfaces preserve their legacy formats.
func CreateBackupSummary(ctx context.Context, cfg config.Config, configPath, outputPath string, key []byte, options CreateBackupOptions) (backup.ArchiveSummary, error) {
	var summary backup.ArchiveSummary
	_, err := createBackupWithWriter(ctx, cfg, configPath, outputPath, key, options, func(options backup.CreateOptions) (backup.Manifest, error) {
		var err error
		summary, err = backup.CreatePaged(options)
		return summary.Manifest, err
	})
	if err != nil {
		return backup.ArchiveSummary{}, err
	}
	return summary, nil
}
func WriteCreateBackup(ctx context.Context, cfg config.Config, configPath, outputPath string, key []byte, options CreateBackupOptions, destination io.Writer) error {
	if destination == nil {
		return errors.New("backup report destination is required")
	}
	ctx, err := backupWorkspaceContext(ctx, cfg)
	if err != nil {
		return err
	}
	summary, err := CreateBackupSummary(ctx, cfg, configPath, outputPath, key, options)
	if err != nil {
		return err
	}
	// All native snapshot owners and the data lock have retired. Verify the
	// published archive and provide the complete inventory through the new reader.
	return backup.WriteVerifiedReportForSummary(ctx, outputPath, key, summary, destination)
}
func WriteVerifyBackup(ctx context.Context, path string, key []byte, destination io.Writer) error {
	return backup.WriteVerifiedReport(ctx, path, key, destination)
}

// WriteVerifyBackupWithConfig admits all private indexes/spools to one command
// budget. The no-config API preserves its legacy resource contract.
func WriteVerifyBackupWithConfig(ctx context.Context, cfg config.Config, path string, key []byte, destination io.Writer) error {
	ctx, err := backupWorkspaceContext(ctx, cfg)
	if err != nil {
		return err
	}
	return backup.WriteVerifiedReport(ctx, path, key, destination)
}
