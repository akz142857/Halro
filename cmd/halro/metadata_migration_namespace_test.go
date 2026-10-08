package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

func migrationRestartManager(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1 << 30, Disk: 1 << 30}, RecoveryReserve: resourcebudget.Cost{RAM: 128 << 20, Disk: 128 << 20}, MaxClaims: 64, RecoveryClaims: 8, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[resourcebudget.Recovery] = l.RecoveryReserve
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: int64(l.MaxClaims) * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

func openMigrationRestartArchive(t *testing.T, a metadataMigrationArguments, manager *resourcebudget.Manager) (context.Context, *backup.VerifiedArchive, []byte) {
	t.Helper()
	ctx := context.Background()
	b, err := backup.NewWorkspaceBudget(ctx, 16<<20, func(ctx context.Context, size int64) (func() error, error) {
		p, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 20, Disk: size})
		if err != nil {
			return nil, err
		}
		return p.Release, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = b.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key, err := metadataMigrationKeyFile(a.backupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	archive, err := backup.OpenVerified(ctx, a.backup, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := archive.Close(); err != nil {
			t.Error(err)
		}
	})
	ledgerKey, err := metadataMigrationKeyFile(a.ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(ledgerKey) })
	return ctx, archive, ledgerKey
}

func TestMigrationNamespaceKilledPreparationHelper(t *testing.T) {
	input := os.Getenv("HALRO_TEST_MIGRATION_NAMESPACE_KILL")
	if input == "" {
		return
	}
	var request struct {
		Config    config.Config
		Arguments metadataMigrationArguments
	}
	// Arguments use the test's separate JSON envelope because CLI flags are
	// private fields, not an exported configuration serialization contract.
	var envelope struct {
		Config                                               config.Config
		Backup, BackupKey, LedgerKey, JournalKey, InstanceID string
		Identity                                             [32]byte
		Phase                                                string
		Restart                                              bool
	}
	if err := json.Unmarshal([]byte(input), &envelope); err != nil {
		t.Fatal(err)
	}
	request.Config = envelope.Config
	request.Arguments = metadataMigrationArguments{activateRuntime: true, backup: envelope.Backup, backupKey: envelope.BackupKey, ledgerKey: envelope.LedgerKey, sourceJournalKey: envelope.JournalKey, output: filepath.Join(envelope.Config.Storage.DataDir, "memory-state"), instanceID: envelope.InstanceID, identity: envelope.Identity, keyGeneration: 1}
	m := migrationRestartManager(t)
	ctx, archive, key := openMigrationRestartArchive(t, request.Arguments, m)
	guard, err := lock.Acquire(request.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Phase != "" {
		journalKey, err := metadataMigrationKeyFile(request.Arguments.sourceJournalKey)
		if err != nil {
			t.Fatal(err)
		}
		err = app.PrepareMemoryRuntimeMigrationDirectory(ctx, request.Config, app.MemoryMigrationDirectoryOptions{
			Identity: envelope.Identity, InstanceID: envelope.InstanceID, KeyGeneration: 1, SourceJournalKey: journalKey, Restart: envelope.Restart,
			PreparationHook: func(phase string) error {
				if phase == envelope.Phase {
					if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
						t.Fatal(err)
					}
					for {
						time.Sleep(time.Hour)
					} // Wait for the actual signal; do not race Fatal against it.
				}
				return nil
			},
		}, archive, key, m, guard)
		t.Fatalf("did not reach namespace crash boundary %s: %v", envelope.Phase, err)
	}
	if err := prepareRuntimeMigrationDirectory(ctx, request.Config, request.Arguments, archive, key, m, guard); err != nil {
		t.Fatal(err)
	}
	// Complete the actual Metadata preparation, then die before the other six
	// roots/common publication. Neither deferred cleanup nor guard Close runs.
	journalKey, err := metadataMigrationKeyFile(request.Arguments.sourceJournalKey)
	if err != nil {
		t.Fatal(err)
	}
	o := metadatastate.BaselineOptions{Identity: request.Arguments.identity, InstanceID: request.Arguments.instanceID, KeyGeneration: 1, Budget: m, MaxSourceBytes: 32 << 20, MaxDiskBytes: 32 << 20, MaxAppendBytes: 8 << 20}
	if _, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, key, journalKey, request.Config.Storage.DataDir, request.Arguments.output, guard, o, 16<<20); err != nil {
		t.Fatal(err)
	}
	// A kill during encrypted bootstrap creation may leave opaque partial
	// bytes. Restart preserves them; it never uses them as root authority.
	if err := os.WriteFile(filepath.Join(request.Config.Storage.DataDir, "memory-state.bootstrap.pending"), []byte("interrupted encrypted capsule"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	} // The signal is asynchronous; never turn it into an exit-code failure.
}

func TestMigrationNamespaceRealSIGKILLPreservesOldCandidateAndRebuildsAuthenticatedBackup(t *testing.T) {
	cfg, a, _, metadata, journal := metadataMigrationCommandFixture(t)
	cfg.ResourceBudget.PersistentInventoryDiskBytes = 64 << 20
	a.activateRuntime, a.output = true, filepath.Join(cfg.Storage.DataDir, "memory-state")
	dbBefore, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	journalBefore, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(struct {
		Config                                               config.Config
		Backup, BackupKey, LedgerKey, JournalKey, InstanceID string
		Identity                                             [32]byte
	}{cfg, a.backup, a.backupKey, a.ledgerKey, a.sourceJournalKey, a.instanceID, a.identity})
	if err != nil {
		t.Fatal(err)
	}
	killCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(killCtx, os.Args[0], "-test.run=^TestMigrationNamespaceKilledPreparationHelper$")
	child.Env = append(os.Environ(), "HALRO_TEST_MIGRATION_NAMESPACE_KILL="+string(input))
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("helper did not reach real SIGKILL: %v %s", err, output)
	}
	oldPages, err := os.ReadFile(filepath.Join(a.output, "metadata-root", "pages"))
	if err != nil {
		t.Fatal("helper did not build real Metadata root", err)
	}
	m := migrationRestartManager(t)
	ctx, archive, key := openMigrationRestartArchive(t, a, m)
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal("SIGKILL did not release actual process lock", err)
	}
	defer guard.Close()
	a.restartPreparation = true
	if err := prepareRuntimeMigrationDirectory(ctx, cfg, a, archive, key, m, guard); err != nil {
		t.Fatal(err)
	}
	interrupted := a.output + ".interrupted-0"
	if actual, err := os.ReadFile(filepath.Join(interrupted, "metadata-root", "pages")); err != nil || !bytes.Equal(actual, oldPages) {
		t.Fatal("preservation changed interrupted bytes", err)
	}
	if raw, err := os.ReadFile(filepath.Join(interrupted, "memory-state.bootstrap.pending")); err != nil || string(raw) != "interrupted encrypted capsule" {
		t.Fatal("restart lost partial publication bytes", err)
	}
	journalKey, err := metadataMigrationKeyFile(a.sourceJournalKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(journalKey)
	o := metadatastate.BaselineOptions{Identity: a.identity, InstanceID: a.instanceID, KeyGeneration: a.keyGeneration, Budget: m, MaxSourceBytes: 32 << 20, MaxDiskBytes: 32 << 20, MaxAppendBytes: 8 << 20}
	p, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, key, journalKey, cfg.Storage.DataDir, a.output, guard, o, 16<<20)
	if err != nil {
		t.Fatal("fresh preparation did not use original authenticated backup", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if p.Receipt.Root.ID == ([16]byte{}) || p.Receipt.Evidence.CiphertextDigest == ([32]byte{}) {
		t.Fatal(p.Receipt)
	}
	for _, source := range []struct {
		Path   string
		Before []byte
	}{{metadata, dbBefore}, {journal, journalBefore}} {
		if actual, err := os.ReadFile(source.Path); err != nil || !bytes.Equal(actual, source.Before) {
			t.Fatal("restart changed original native source", source.Path, err)
		}
	}
}

func TestMigrationNamespaceRejectsForeignSourceReplacementAndCommittedSeal(t *testing.T) {
	for _, fault := range []string{"wrong-key", "wrong-identity", "tampered-intent", "replaced-directory", "committed-seal", "occupied-slot", "inventory-limit", "source-changed"} {
		t.Run(fault, func(t *testing.T) {
			cfg, a, _, _, _ := metadataMigrationCommandFixture(t)
			cfg.ResourceBudget.PersistentInventoryDiskBytes = 64 << 20
			a.activateRuntime, a.output = true, filepath.Join(cfg.Storage.DataDir, "memory-state")
			m := migrationRestartManager(t)
			ctx, archive, key := openMigrationRestartArchive(t, a, m)
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			if err := prepareRuntimeMigrationDirectory(ctx, cfg, a, archive, key, m, guard); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(a.output, "preserve"), []byte("interrupted bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "wrong-key":
				key = bytes.Repeat([]byte{91}, 32)
			case "wrong-identity":
				a.identity[0] ^= 1
			case "tampered-intent":
				if err := os.WriteFile(filepath.Join(a.output, "migration.intent"), []byte("foreign receipt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "replaced-directory":
				if err := os.Rename(a.output, a.output+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(a.output, 0700); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(a.output+".original", "migration.intent"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(a.output, "migration.intent"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(a.output, "preserve"), []byte("interrupted bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			case "committed-seal":
				if err := os.WriteFile(filepath.Join(cfg.Storage.DataDir, "active.seal"), []byte("preserved authority"), 0600); err != nil {
					t.Fatal(err)
				}
			case "occupied-slot":
				if err := os.Mkdir(a.output+".interrupted-0", 0700); err != nil {
					t.Fatal(err)
				}
			case "inventory-limit":
				cfg.ResourceBudget.PersistentInventoryDiskBytes = 1
			case "source-changed":
				s, err := boltstore.Open(cfg.MetadataPath())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.AttachMetadataJournal(bytes.Repeat([]byte{73}, 32), "actual-cli-migration"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PutProject(ctx, domain.Project{ID: "prj_new_native", Name: "changed source", Enabled: true}, 0, nil); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			a.restartPreparation = true
			if err := prepareRuntimeMigrationDirectory(ctx, cfg, a, archive, key, m, guard); err == nil {
				t.Fatal("unqualified restart accepted")
			}
			if raw, err := os.ReadFile(filepath.Join(a.output, "preserve")); err != nil || string(raw) != "interrupted bytes" {
				t.Fatal("refusal moved or changed candidate", err)
			}
		})
	}
}

func killMigrationNamespaceBoundary(t *testing.T, cfg config.Config, a metadataMigrationArguments, phase string, restart bool) {
	t.Helper()
	input, err := json.Marshal(struct {
		Config                                               config.Config
		Backup, BackupKey, LedgerKey, JournalKey, InstanceID string
		Identity                                             [32]byte
		Phase                                                string
		Restart                                              bool
	}{cfg, a.backup, a.backupKey, a.ledgerKey, a.sourceJournalKey, a.instanceID, a.identity, phase, restart})
	if err != nil {
		t.Fatal(err)
	}
	killCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(killCtx, os.Args[0], "-test.run=^TestMigrationNamespaceKilledPreparationHelper$")
	child.Env = append(os.Environ(), "HALRO_TEST_MIGRATION_NAMESPACE_KILL="+string(input))
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("boundary %s did not reach real SIGKILL: %v %s", phase, err, output)
	}
}

func TestMigrationNamespaceFirstRunRealSIGKILLAllPublicationBoundaries(t *testing.T) {
	for _, phase := range []string{"before-staging-create", "staging-created", "intent-created", "intent-partially-written", "intent-written", "intent-synced", "staging-synced", "namespace-published", "namespace-synced"} {
		t.Run(phase, func(t *testing.T) {
			cfg, a, _, metadata, journal := metadataMigrationCommandFixture(t)
			cfg.ResourceBudget.PersistentInventoryDiskBytes = 64 << 20
			a.activateRuntime, a.output, a.restartPreparation = true, filepath.Join(cfg.Storage.DataDir, "memory-state"), true
			dbBefore, err := os.ReadFile(metadata)
			if err != nil {
				t.Fatal(err)
			}
			journalBefore, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			killMigrationNamespaceBoundary(t, cfg, a, phase, false)
			// Read only the fixed receipt file to establish exact preserved bytes.
			stagedIntent, _ := os.ReadFile(filepath.Join(a.output+".preparing", "migration.intent"))
			m := migrationRestartManager(t)
			ctx, archive, key := openMigrationRestartArchive(t, a, m)
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			if err := prepareRuntimeMigrationDirectory(ctx, cfg, a, archive, key, m, guard); err != nil {
				t.Fatal(err)
			}
			if phase == "staging-created" || phase == "intent-created" || phase == "intent-partially-written" {
				preserved := a.output + ".unqualified-0"
				if _, err := os.Lstat(preserved); err != nil {
					t.Fatal("lost unqualified candidate", err)
				}
				if phase != "staging-created" {
					raw, err := os.ReadFile(filepath.Join(preserved, "migration.intent"))
					if err != nil || !bytes.Equal(raw, stagedIntent) {
						t.Fatal("opaque receipt was changed", err)
					}
				}
			}
			journalKey, err := metadataMigrationKeyFile(a.sourceJournalKey)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(journalKey)
			o := metadatastate.BaselineOptions{Identity: a.identity, InstanceID: a.instanceID, KeyGeneration: a.keyGeneration, Budget: m, MaxSourceBytes: 32 << 20, MaxDiskBytes: 32 << 20, MaxAppendBytes: 8 << 20}
			p, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, key, journalKey, cfg.Storage.DataDir, a.output, guard, o, 16<<20)
			if err != nil {
				t.Fatal("real Metadata rebuild failed", err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			for _, source := range []struct {
				path   string
				before []byte
			}{{metadata, dbBefore}, {journal, journalBefore}} {
				raw, err := os.ReadFile(source.path)
				if err != nil || !bytes.Equal(raw, source.before) {
					t.Fatal("original source changed", source.path, err)
				}
			}
		})
	}
}

func TestMigrationNamespaceRestartRealSIGKILLAllPreservationBoundaries(t *testing.T) {
	for _, phase := range []string{"staging-preserved", "candidate-preserved", "pending-preserved:memory-state.bootstrap.pending", "pending-preserved:active.seal.pending", "pending-preserved:memory-audit-1.cache", "pending-preserved:memory-governance-1.cache"} {
		t.Run(phase, func(t *testing.T) {
			cfg, a, _, _, _ := metadataMigrationCommandFixture(t)
			cfg.ResourceBudget.PersistentInventoryDiskBytes = 64 << 20
			a.activateRuntime, a.output, a.restartPreparation = true, filepath.Join(cfg.Storage.DataDir, "memory-state"), true
			pending := []string{"memory-state.bootstrap.pending", "active.seal.pending", "memory-audit-1.cache", "memory-governance-1.cache"}
			if phase == "staging-preserved" {
				killMigrationNamespaceBoundary(t, cfg, a, "intent-partially-written", false)
			} else {
				killMigrationNamespaceBoundary(t, cfg, a, "namespace-synced", false)
				for _, name := range pending {
					if err := os.WriteFile(filepath.Join(cfg.Storage.DataDir, name), []byte(name), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			killMigrationNamespaceBoundary(t, cfg, a, phase, true)
			m := migrationRestartManager(t)
			ctx, archive, key := openMigrationRestartArchive(t, a, m)
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			if err := prepareRuntimeMigrationDirectory(ctx, cfg, a, archive, key, m, guard); err != nil {
				t.Fatal(err)
			}
			if phase != "staging-preserved" {
				for _, name := range pending {
					raw, err := os.ReadFile(filepath.Join(a.output+".interrupted-0", name))
					if err != nil || string(raw) != name {
						t.Fatal("interrupted preservation changed bytes", name, err)
					}
				}
			}
		})
	}
}

func TestMigrationNamespaceOpaqueCandidatesArePreservedWithoutAuthority(t *testing.T) {
	for _, fault := range []string{"foreign-private-staging", "foreign-child-symlink", "symlink-staging", "public-staging", "exhausted-opaque-slots", "foreign-pending-without-generation"} {
		t.Run(fault, func(t *testing.T) {
			cfg, a, _, _, _ := metadataMigrationCommandFixture(t)
			cfg.ResourceBudget.PersistentInventoryDiskBytes = 64 << 20
			a.activateRuntime, a.output, a.restartPreparation = true, filepath.Join(cfg.Storage.DataDir, "memory-state"), true
			staging := a.output + ".preparing"
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("do not read, delete or authenticate"), 0600); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "symlink-staging":
				if err := os.Symlink(filepath.Dir(outside), staging); err != nil {
					t.Fatal(err)
				}
			case "public-staging":
				if err := os.Mkdir(staging, 0755); err != nil {
					t.Fatal(err)
				}
			case "foreign-pending-without-generation":
				if err := os.WriteFile(filepath.Join(cfg.Storage.DataDir, "active.seal.pending"), []byte("foreign pending"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(staging, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(staging, "migration.intent"), []byte("unqualified foreign intent"), 0600); err != nil {
					t.Fatal(err)
				}
				if fault == "foreign-child-symlink" {
					if err := os.Symlink(outside, filepath.Join(staging, "foreign-content")); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(staging, "foreign-content"), []byte("opaque bytes"), 0600); err != nil {
					t.Fatal(err)
				}
				if fault == "exhausted-opaque-slots" {
					for i := 0; i < 4; i++ {
						if err := os.Mkdir(fmt.Sprintf("%s.unqualified-%d", a.output, i), 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			m := migrationRestartManager(t)
			ctx, archive, key := openMigrationRestartArchive(t, a, m)
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			err = prepareRuntimeMigrationDirectory(ctx, cfg, a, archive, key, m, guard)
			if fault == "foreign-private-staging" {
				if err != nil {
					t.Fatal("explicit opaque preservation failed", err)
				}
				raw, err := os.ReadFile(filepath.Join(a.output+".unqualified-0", "migration.intent"))
				if err != nil || string(raw) != "unqualified foreign intent" {
					t.Fatal("opaque bytes changed", err)
				}
				content, err := os.ReadFile(filepath.Join(a.output+".unqualified-0", "foreign-content"))
				if err != nil || string(content) != "opaque bytes" {
					t.Fatal("opaque content changed", err)
				}
				if _, err := os.Lstat(a.output + ".interrupted-0"); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("foreign receipt obtained generation authority", err)
				}
			} else if err == nil {
				t.Fatal("unsafe candidate accepted", fault)
			}
			raw, err := os.ReadFile(outside)
			if err != nil || string(raw) != "do not read, delete or authenticate" {
				t.Fatal("foreign source changed", err)
			}
		})
	}
}

func TestMigrationNamespaceRejectsReplacementDuringPublication(t *testing.T) {
	for _, phase := range []string{"staging-created", "namespace-published"} {
		t.Run(phase, func(t *testing.T) {
			cfg, a, _, _, _ := metadataMigrationCommandFixture(t)
			m := migrationRestartManager(t)
			ctx, archive, key := openMigrationRestartArchive(t, a, m)
			journalKey, err := metadataMigrationKeyFile(a.sourceJournalKey)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(journalKey)
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			path := filepath.Join(cfg.Storage.DataDir, "memory-state")
			if phase == "staging-created" {
				path += ".preparing"
			}
			replaced := false
			err = app.PrepareMemoryRuntimeMigrationDirectory(ctx, cfg, app.MemoryMigrationDirectoryOptions{
				Identity: a.identity, InstanceID: a.instanceID, KeyGeneration: a.keyGeneration, SourceJournalKey: journalKey,
				PreparationHook: func(actual string) error {
					if actual != phase {
						return nil
					}
					if err := os.Rename(path, path+".original"); err != nil {
						return err
					}
					if err := os.Mkdir(path, 0700); err != nil {
						return err
					}
					replaced = true
					return os.WriteFile(filepath.Join(path, "foreign"), []byte("preserved replacement"), 0600)
				},
			}, archive, key, m, guard)
			if err == nil || !replaced {
				t.Fatal("foreign publication replacement accepted", err, replaced)
			}
			if raw, err := os.ReadFile(filepath.Join(path, "foreign")); err != nil || string(raw) != "preserved replacement" {
				t.Fatal("foreign replacement changed", err)
			}
			if _, err := os.Lstat(filepath.Join(path, "migration.intent")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("foreign replacement received an authenticated intent", err)
			}
			if _, err := os.Lstat(path + ".original"); err != nil {
				t.Fatal("original inode was removed", err)
			}
		})
	}
}
