package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"gopkg.in/yaml.v3"
)

// SelectedLegacyExport is a point-in-time, independently runnable maintenance
// candidate. Its fresh archive is captured from the selected generation, never
// from the retired unlock database or a pre-migration backup. A reader from an
// arbitrary older release is not claimed compatible with this candidate.
type SelectedLegacyExport struct {
	Reader          buildinfo.Info `json:"reader"`
	BackupID        string         `json:"backup_id"`
	ConfigPath      string         `json:"config_path"`
	DataDir         string         `json:"data_dir"`
	ArchivePath     string         `json:"archive_path"`
	ArchiveKeyPath  string         `json:"archive_key_path"`
	LedgerSequence  uint64         `json:"ledger_sequence"`
	SchemaVersion   uint64         `json:"schema_version"`
	RuntimeVerified bool           `json:"runtime_verified"`
}

type selectedExportRetirement struct {
	cause       error
	ctx         context.Context
	reservation resourcebudget.Permit
}

func (e *selectedExportRetirement) Error() string { return e.cause.Error() }
func (e *selectedExportRetirement) Unwrap() error { return e.cause }

type selectedExportFileRetirement struct {
	cause error
	file  *os.File
	owner *ownedfile.Owner
}

func (e *selectedExportFileRetirement) Error() string { return e.cause.Error() }
func (e *selectedExportFileRetirement) Unwrap() error { return e.cause }

// ExportSelectedLegacy builds and verifies an isolated current-schema legacy
// reader candidate. It does not switch the source directory or its key. The
// source must be stopped: CreateBackup owns the actual offline lock and audits
// the complete selected state and native history before taking the snapshot.
// As with explicit restore, administrator sessions are invalidated and scheduled
// prices crossing the maintenance window are quarantined in the candidate.
// Failures preserve the candidate and fresh archive for inspection; a retry must
// select a new absent output, so it cannot silently reuse an obsolete snapshot.
func ExportSelectedLegacy(ctx context.Context, cfg config.Config, configPath, output string) (result SelectedLegacyExport, resultErr error) {
	return exportSelectedLegacyWithSampleSource(ctx, cfg, configPath, output, nil)
}

func exportSelectedLegacyWithSampleSource(ctx context.Context, cfg config.Config, configPath, output string, sourceSample resourcebudget.SampleSource) (result SelectedLegacyExport, resultErr error) {
	if cfg.Replication != nil || cfg.Storage.MasterKey.Mode != config.MasterKeyModeFile || cfg.ResourceBudget == nil || cfg.ResourcePressure == nil {
		return result, errors.New("selected legacy export requires a budgeted offline Standalone with file Master Key")
	}
	if active, err := memoryRuntimeActivated(cfg); err != nil || !active {
		return result, errors.Join(err, errors.New("selected legacy export requires an activated memory-state generation"))
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	sourceCtx, err := selectedExportCommandContext(ctx, cfg, sourceSample)
	if err != nil {
		return result, err
	}
	// Fixed keys, configuration and the finite reader receipt remain distinct
	// physical output from the archive and its independently admitted staging.
	_, pressure, err := commandBudgetContext(sourceCtx, cfg)
	if err != nil {
		return result, err
	}
	fixedOutput, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: 2 << 20, Disk: 2 << 20})
	if err != nil {
		return result, err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &selectedExportRetirement{cause: resultErr, ctx: sourceCtx, reservation: fixedOutput}
		} else {
			resultErr = errors.Join(resultErr, fixedOutput.Release())
		}
	}()
	abs, err := filepath.Abs(output)
	if err != nil || len(abs) > 4096 {
		return result, errors.Join(err, errors.New("invalid selected export output"))
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return result, err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	source, err := filepath.EvalSymlinks(cfg.Storage.DataDir)
	if err != nil || pathWithin(abs, source) || pathWithin(source, abs) {
		return result, errors.Join(err, errors.New("selected export output must be independent of its source data directory"))
	}
	// Unlock before creating any candidate. This is the original file key; the
	// fresh snapshot's Vault check independently authenticates it during restore.
	master, err := unlockMasterKey(ctx, cfg)
	if err != nil {
		return result, err
	}
	defer clear(master)
	if err := os.Mkdir(abs, 0700); err != nil {
		return result, err
	}
	if err := durable.SyncDirectory(parent); err != nil {
		return result, err
	}
	target := cfg
	target.Storage.DataDir = filepath.Join(abs, "data")
	target.Storage.MasterKey.File = filepath.Join(abs, "master.key")
	if target.Logging.File != "" {
		target.Logging.File = filepath.Join(abs, "halro.log")
	}
	if target.Logging.ErrorFile.File != "" {
		target.Logging.ErrorFile.File = filepath.Join(abs, "halro-errors.log")
	}
	if err := target.Validate(config.LoadOptions{SkipListenerValidation: true}); err != nil {
		return result, err
	}
	if err := writeSelectedExportFile(ctx, target.Storage.MasterKey.File, master); err != nil {
		return result, err
	}
	var archiveKey [32]byte
	if _, err := rand.Read(archiveKey[:]); err != nil {
		return result, err
	}
	defer clear(archiveKey[:])
	result.ArchivePath, result.ArchiveKeyPath = filepath.Join(abs, "latest.hmbk"), filepath.Join(abs, "archive.key")
	if err := writeSelectedExportFile(ctx, result.ArchiveKeyPath, archiveKey[:]); err != nil {
		return result, err
	}
	summary, err := CreateBackupSummary(sourceCtx, cfg, configPath, result.ArchivePath, archiveKey[:], CreateBackupOptions{})
	if err != nil {
		return result, err
	}
	if err := os.Mkdir(target.Storage.DataDir, 0700); err != nil {
		return result, err
	}
	targetCtx, err := selectedExportCommandContext(ctx, target, sourceSample)
	if err != nil {
		return result, err
	}
	var restored RestoreResult
	err = RestoreBackupWithReport(targetCtx, target, result.ArchivePath, archiveKey[:], summary.Manifest.BackupID, RestoreOptions{}, func(value RestoreResult, _ func(func(string) error) error) error {
		restored = value // finite result: no historical Gateway Key ID slice
		return nil
	})
	if err != nil {
		return result, err
	}
	// Exercise the actual legacy Runtime startup/recovery/readiness gate. The
	// restored directory intentionally has no selected bootstrap or common seal.
	r, err := OpenWithOptions(ctx, target, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{resourceSampleSource: sourceSample, AllowGeneratedSetupToken: true, offlineVerification: true})
	if err != nil {
		return result, err
	}
	if r.selectedState != nil {
		return result, errors.Join(errors.New("export candidate unexpectedly selected a memory-state backend"), r.Close())
	}
	if err := r.Close(); err != nil {
		return result, err
	}
	encoded, err := yaml.Marshal(target)
	if err != nil || len(encoded) > 1<<20 {
		return result, errors.Join(err, errors.New("selected export config exceeds 1 MiB"))
	}
	result.Reader, result.BackupID = buildinfo.Current(), restored.BackupID
	result.ConfigPath, result.DataDir = filepath.Join(abs, "config.yaml"), target.Storage.DataDir
	result.LedgerSequence, result.SchemaVersion, result.RuntimeVerified = restored.LedgerSequence, uint64(restored.SchemaVersionAfter), true
	if err := writeSelectedExportFile(ctx, result.ConfigPath, encoded); err != nil {
		return result, err
	}
	receipt, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if err := writeSelectedExportFile(ctx, filepath.Join(abs, "verified-reader.json"), receipt); err != nil {
		return result, err
	}
	return result, nil
}

// Distinct directories have distinct physical native owners and pressure
// sources. They must not borrow the source command's path-bound reservations.
func selectedExportCommandContext(ctx context.Context, cfg config.Config, source resourcebudget.SampleSource) (context.Context, error) {
	p, err := newResourcePressureWithSource(cfg, source)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: p, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure}), nil
}

func writeSelectedExportFile(ctx context.Context, path string, data []byte) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, owner, err := ownedfile.Create(path, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := ownedfile.CloseFile(f); closeErr != nil {
			resultErr = &selectedExportFileRetirement{cause: errors.Join(resultErr, closeErr), file: f, owner: owner}
		}
	}()
	if n, err := f.Write(data); err != nil || n != len(data) {
		return errors.Join(err, io.ErrShortWrite)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := owner.VerifyFile(f); err != nil {
		return err
	}
	return durable.SyncDirectory(filepath.Dir(path))
}
