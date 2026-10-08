package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

type metadataMigrationArguments struct {
	activateRuntime                                                    bool
	resumeActivation                                                   bool
	restartPreparation                                                 bool
	backup, backupKey, ledgerKey, sourceJournalKey, output, instanceID string
	identity                                                           [32]byte
	keyGeneration                                                      uint64
}

func runMetadataCommand(arguments []string, out io.Writer) error {
	if len(arguments) > 0 && (arguments[0] == "ha-prepare" || arguments[0] == "ha-maintain" || arguments[0] == "ha-install" || arguments[0] == "ha-activate" || arguments[0] == "ha-checkpoint") {
		return runHAMetadataCommand(arguments[0], arguments[1:], out)
	}
	if len(arguments) > 0 && arguments[0] == "export-legacy" {
		return runSelectedLegacyExportCommand(arguments[1:], out)
	}
	if len(arguments) == 0 || arguments[0] != "migrate" {
		return errors.New("usage: halro metadata migrate --config <path> --from-backup <file> --backup-key-file <file> --ledger-key-file <file> --source-journal-key-file <file> --identity <64 hex> --instance-id <id> --key-generation <n> --output <absent directory>")
	}
	f := flag.NewFlagSet("metadata migrate", flag.ContinueOnError)
	configPath := f.String("config", "config.yaml", "offline Standalone configuration with calibrated resource budget and pressure")
	a := metadataMigrationArguments{}
	f.StringVar(&a.backup, "from-backup", "", "authenticated encrypted source backup")
	f.StringVar(&a.backupKey, "backup-key-file", "", "32-byte backup key file")
	f.StringVar(&a.ledgerKey, "ledger-key-file", "", "independently recovered stable 32-byte Ledger key file")
	f.StringVar(&a.sourceJournalKey, "source-journal-key-file", "", "stable format1 Metadata HMAC key file")
	f.StringVar(&a.instanceID, "instance-id", "", "independently established instance ID")
	f.Uint64Var(&a.keyGeneration, "key-generation", 0, "independently established key generation")
	identity := f.String("identity", "", "independently established 32-byte identity in hex")
	f.StringVar(&a.output, "output", "", "absent private candidate directory; live files are never replaced")
	f.BoolVar(&a.activateRuntime, "activate-runtime", false, "publish all seven Standalone roots and activate default Runtime; output must be data_dir/memory-state")
	f.BoolVar(&a.resumeActivation, "resume-activation", false, "resume authenticated seven-root bootstrap publication after interruption")
	f.BoolVar(&a.restartPreparation, "restart-preparation", false, "preserve interrupted pre-seal generations and unqualified staging without trusting it; restart from the same authenticated backup")
	if err := f.Parse(arguments[1:]); err != nil {
		return err
	}
	id, err := hex.DecodeString(*identity)
	if f.NArg() != 0 || (!a.resumeActivation && (err != nil || len(id) != 32 || a.instanceID == "" || a.keyGeneration == 0 || a.backup == "" || a.backupKey == "" || a.ledgerKey == "" || a.sourceJournalKey == "" || a.output == "")) {
		return errors.New("metadata migration requires all source, key, independent identity and output flags")
	}
	copy(a.identity[:], id)
	if a.restartPreparation && (!a.activateRuntime || a.resumeActivation) {
		return errors.New("restart-preparation requires activate-runtime and all original source flags")
	}
	if a.identity == ([32]byte{}) && !a.resumeActivation {
		return metadatastate.ErrSource
	}
	cfg, err := config.Load(*configPath, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		return err
	}
	if err := rejectReplicationMutation(cfg, "metadata migrate"); err != nil {
		return err
	}
	if cfg.ResourceBudget == nil || cfg.ResourcePressure == nil || cfg.ResourceBudget.BackupWorkspaceDiskBytes <= 0 || cfg.ResourceBudget.MetadataStoreDiskBytes <= 0 || cfg.ResourceBudget.MetadataJournalDiskBytes <= 0 {
		return errors.New("metadata migration requires calibrated resource_budget, resource_pressure, backup_workspace_disk_bytes, metadata_store_disk_bytes and metadata_journal_disk_bytes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	manager, err := resourcebudget.New(cfg.ResourceBudget.Limits(cfg.ResourcePressure.MaxAge.Value()))
	if err != nil {
		return err
	}
	sampler, err := resourcebudget.NewSampler(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	monitor, err := resourcebudget.NewMonitor(sampler, cfg.ResourcePressure.Policy(), cfg.ResourcePressure.SampleInterval.Value())
	if err != nil {
		return err
	}
	if err := monitor.AttachManager(manager); err != nil {
		return err
	}
	monitorCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- monitor.Run(monitorCtx, nil) }()
	defer func() { stop(); <-done }()
	// Read the actual sampler's current lease, including its configured recovery
	// cooldown. Missing/corrupt physical signals never become manual Healthy.
	ticker := time.NewTicker(cfg.ResourcePressure.SampleInterval.Value())
	defer ticker.Stop()
	for manager.Snapshot().Pressure != resourcebudget.Healthy {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			state := monitor.Snapshot()
			if state.Samples > 0 && state.Issues & ^resourcebudget.Cooling != 0 {
				return resourcebudget.ErrPressure
			}
		}
	}
	return prepareMetadataMigrationCommand(ctx, cfg, a, manager, out)
}

// The command owns the real data-directory lock and verified archive. A
// supplied Manager is shared with its pressure owner, not constructed by rows.
func prepareMetadataMigrationCommand(ctx context.Context, cfg config.Config, a metadataMigrationArguments, manager *resourcebudget.Manager, out io.Writer) (resultErr error) {
	if err := rejectReplicationMutation(cfg, "metadata migrate"); err != nil {
		return err
	}
	if manager == nil || cfg.ResourceBudget == nil {
		return resourcebudget.ErrInvalid
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("metadata migration requires an offline data directory: %w", err)
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ledger.ErrSegmentRetirement) || errors.Is(resultErr, boltstore.ErrMetadataFileRetirement) {
			resultErr = &metadataMigrationLockDebt{guard: guard, cause: resultErr}
		} else {
			resultErr = errors.Join(resultErr, guard.Close())
		}
	}()
	if err := rejectReplicationMutation(cfg, "metadata migrate"); err != nil {
		return err
	}
	if a.resumeActivation {
		if a.activateRuntime || a.restartPreparation || a.backup != "" || a.output != "" {
			return errors.New("resume-activation cannot be combined with new migration output or activate-runtime")
		}
		receipt, err := app.ResumeMemoryRuntimeActivation(ctx, cfg, guard, manager)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(receipt)
	}
	backupKey, err := metadataMigrationKeyFile(a.backupKey)
	if err != nil {
		return err
	}
	defer clear(backupKey)
	ledgerKey, err := metadataMigrationKeyFile(a.ledgerKey)
	if err != nil {
		return err
	}
	defer clear(ledgerKey)
	journalKey, err := metadataMigrationKeyFile(a.sourceJournalKey)
	if err != nil {
		return err
	}
	defer clear(journalKey)
	workspaceBudget, err := backup.NewWorkspaceBudget(ctx, cfg.ResourceBudget.BackupWorkspaceDiskBytes, func(ctx context.Context, bytes int64) (func() error, error) {
		permit, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 20, Disk: bytes})
		if err != nil {
			return nil, err
		}
		return permit.Release, nil
	})
	if err != nil {
		return err
	}
	ctx, err = workspaceBudget.Bind(ctx)
	if err != nil {
		return err
	}
	archive, err := backup.OpenVerified(ctx, a.backup, backupKey)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, archive.Close()) }()
	output, err := filepath.Abs(a.output)
	if err != nil {
		return err
	}
	if a.activateRuntime && output != filepath.Join(cfg.Storage.DataDir, "memory-state") {
		return errors.New("Runtime activation output must be the reserved data_dir/memory-state directory")
	}
	a.output = output
	if a.activateRuntime {
		if err := prepareRuntimeMigrationDirectory(ctx, cfg, a, archive, ledgerKey, manager, guard); err != nil {
			return err
		}
	} else {
		if err := os.Mkdir(output, 0700); err != nil {
			return err
		}
	}
	// Never recursively remove a pathname: an uncertain owner or a foreign
	// replacement is preserved. Successful candidate files remain for review.
	defer func() {
		if resultErr != nil {
			_ = os.Remove(output)
		}
	}()
	options := metadatastate.BaselineOptions{Identity: a.identity, InstanceID: a.instanceID, KeyGeneration: a.keyGeneration, Budget: manager, MaxSourceBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxDiskBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxAppendBytes: 8 << 20}
	p, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, ledgerKey, journalKey, cfg.Storage.DataDir, output, guard, options, cfg.ResourceBudget.MetadataJournalDiskBytes)
	if err != nil {
		if p != nil {
			err = errors.Join(err, p.Close())
			return &metadatastate.MigrationPreparationFailure{Cause: err, Owner: p}
		}
		return err
	}
	if a.activateRuntime {
		activation, err := app.ActivatePreparedMemoryRuntime(ctx, cfg, p, guard, manager, ledgerKey, journalKey)
		if err != nil {
			return &memoryRuntimeActivationFailure{Cause: err, Owner: activation}
		}
		if err := errors.Join(activation.Close(), archive.Close(), guard.Close()); err != nil {
			return &memoryRuntimeActivationFailure{Cause: err, Owner: activation}
		}
		return json.NewEncoder(out).Encode(activation)
	}
	if err := p.Close(); err != nil {
		return &metadatastate.MigrationPreparationFailure{Cause: err, Owner: p}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := guard.Close(); err != nil {
		return err
	}
	// This is a content receipt, not a Runtime bootstrap/role/common-cut grant.
	return json.NewEncoder(out).Encode(p.Receipt)
}

type memoryRuntimeActivationFailure struct {
	Cause error
	Owner *app.MemoryRuntimeActivation
}

// The actual process guard remains reachable if a candidate/source descriptor
// did not definitely exit. A second migration cannot inherit uncertain IO.
type metadataMigrationLockDebt struct {
	guard *lock.Lock
	cause error
}

func (e *metadataMigrationLockDebt) Error() string { return e.cause.Error() }
func (e *metadataMigrationLockDebt) Unwrap() error { return e.cause }

func (e *memoryRuntimeActivationFailure) Error() string { return e.Cause.Error() }
func (e *memoryRuntimeActivationFailure) Unwrap() error { return e.Cause }

func metadataMigrationKeyFile(path string) (key []byte, resultErr error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, ownedfile.CloseFile(f))
		if resultErr != nil {
			clear(key)
			key = nil
		}
	}()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() != 32 {
		return nil, errors.Join(errors.New("migration key must be a private regular file containing exactly 32 bytes"), err)
	}
	key = make([]byte, 32)
	if _, err := io.ReadFull(f, key); err != nil {
		return key, err
	}
	after, err := f.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !os.SameFile(before, after) || !os.SameFile(before, named) || after.Size() != 32 || named.Size() != 32 || !before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(named.ModTime()) || after.Mode().Perm()&0077 != 0 || named.Mode().Perm()&0077 != 0 {
		return key, errors.Join(metadatastate.ErrSource, err, nameErr)
	}
	return key, nil
}
