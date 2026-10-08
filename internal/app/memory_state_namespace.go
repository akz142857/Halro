package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

const migrationIntentLimit = 4096
const migrationInterruptedSlots = 4

// MemoryMigrationDirectoryOptions supplies independently selected provenance.
// Restart preserves private files; it does not authorize their contents.
type MemoryMigrationDirectoryOptions struct {
	Identity         [32]byte
	InstanceID       string
	KeyGeneration    uint64
	SourceJournalKey []byte
	Restart          bool
	// PreparationHook observes durable preparation boundaries; callers must not
	// mutate namespace or source files. It is primarily for process-crash tests.
	PreparationHook func(string) error
}

// This receipt authorizes only preserving an interrupted private namespace.
// It supplies no root selection, deletion, native replay or startup authority.
type migrationNamespaceIntent struct {
	Version       uint8
	Path          string
	Identity      [32]byte
	InstanceID    string
	KeyGeneration uint64
	Source        [32]byte
	Device, Inode uint64
	Attempt       uint8
}

func migrationDirectoryIdentity(info os.FileInfo) (uint64, uint64, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return 0, 0, ownedfile.ErrReplaced
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}

func migrationIntentMAC(key, raw []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("halro:memory-migration:namespace:v1\x00"))
	m.Write(raw)
	return m.Sum(nil)
}

func readMigrationNamespaceIntent(directory string, expected migrationNamespaceIntent, key []byte) (actual migrationNamespaceIntent, resultErr error) {
	path := filepath.Join(directory, "migration.intent")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= sha256.Size || info.Size() > migrationIntentLimit {
		return actual, errors.Join(metadatastate.ErrSource, err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return actual, err
	}
	defer func() { resultErr = errors.Join(resultErr, ownedfile.CloseFile(f)) }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return actual, errors.Join(ownedfile.ErrReplaced, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, migrationIntentLimit+1))
	if err != nil || len(raw) <= sha256.Size || len(raw) > migrationIntentLimit {
		return actual, errors.Join(metadatastate.ErrSource, err)
	}
	payload := raw[:len(raw)-sha256.Size]
	if !hmac.Equal(raw[len(payload):], migrationIntentMAC(key, payload)) {
		return actual, metadatastate.ErrSource
	}
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	if err := d.Decode(&actual); err != nil {
		return actual, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return actual, metadatastate.ErrSource
	}
	info, err = os.Lstat(directory)
	if err != nil {
		return actual, err
	}
	dev, ino, err := migrationDirectoryIdentity(info)
	if err != nil || actual.Version != 1 || actual.Path != expected.Path || actual.Identity != expected.Identity || actual.InstanceID != expected.InstanceID || actual.KeyGeneration != expected.KeyGeneration || actual.Source != expected.Source || actual.Device != dev || actual.Inode != ino || actual.Attempt > migrationInterruptedSlots {
		return actual, errors.Join(metadatastate.ErrSource, err)
	}
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, named) || opened.Size() != named.Size() || !opened.ModTime().Equal(named.ModTime()) {
		return actual, errors.Join(ownedfile.ErrReplaced, err)
	}
	return actual, nil
}

// PrepareMemoryRuntimeMigrationDirectory publishes a private namespace only
// after its durable preparation receipt is complete. Explicit restart preserves
// authenticated interrupted generations in four fixed slots; pre-receipt staging
// remains opaque in four separate unqualified slots and grants no root authority.
// Restart reimports the independently authenticated backup and native sources;
// no partially built root is promoted. Disk reconciliation covers the retained
// generation before any rename, and exhausting slots is a definite refusal.
func PrepareMemoryRuntimeMigrationDirectory(ctx context.Context, cfg config.Config, o MemoryMigrationDirectoryOptions, archive *backup.VerifiedArchive, key []byte, manager *resourcebudget.Manager, guard *lock.Lock) (resultErr error) {
	path := filepath.Join(cfg.Storage.DataDir, "memory-state")
	staging := path + ".preparing"
	checkpoint := func(phase string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if o.PreparationHook != nil {
			return o.PreparationHook(phase)
		}
		return nil
	}
	if cfg.Replication != nil || cfg.ResourceBudget == nil || o.Identity == ([32]byte{}) || len(o.InstanceID) == 0 || len(o.InstanceID) > 128 || o.KeyGeneration == 0 || len(o.SourceJournalKey) != 32 || archive == nil || manager == nil || len(key) != 32 || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return metadatastate.ErrSource
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A committed common seal must use the selected-root publication recovery.
	for _, name := range []string{"active.seal", "memory-state.bootstrap"} {
		if _, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, name)); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(os.ErrExist, err)
		}
	}
	source, err := archive.CiphertextDigest()
	if err != nil {
		return err
	}
	// Re-import is eligible only while the retained native Metadata still
	// projects this exact backup. Check it before preserving any names.
	journalKey := bytes.Clone(o.SourceJournalKey)
	defer clear(journalKey)
	old, err := boltstore.OpenReadOnlyWithMappingBudget(ctx, cfg.MetadataPath(), metadataLegacyFileCeiling(cfg), manager)
	if err != nil {
		return err
	}
	metadataInfo, infoErr := old.Info()
	journalPath := old.MetadataJournalPath()
	if err := errors.Join(infoErr, old.Close()); err != nil {
		return err
	}
	savedMetadata := archive.Summary().Metadata
	// A backup can compact its bbolt image, giving that image a different
	// transaction ID. The authoritative native position and schema must agree;
	// a snapshot's local TxID cannot fence the retained source database.
	if metadataInfo.SchemaVersion != savedMetadata.SchemaVersion || metadataInfo.MetadataJournalEpoch != savedMetadata.MetadataJournalEpoch || metadataInfo.MetadataJournalSequence != savedMetadata.MetadataJournalSequence || metadataInfo.MinimumLedgerReaderVersion != savedMetadata.MinimumLedgerReaderVersion || metadataInfo.LedgerFeatureEpoch != savedMetadata.LedgerFeatureEpoch {
		return fmt.Errorf("retained Metadata native position differs from backup: %w", metadatastate.ErrSource)
	}
	head, err := metadatajournal.ProbeExactContext(ctx, journalPath, journalKey, cfg.ResourceBudget.MetadataJournalDiskBytes)
	if err != nil || head.FormatVersion != 1 || head.Epoch != metadataInfo.MetadataJournalEpoch || head.Sequence != metadataInfo.MetadataJournalSequence {
		return fmt.Errorf("retained Metadata journal differs from its projection: %w", errors.Join(metadatastate.ErrSource, err))
	}
	expected := migrationNamespaceIntent{Version: 1, Path: path, Identity: o.Identity, InstanceID: o.InstanceID, KeyGeneration: o.KeyGeneration, Source: source}
	fee, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 64 << 10, Disk: migrationIntentLimit})
	if err != nil {
		return err
	}
	persisted := false
	defer func() {
		// Intent bytes survive Close; only temporary RAM can retire here.
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			if persisted {
				resultErr = errors.Join(resultErr, fee.ReleasePart(resourcebudget.Cost{RAM: 64 << 10}))
			} else {
				resultErr = errors.Join(resultErr, fee.Release())
			}
		}
	}()
	attempt := uint8(0)
	if o.Restart {
		if cfg.ResourceBudget.PersistentInventoryDiskBytes <= 0 {
			return errors.New("restart-preparation requires persistent_inventory_disk_bytes")
		}
		baseline, err := diskinventory.Reconcile(ctx, diskinventory.Options{DataDir: cfg.Storage.DataDir, Guard: guard, Budget: manager, Ceiling: cfg.ResourceBudget.PersistentInventoryDiskBytes, Deadline: time.Minute})
		if err != nil {
			return errors.Join(err, baseline.Close())
		}
		if err := baseline.Publish(); err != nil {
			return errors.Join(err, baseline.Close())
		}
		// A staging name is never startup authority. Before its receipt is
		// complete, even its provenance is unknown. The explicit offline restart
		// operation may preserve that opaque directory, but never read or delete
		// its contents, and never count it as an authenticated generation.
		if err := preserveMigrationStaging(ctx, cfg.Storage.DataDir, staging, path, expected, key, guard, checkpoint); err != nil {
			return err
		}
		var captured os.FileInfo
		captured, err = os.Lstat(path)
		var intent migrationNamespaceIntent
		var interrupted string
		if err == nil {
			intent, err = readMigrationNamespaceIntent(path, expected, key)
			if err != nil {
				return err
			}
			if intent.Attempt >= migrationInterruptedSlots {
				return errors.New("interrupted migration slots exhausted; retained generations require explicit archival")
			}
			interrupted = fmt.Sprintf("%s.interrupted-%d", path, intent.Attempt)
			if _, err := os.Lstat(interrupted); !errors.Is(err, os.ErrNotExist) {
				return errors.Join(os.ErrExist, err)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			named, err := os.Lstat(path)
			if err != nil || !os.SameFile(captured, named) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
				return errors.Join(ownedfile.ErrReplaced, err)
			}
			if err := renameMigrationWithoutReplace(path, interrupted); err != nil {
				return err
			}
			if err := checkpoint("candidate-preserved"); err != nil {
				return err
			}
			if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
				return err
			}
		} else if errors.Is(err, os.ErrNotExist) {
			// Resume an interrupted multi-name preservation after the candidate
			// rename. Pick only the latest fixed slot carrying this same receipt.
			for i := 0; i < migrationInterruptedSlots; i++ {
				slot := fmt.Sprintf("%s.interrupted-%d", path, i)
				if _, err := os.Lstat(slot); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return err
				}
				intent, err = readMigrationNamespaceIntent(slot, expected, key)
				if err != nil || int(intent.Attempt) != i {
					return errors.Join(metadatastate.ErrSource, err)
				}
				interrupted = slot
			}
		} else {
			return err
		}
		if interrupted != "" {
			attempt = intent.Attempt + 1
		}
		if attempt > migrationInterruptedSlots {
			return errors.New("interrupted migration slots exhausted; retained generations require explicit archival")
		}
		for _, name := range []string{"memory-state.bootstrap.pending", "active.seal.pending", "memory-audit-1.cache", "memory-governance-1.cache"} {
			original := filepath.Join(cfg.Storage.DataDir, name)
			info, err := os.Lstat(original)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return errors.Join(ownedfile.ErrReplaced, err)
			}
			if interrupted == "" {
				return fmt.Errorf("pending publication has no authenticated preserved generation: %w", metadatastate.ErrSource)
			}
			destination := filepath.Join(interrupted, name)
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				return errors.Join(os.ErrExist, err)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := renameMigrationWithoutReplace(original, destination); err != nil {
				return err
			}
			if err := checkpoint("pending-preserved:" + name); err != nil {
				return err
			}
			if err := errors.Join(durable.SyncDirectory(interrupted), durable.SyncDirectory(cfg.Storage.DataDir)); err != nil {
				return err
			}
		}
	} else {
		for _, name := range []string{"memory-state.bootstrap.pending", "active.seal.pending", "memory-audit-1.cache", "memory-governance-1.cache"} {
			if _, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, name)); !errors.Is(err, os.ErrNotExist) {
				return errors.Join(os.ErrExist, err)
			}
		}
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(os.ErrExist, err)
	}
	if err := checkpoint("before-staging-create"); err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		return err
	}
	persisted = true
	info, err := os.Lstat(staging)
	if err != nil {
		return err
	}
	expected.Device, expected.Inode, err = migrationDirectoryIdentity(info)
	if err != nil {
		return err
	}
	if err := checkpoint("staging-created"); err != nil {
		return err
	}
	namedStaging, err := os.Lstat(staging)
	if err != nil || !os.SameFile(info, namedStaging) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	expected.Attempt = attempt
	raw, err := json.Marshal(expected)
	if err != nil || len(raw)+sha256.Size > migrationIntentLimit {
		return errors.Join(metadatastate.ErrSource, err)
	}
	raw = append(raw, migrationIntentMAC(key, raw)...)
	f, owner, err := ownedfile.Create(filepath.Join(staging, "migration.intent"), os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	open := true
	defer func() {
		if open {
			resultErr = errors.Join(resultErr, ownedfile.CloseFile(f))
		}
	}()
	if err := checkpoint("intent-created"); err != nil {
		return err
	}
	// Two bounded writes expose the actual interrupted/partial-file branch.
	for i, part := range [][]byte{raw[:len(raw)/2], raw[len(raw)/2:]} {
		n, err := f.Write(part)
		if err != nil || n != len(part) {
			return errors.Join(err, io.ErrShortWrite)
		}
		phase := "intent-partially-written"
		if i == 1 {
			phase = "intent-written"
		}
		if err := checkpoint(phase); err != nil {
			return err
		}
	}
	if err := errors.Join(f.Sync(), owner.VerifyFile(f)); err != nil {
		return err
	}
	if err := checkpoint("intent-synced"); err != nil {
		return err
	}
	err = ownedfile.CloseFile(f)
	open = false
	if err != nil {
		return err
	}
	if err := durable.SyncDirectory(staging); err != nil {
		return err
	}
	if err := checkpoint("staging-synced"); err != nil {
		return err
	}
	if _, err := readMigrationNamespaceIntent(staging, expected, key); err != nil {
		return err
	}
	named, err := os.Lstat(staging)
	if err != nil || !os.SameFile(info, named) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := renameMigrationWithoutReplace(staging, path); err != nil {
		return err
	}
	if err := checkpoint("namespace-published"); err != nil {
		return err
	}
	published, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, published) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if _, err := readMigrationNamespaceIntent(path, expected, key); err != nil {
		return err
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return err
	}
	return checkpoint("namespace-synced")
}

func preserveMigrationStaging(ctx context.Context, dataDir, staging, path string, expected migrationNamespaceIntent, key []byte, guard *lock.Lock, checkpoint func(string) error) error {
	captured, err := os.Lstat(staging)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, _, err := migrationDirectoryIdentity(captured); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ambiguous published and staging namespaces: %w", errors.Join(os.ErrExist, err))
	}
	intent, receiptErr := readMigrationNamespaceIntent(staging, expected, key)
	// An uncertain actual receipt descriptor Close is a lifecycle debt, not
	// evidence that the namespace is simply unauthenticated. Stop and retain
	// the admission/lock rather than proceeding with another generation.
	if errors.Is(receiptErr, ownedfile.ErrRetirement) {
		return receiptErr
	}
	var destination string
	if receiptErr == nil {
		if intent.Attempt >= migrationInterruptedSlots {
			return errors.New("interrupted migration slots exhausted; retained generations require explicit archival")
		}
		destination = fmt.Sprintf("%s.interrupted-%d", path, intent.Attempt)
	} else {
		// Four fixed opaque preservation slots bound names and retry growth.
		// Their existence never contributes to generation/receipt selection.
		for i := 0; i < migrationInterruptedSlots; i++ {
			candidate := fmt.Sprintf("%s.unqualified-%d", path, i)
			if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
				destination = candidate
				break
			} else if err != nil {
				return err
			}
		}
		if destination == "" {
			return errors.New("unqualified migration slots exhausted; opaque candidates require explicit archival")
		}
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(os.ErrExist, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	named, err := os.Lstat(staging)
	if err != nil || !os.SameFile(captured, named) || !guard.CoversDataDirectory(dataDir) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := renameMigrationWithoutReplace(staging, destination); err != nil {
		return err
	}
	if err := checkpoint("staging-preserved"); err != nil {
		return err
	}
	return durable.SyncDirectory(dataDir)
}

// memoryMigrationNamespacePresent checks only fixed reserved names. Without a
// committed bootstrap, any of them means an explicit offline preparation may
// be interrupted: legacy startup must not mutate the retained source prefix.
// Neither existence nor this refusal authenticates any candidate's contents.
func memoryMigrationNamespacePresent(cfg config.Config) (bool, error) {
	path := filepath.Join(cfg.Storage.DataDir, "memory-state")
	names := []string{path, path + ".preparing"}
	for i := 0; i < migrationInterruptedSlots; i++ {
		names = append(names, fmt.Sprintf("%s.interrupted-%d", path, i), fmt.Sprintf("%s.unqualified-%d", path, i))
	}
	for _, name := range names {
		if _, err := os.Lstat(name); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}
