//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
	"golang.org/x/sys/unix"
)

// The offline Standalone migration (`halro metadata migrate --activate-runtime`)
// at actual ENOSPC. At each boundary of the namespace, the Metadata
// reconstruction and the seven-root activation, a foreign file fills the
// isolated ext4. The migration must finish inside space it already holds or
// fail with that ENOSPC, leave no owned temporary and leave the retained
// native Metadata and journal byte-identical. While migration names exist,
// ordinary startup must refuse. Once the fill is freed, the documented
// operator recovery must complete: --resume-activation after the common seal,
// otherwise --restart-preparation from the same backup. The default Runtime
// then opens with the business settled before the migration.
func TestStandaloneMigrationAtActualENOSPCRecovers(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires owned isolated ext4 fixture")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil || st.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 required", err)
	}
	t.Setenv("TMPDIR", root)
	namespace := []string{"before-staging-create", "intent-created", "staging-synced", "namespace-synced"}
	activation := []string{"sources-qualified", "roots-built", "bootstrap-pending-synced", "startup-caches-persisted", "common-seal-published", "bootstrap-activated"}
	phases := append(append(append([]string{}, namespace...), "metadata-preparation"), activation...)
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			f := standaloneMigrationENOSPCFixture(t, ctx)
			fill := filepath.Join(root, "foreign-migration-fill-"+phase)
			defer os.Remove(fill)
			filled := false
			hook := func(name string) error {
				if name != phase {
					return nil
				}
				filled = true
				return exhaustHAPhysicalFixture(fill, haPhysicalFixtureFillBound)
			}
			err := f.migrate(t, ctx, false, hook)
			if !filled {
				t.Fatal("migration never reached the phase", err)
			}
			failed := err != nil
			if failed && !errors.Is(err, syscall.ENOSPC) {
				t.Fatal("migration at actual ENOSPC failed otherwise", err)
			}
			if left := haPhysicalOwnedTemporaries(t, f.cfg.Storage.DataDir); len(left) != 0 {
				t.Fatal("migration at actual ENOSPC left owned temporaries", left)
			}
			f.assertSourcesUnchanged(t)
			if err := os.Remove(fill); err != nil {
				t.Fatal(err)
			}
			recovery := "none"
			if failed {
				if f.migrationNamesExist(t) {
					r, err := OpenWithOptions(ctx, f.cfg, f.logger, f.open)
					if err == nil {
						_ = r.Close()
						t.Fatal("ordinary startup opened an interrupted migration")
					}
					f.assertSourcesUnchanged(t)
				}
				if _, err := os.Lstat(filepath.Join(f.cfg.Storage.DataDir, "active.seal")); err == nil {
					recovery = "resume-activation"
					m := f.manager(t)
					guard, err := lock.Acquire(f.cfg.Storage.DataDir)
					if err != nil {
						t.Fatal(err)
					}
					_, err = ResumeMemoryRuntimeActivation(ctx, f.cfg, guard, m)
					if err := errors.Join(err, guard.Close()); err != nil {
						t.Fatal("resume after space returned", err)
					}
				} else {
					recovery = "restart-preparation"
					if err := f.migrate(t, ctx, true, nil); err != nil {
						t.Fatal("restart after space returned", err)
					}
				}
			}
			f.assertSourcesUnchanged(t)
			f.assertBusiness(t, ctx)
			t.Log(phase, "failed at ENOSPC", failed, "recovery", recovery)
		})
	}
}

type standaloneMigrationENOSPC struct {
	cfg                           config.Config
	logger                        *slog.Logger
	open                          OpenOptions
	archivePath                   string
	backupKey, ledgerKey, jKey    []byte
	identity                      [32]byte
	instanceID, requestID         string
	metadataBefore, journalBefore []byte
	journalPath                   string
}

func standaloneMigrationENOSPCFixture(t *testing.T, ctx context.Context) *standaloneMigrationENOSPC {
	t.Helper()
	r, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_before_enospc_migration", "chat")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := r.accounting.ReserveAttempt(ctx, request, 20, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 7, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	cfg, instanceID := r.config, r.instanceID
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	f := &standaloneMigrationENOSPC{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), instanceID: instanceID, requestID: request.RequestID, identity: sha256.Sum256([]byte("independent-enospc-migration-fixture"))}
	f.open = OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	f.archivePath = filepath.Join(t.TempDir(), "source.hmbk")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.backupKey = bytes.Repeat([]byte{0x3a}, 32)
	if _, err := CreateBackup(ctx, configWithoutBudget(cfg), configPath, f.archivePath, f.backupKey); err != nil {
		t.Fatal(err)
	}
	limits, pressure := selectedRuntimeBudgetFixture(), runtimePressureFixture()
	pressure.MaxAge = config.Duration(time.Minute)
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal("selected budget fixture", err)
	}
	f.cfg = cfg
	old, err := openCommandMetadataReadOnly(ctx, configWithoutBudget(cfg), cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	master, err := unlockMasterKey(ctx, cfg, old)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	v, err := vault.New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if f.ledgerKey, err = loadLedgerHMACKey(old, v, master); err != nil {
		t.Fatal(err)
	}
	if f.jKey, err = loadMetadataJournalHMACKey(old, v, master); err != nil {
		t.Fatal(err)
	}
	f.journalPath = old.MetadataJournalPath()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if f.metadataBefore, err = os.ReadFile(cfg.MetadataPath()); err != nil {
		t.Fatal(err)
	}
	if f.journalBefore, err = os.ReadFile(f.journalPath); err != nil {
		t.Fatal(err)
	}
	return f
}

// manager is fresh per attempt: a failed attempt keeps its persistent fees, as
// a real failed process exits holding them.
func (f *standaloneMigrationENOSPC) manager(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	m, err := resourcebudget.New(f.cfg.ResourceBudget.Limits(f.cfg.ResourcePressure.MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

// migrate follows the command order: namespace, Metadata reconstruction from
// the backup, then activation. hook fills at the named boundary.
func (f *standaloneMigrationENOSPC) migrate(t *testing.T, ctx context.Context, restart bool, hook func(string) error) (resultErr error) {
	t.Helper()
	if hook == nil {
		hook = func(string) error { return nil }
	}
	m := f.manager(t)
	guard, err := lock.Acquire(f.cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	archive, err := backup.OpenVerified(ctx, f.archivePath, f.backupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { resultErr = errors.Join(resultErr, archive.Close()) }()
	limits := f.cfg.ResourceBudget
	if err := PrepareMemoryRuntimeMigrationDirectory(ctx, f.cfg, MemoryMigrationDirectoryOptions{Identity: f.identity, InstanceID: f.instanceID, KeyGeneration: 1, SourceJournalKey: f.jKey, Restart: restart, PreparationHook: hook}, archive, f.ledgerKey, m, guard); err != nil {
		return err
	}
	if err := hook("metadata-preparation"); err != nil {
		return err
	}
	p, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, f.ledgerKey, f.jKey, f.cfg.Storage.DataDir, memoryStateDirectory(f.cfg), guard, metadatastate.BaselineOptions{Identity: f.identity, InstanceID: f.instanceID, KeyGeneration: 1, Budget: m, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: 8 << 20}, limits.MetadataJournalDiskBytes)
	if err != nil {
		if p != nil {
			err = errors.Join(err, p.Close())
		}
		return err
	}
	a, err := activatePreparedMemoryRuntime(ctx, f.cfg, p, guard, m, f.ledgerKey, f.jKey, hook)
	if err != nil {
		// A failed activation already joined its own Close.
		return err
	}
	return a.Close()
}

func (f *standaloneMigrationENOSPC) migrationNamesExist(t *testing.T) bool {
	t.Helper()
	data := f.cfg.Storage.DataDir
	for _, name := range []string{"memory-state", "memory-state.preparing", "memory-state.bootstrap", "memory-state.bootstrap.pending", "active.seal", "active.seal.pending"} {
		if _, err := os.Lstat(filepath.Join(data, name)); err == nil {
			return true
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return false
}

func (f *standaloneMigrationENOSPC) assertSourcesUnchanged(t *testing.T) {
	t.Helper()
	for _, source := range []struct {
		path   string
		before []byte
	}{{f.cfg.MetadataPath(), f.metadataBefore}, {f.journalPath, f.journalBefore}} {
		raw, err := os.ReadFile(source.path)
		if err != nil || !bytes.Equal(raw, source.before) {
			t.Fatal("retained native source changed", source.path, err)
		}
	}
}

func (f *standaloneMigrationENOSPC) assertBusiness(t *testing.T, ctx context.Context) {
	t.Helper()
	r, err := OpenWithOptions(ctx, f.cfg, f.logger, f.open)
	if err != nil {
		t.Fatal("default Runtime after recovered migration", err)
	}
	defer r.Close()
	if r.selectedState == nil || r.accounting != r.selectedState.services.Accounting {
		t.Fatal("default Runtime bypassed selected roots")
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	summary, found, err := r.usage.disk.ReadRequest(ctx, f.requestID)
	if err != nil || !found || summary.CostMicrosUSD != 7 {
		t.Fatal(fmt.Sprint("lost settled business across migration ", summary, found), err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
