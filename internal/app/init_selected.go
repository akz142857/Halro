package app

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

// selectedInitKeyGeneration is the keyring version a fresh instance starts at.
const selectedInitKeyGeneration = 1

// selectedInitArchivePath names the internal archive a selected initialization
// activates from. It sits beside the data directory because backups refuse an
// output inside it, and it must survive a crash: an interrupted preparation
// only restarts from the archive whose ciphertext it recorded.
func selectedInitArchivePath(cfg config.Config) (string, error) {
	data, err := filepath.Abs(cfg.Storage.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(data), "."+filepath.Base(data)+".init.hmbk"), nil
}

// selectedInitSecret derives per-instance material for initialization from the
// Master Key, so a restarted initialization recomputes the same archive key and
// identity without writing new key material to disk.
func selectedInitSecret(masterKey []byte, purpose string) ([]byte, error) {
	return hkdf.Key(sha256.New, masterKey, nil, "halro:selected-init:"+purpose+":v1", 32)
}

// InitializeSelected brings an empty Standalone data directory to an activated
// selected instance: it writes the exchange format (Metadata bbolt plus native
// logs), takes an internal archive of it and activates the seven roots from
// that archive, the same path a restore uses. Every step is resumable from what
// is on disk. It reports false when the instance was already activated.
func InitializeSelected(ctx context.Context, cfg config.Config, configPath string) (bool, error) {
	return initializeSelected(ctx, cfg, configPath, nil, nil)
}

// source and phase are test seams: source drives the real pressure monitor and
// phase observes durable boundaries without mutating any file.
func initializeSelected(ctx context.Context, cfg config.Config, configPath string, source resourcebudget.SampleSource, phase func(string) error) (bool, error) {
	checkpoint := func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if phase != nil {
			return phase(name)
		}
		return nil
	}
	if cfg.Replication != nil {
		return false, errors.New("selected initialization is Standalone only")
	}
	if cfg.ResourceBudget == nil || cfg.ResourcePressure == nil || cfg.ResourceBudget.BackupWorkspaceDiskBytes <= 0 || cfg.ResourceBudget.MetadataStoreDiskBytes <= 0 || cfg.ResourceBudget.MetadataJournalDiskBytes <= 0 {
		return false, errors.New("selected initialization requires resource_budget, resource_pressure, backup_workspace_disk_bytes, metadata_store_disk_bytes and metadata_journal_disk_bytes")
	}
	archivePath, err := selectedInitArchivePath(cfg)
	if err != nil {
		return false, err
	}
	activated, err := memoryRuntimeActivated(cfg)
	resume := errors.Is(err, ErrMemoryStateActivationPending)
	if err != nil && !resume {
		return false, err
	}
	if activated {
		return false, removeSelectedInitArchive(archivePath)
	}
	// Only a published common seal is resumed; before it, preparation restarts
	// and preserves the pending bootstrap with the rest of the interrupted tree.
	sealPublished := false
	if resume {
		if _, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, "active.seal")); err == nil {
			sealPublished = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	// The exchange format and the internal archive are written by the ordinary
	// Standalone paths, without the shared budget: the selected budget governs
	// the instance being activated, not its one-shot seed.
	seed := cfg
	seed.ResourceBudget, seed.ResourcePressure = nil, nil
	if !resume {
		state, err := InspectInitialization(seed)
		if err != nil {
			return false, err
		}
		switch state {
		case InitializationEmpty:
			if err := Initialize(seed); err != nil {
				return false, err
			}
			if err := checkpoint("exchange-written"); err != nil {
				return false, err
			}
		case InitializationSystemReady:
		default:
			return false, errors.New("Halro initialization is incomplete; restore the matching master key and data directory or move the partial state aside")
		}
	}
	masterKey, err := unlockMasterKey(ctx, seed)
	if err != nil {
		return false, err
	}
	defer clear(masterKey)
	if sealPublished {
		return true, resumeSelectedInitialization(ctx, cfg, archivePath, source)
	}
	archiveKey, err := selectedInitSecret(masterKey, "archive-key")
	if err != nil {
		return false, err
	}
	defer clear(archiveKey)
	if _, err := os.Lstat(archivePath); errors.Is(err, os.ErrNotExist) {
		// Seed only before the archive exists: any later Metadata write would
		// move the native position the archive and its restart are bound to.
		if _, err := seedSelectedInitInstanceID(ctx, seed); err != nil {
			return false, err
		}
		pending := archivePath + ".pending"
		if err := os.Remove(pending); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if _, err := CreateBackup(ctx, seed, configPath, pending, archiveKey); err != nil {
			return false, fmt.Errorf("create internal initialization archive: %w", err)
		}
		if err := checkpoint("archive-pending"); err != nil {
			return false, err
		}
		if err := os.Rename(pending, archivePath); err != nil {
			return false, err
		}
		if err := durable.SyncDirectory(filepath.Dir(archivePath)); err != nil {
			return false, err
		}
		if err := checkpoint("archive-published"); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	identityKey, err := selectedInitSecret(masterKey, "identity")
	if err != nil {
		return false, err
	}
	var identity [32]byte
	copy(identity[:], identityKey)
	clear(identityKey)
	if err := activateSelectedInitialization(ctx, cfg, masterKey, archivePath, archiveKey, identity, source, phase); err != nil {
		return false, err
	}
	if err := checkpoint("activated"); err != nil {
		return false, err
	}
	return true, removeSelectedInitArchive(archivePath)
}

// seedSelectedInitInstanceID writes the instance ID before the archive is
// taken; the Ordinary engine only seeds it on its first start, and the
// activated baseline must carry the same ID it is bound to.
func seedSelectedInitInstanceID(ctx context.Context, cfg config.Config) (instanceID string, resultErr error) {
	store, err := openCommandMetadataStore(ctx, cfg, cfg.MetadataPath())
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return "", err
	}
	defer clear(masterKey)
	v, err := vault.New(masterKey)
	if err != nil {
		return "", err
	}
	defer v.Close()
	if err := verifyVaultKeyCheck(store, v); err != nil {
		return "", err
	}
	if _, err := attachConfiguredMetadataJournal(store, v, masterKey, "selected initialization", cfg); err != nil {
		return "", err
	}
	candidate, err := id.New("ins")
	if err != nil {
		return "", err
	}
	return store.SeedInstanceID(candidate)
}

func activateSelectedInitialization(ctx context.Context, cfg config.Config, masterKey []byte, archivePath string, archiveKey []byte, identity [32]byte, source resourcebudget.SampleSource, phase func(string) error) (resultErr error) {
	manager, stop, err := startSelectedInitPressure(ctx, cfg, source)
	if err != nil {
		return err
	}
	defer stop()
	// The command-level open refuses once the private namespace exists, so the
	// retained exchange-format Metadata is read directly, as preparation does.
	old, err := boltstore.OpenReadOnlyWithMappingBudget(ctx, cfg.MetadataPath(), metadataLegacyFileCeiling(cfg), manager)
	if err != nil {
		return err
	}
	v, err := vault.New(masterKey)
	if err != nil {
		return errors.Join(err, old.Close())
	}
	verifyErr := verifyVaultKeyCheck(old, v)
	ledgerKey, ledgerErr := loadLedgerHMACKey(old, v, masterKey)
	journalKey, journalErr := loadMetadataJournalHMACKey(old, v, masterKey)
	instanceID, instanceErr := old.InstanceID()
	v.Close()
	defer clear(ledgerKey)
	defer clear(journalKey)
	if err := errors.Join(verifyErr, ledgerErr, journalErr, instanceErr, old.Close()); err != nil {
		return err
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("selected initialization requires an offline data directory: %w", err)
	}
	keepGuard := false
	defer func() {
		if !keepGuard {
			resultErr = errors.Join(resultErr, guard.Close())
		}
	}()
	workspace, err := backup.NewWorkspaceBudget(ctx, cfg.ResourceBudget.BackupWorkspaceDiskBytes, func(ctx context.Context, bytes int64) (func() error, error) {
		permit, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 20, Disk: bytes})
		if err != nil {
			return nil, err
		}
		return permit.Release, nil
	})
	if err != nil {
		return err
	}
	ctx, err = workspace.Bind(ctx)
	if err != nil {
		return err
	}
	archive, err := backup.OpenVerified(ctx, archivePath, archiveKey)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, archive.Close()) }()
	restart, err := memoryMigrationNamespacePresent(cfg)
	if err != nil {
		return err
	}
	if err := PrepareMemoryRuntimeMigrationDirectory(ctx, cfg, MemoryMigrationDirectoryOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: selectedInitKeyGeneration, SourceJournalKey: journalKey, Restart: restart, PreparationHook: phase}, archive, ledgerKey, manager, guard); err != nil {
		return err
	}
	limits := cfg.ResourceBudget
	options := metadatastate.BaselineOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: selectedInitKeyGeneration, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: 8 << 20}
	p, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, ledgerKey, journalKey, cfg.Storage.DataDir, memoryStateDirectory(cfg), guard, options, limits.MetadataJournalDiskBytes)
	if err != nil {
		if p != nil {
			err = errors.Join(err, p.Close())
		}
		keepGuard = retainsSelectedInitLock(err)
		return err
	}
	activation, err := activatePreparedMemoryRuntime(ctx, cfg, p, guard, manager, ledgerKey, journalKey, phase)
	if err == nil {
		err = activation.Close()
	} else if activation != nil {
		err = errors.Join(err, activation.Close())
	}
	keepGuard = retainsSelectedInitLock(err)
	return err
}

func resumeSelectedInitialization(ctx context.Context, cfg config.Config, archivePath string, source resourcebudget.SampleSource) (resultErr error) {
	manager, stop, err := startSelectedInitPressure(ctx, cfg, source)
	if err != nil {
		return err
	}
	defer stop()
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("selected initialization requires an offline data directory: %w", err)
	}
	if _, err := ResumeMemoryRuntimeActivation(ctx, cfg, guard, manager); err != nil {
		if retainsSelectedInitLock(err) {
			return err
		}
		return errors.Join(err, guard.Close())
	}
	if err := guard.Close(); err != nil {
		return err
	}
	return removeSelectedInitArchive(archivePath)
}

// An owner whose descriptor did not definitely exit keeps the data lock until
// the process exits, so a second initialization cannot inherit uncertain IO.
func retainsSelectedInitLock(err error) bool {
	return errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ledger.ErrSegmentRetirement) || errors.Is(err, boltstore.ErrMetadataFileRetirement)
}

// startSelectedInitPressure runs the real pressure monitor and waits for its
// first Healthy lease; a missing or corrupt signal never becomes Healthy.
func startSelectedInitPressure(ctx context.Context, cfg config.Config, source resourcebudget.SampleSource) (*resourcebudget.Manager, func(), error) {
	manager, err := resourcebudget.New(cfg.ResourceBudget.Limits(cfg.ResourcePressure.MaxAge.Value()))
	if err != nil {
		return nil, nil, err
	}
	if source == nil {
		sampler, err := resourcebudget.NewSampler(cfg.Storage.DataDir)
		if err != nil {
			return nil, nil, err
		}
		source = sampler
	}
	monitor, err := resourcebudget.NewMonitor(source, cfg.ResourcePressure.Policy(), cfg.ResourcePressure.SampleInterval.Value())
	if err != nil {
		return nil, nil, err
	}
	if err := monitor.AttachManager(manager); err != nil {
		return nil, nil, err
	}
	monitorCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- monitor.Run(monitorCtx, nil) }()
	stop := func() { cancel(); <-done }
	ticker := time.NewTicker(cfg.ResourcePressure.SampleInterval.Value())
	defer ticker.Stop()
	for manager.Snapshot().Pressure != resourcebudget.Healthy {
		select {
		case <-ctx.Done():
			stop()
			return nil, nil, ctx.Err()
		case <-ticker.C:
			state := monitor.Snapshot()
			if state.Samples > 0 && state.Issues & ^resourcebudget.Cooling != 0 {
				stop()
				return nil, nil, resourcebudget.ErrPressure
			}
		}
	}
	return manager, stop, nil
}

func removeSelectedInitArchive(path string) error {
	removed := false
	for _, name := range []string{path + ".pending", path} {
		if err := os.Remove(name); err == nil {
			removed = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !removed {
		return nil
	}
	return durable.SyncDirectory(filepath.Dir(path))
}
