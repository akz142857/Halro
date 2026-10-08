package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type namespaceFirstRunKillInput struct {
	Config                           config.Config
	Backup                           string
	BackupKey, LedgerKey, JournalKey []byte
	Identity                         [32]byte
	InstanceID                       string
}

func TestMemoryNamespaceFirstRunRuntimeSIGKILLHelper(t *testing.T) {
	input := os.Getenv("HALRO_TEST_NAMESPACE_RUNTIME_KILL")
	if input == "" {
		return
	}
	var o namespaceFirstRunKillInput
	if err := json.Unmarshal([]byte(input), &o); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := resourcebudget.New(o.Config.ResourceBudget.Limits(o.Config.ResourcePressure.MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(o.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := backup.OpenVerified(ctx, o.Backup, o.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	err = PrepareMemoryRuntimeMigrationDirectory(ctx, o.Config, MemoryMigrationDirectoryOptions{
		Identity: o.Identity, InstanceID: o.InstanceID, KeyGeneration: 1, SourceJournalKey: o.JournalKey,
		PreparationHook: func(phase string) error {
			if phase == "staging-created" {
				if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				for {
					time.Sleep(time.Hour)
				}
			}
			return nil
		},
	}, archive, o.LedgerKey, m, guard)
	t.Fatal("earliest creation boundary was not reached", err)
}

func TestMemoryNamespaceEarliestSIGKILLCompletesActivationAndRuntimeBusiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	cfg := r.config
	instanceID := r.instanceID
	request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_before_migration", "chat")
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
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instanceID, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	selected, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("actual default selected Runtime", err)
	}
	defer selected.Close()
	if selected.selectedState == nil || selected.accounting != selected.selectedState.services.Accounting {
		t.Fatal("default Runtime bypassed selected roots")
	}
	if err := selected.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := selected.store.PutProject(ctx, domain.Project{ID: "prj_after_earliest_kill", Name: "Retained after restart", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	q, err := selected.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_after_earliest_kill", "chat")
	if err != nil {
		t.Fatal(err)
	}
	a, err := selected.accounting.ReserveAttempt(ctx, q, 20, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := selected.accounting.Settle(ctx, a, budget.Settlement{CommittedMicrosUSD: 9, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := selected.accounting.Finalize(ctx, q, "success"); err != nil {
		t.Fatal(err)
	}
	if err := selected.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := selected.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("actual second default Runtime", err)
	}
	defer again.Close()
	if err := again.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if project, err := again.store.GetProject(ctx, "prj_after_earliest_kill"); err != nil || project.Name != "Retained after restart" {
		t.Fatal("lost actual Runtime Metadata", project, err)
	}
	for id, cost := range map[string]int64{request.RequestID: 7, q.RequestID: 9} {
		summary, found, err := again.usage.disk.ReadRequest(ctx, id)
		if err != nil || !found || summary.CostMicrosUSD != cost {
			t.Fatal("lost actual Runtime Usage/accounting", summary, found, err)
		}
	}
	if err := again.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

// activateClosedRuntimeMemoryStateFixture builds the actual seven-root generation
// from a closed legacy Runtime. killFirst also exercises the pre-intent crash
// path. All real descriptors/owners/locks retire before returning the config.
// The pressure lease is a functional-test control, not capacity acceptance.
func activateClosedRuntimeMemoryStateFixture(t *testing.T, ctx context.Context, cfg config.Config, instanceID string, killFirst bool) config.Config {
	return activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instanceID, killFirst, nil)
}

func activateClosedRuntimeMemoryStateFixtureWithPressure(t *testing.T, ctx context.Context, cfg config.Config, instanceID string, killFirst bool, physicalPressure *config.ResourcePressure) config.Config {
	t.Helper()
	configPath, backupPath := filepath.Join(t.TempDir(), "config.yaml"), filepath.Join(t.TempDir(), "source.hmbk")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	backupKey := bytes.Repeat([]byte{0x39}, 32)
	backupConfig := configWithoutBudget(cfg)
	if cfg.ResourceBudget != nil && cfg.ResourceBudget.MemoryStateRequired {
		// Capacity/migration samples must include the actual bounded backup
		// path and its original shared configuration, not an unbudgeted prelude.
		backupConfig = cfg
	}
	if _, err := CreateBackup(ctx, backupConfig, configPath, backupPath, backupKey); err != nil {
		t.Fatal(err)
	}
	pressure, limits := runtimePressureFixture(), selectedRuntimeBudgetFixture()
	if cfg.ResourceBudget != nil {
		limits = *cfg.ResourceBudget
	}
	// This physical fixture declares two real phases. Legacy reconstruction
	// pays its mappings and only one request envelope; the aggregate completion
	// class belongs to selected owners that do not exist until activation.
	selectedCompletionRAM := int64(0)
	if limits.PhysicalTerminalReserve && !limits.MemoryStateRequired {
		selectedCompletionRAM = limits.CompletionRAMBytes
		limits.CompletionRAMBytes = 192<<20 + 8<<10
	}
	// Offline construction uses a fixed functional lease; Runtime still samples continuously.
	if physicalPressure != nil {
		pressure = *physicalPressure
	} else {
		pressure.MaxAge = config.Duration(time.Minute)
	}
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal("selected budget fixture", err)
	}
	manager, err := resourcebudget.New(limits.Limits(pressure.MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	if physicalPressure == nil {
		// Only functional fixtures can use this override. The physical capacity
		// experiment runs the same OS monitor as real selected startup.
		if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
			t.Fatal(err)
		}
	} else {
		sampler, err := resourcebudget.NewSampler(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		monitor, err := resourcebudget.NewMonitor(sampler, pressure.Policy(), pressure.SampleInterval.Value())
		if err != nil {
			t.Fatal(err)
		}
		if err := monitor.AttachManager(manager); err != nil {
			t.Fatal(err)
		}
		holder := &runtimeSelectedState{}
		defer func() {
			if holder.pressureCancel != nil {
				holder.pressureCancel()
				<-holder.pressureDone
			}
		}()
		if err := startSelectedPressureMonitor(ctx, cfg, manager, &resourcePressureRuntime{monitor: monitor}, holder); err != nil {
			t.Fatal("actual migration pressure monitor", err)
		}
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
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
	ledgerKey, err := loadLedgerHMACKey(old, v, master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(ledgerKey)
	journalKey, err := loadMetadataJournalHMACKey(old, v, master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(journalKey)
	journalPath := old.MetadataJournalPath()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err := backup.OpenVerified(ctx, backupPath, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	directory := memoryStateDirectory(cfg)
	identity := sha256.Sum256([]byte("independent-selected-runtime-fixture"))
	namespace := MemoryMigrationDirectoryOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: 1, SourceJournalKey: journalKey}
	input, err := json.Marshal(namespaceFirstRunKillInput{Config: cfg, Backup: backupPath, BackupKey: backupKey, LedgerKey: ledgerKey, JournalKey: journalKey, Identity: identity, InstanceID: instanceID})
	if err != nil {
		t.Fatal(err)
	}
	if killFirst {
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
		killCtx, killCancel := context.WithTimeout(ctx, 30*time.Second)
		child := exec.CommandContext(killCtx, os.Args[0], "-test.run=^TestMemoryNamespaceFirstRunRuntimeSIGKILLHelper$")
		child.Env = append(os.Environ(), "HALRO_TEST_NAMESPACE_RUNTIME_KILL="+string(input))
		output, err := child.CombinedOutput()
		killCancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("first-run helper did not reach actual SIGKILL: %v %s", err, output)
		}
		// Ordinary startup must stop before touching the original native input.
		metadataBefore, err := os.ReadFile(cfg.MetadataPath())
		if err != nil {
			t.Fatal(err)
		}
		journalBefore, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		pendingRuntime, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })})
		if pendingRuntime != nil {
			_ = pendingRuntime.Close()
		}
		if !errors.Is(err, ErrMemoryStateActivationPending) {
			t.Fatal("pre-seal startup did not refuse legacy mutation", err)
		}
		for _, source := range []struct {
			path   string
			before []byte
		}{{cfg.MetadataPath(), metadataBefore}, {journalPath, journalBefore}} {
			raw, err := os.ReadFile(source.path)
			if err != nil || !bytes.Equal(raw, source.before) {
				t.Fatal("refused Runtime modified retained native source", source.path, err)
			}
		}
		guard, err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Close()
		if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unqualified namespace was published", err)
		}
		if _, err := os.Lstat(filepath.Join(directory+".preparing", "migration.intent")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("helper crossed requested earliest boundary", err)
		}
		// A real process exited; create a fresh manager, rather than refund its
		// unknown persistent claims inside the publishing process.
		manager, err = resourcebudget.New(limits.Limits(pressure.MaxAge.Value()))
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
			t.Fatal(err)
		}
		namespace.Restart = true
		if err := PrepareMemoryRuntimeMigrationDirectory(ctx, cfg, namespace, archive, ledgerKey, manager, guard); err != nil {
			t.Fatal("earliest namespace restart", err)
		}
		if _, err := os.Lstat(directory + ".unqualified-0"); err != nil {
			t.Fatal("opaque earliest namespace was not preserved", err)
		}
	} else {
		if err := PrepareMemoryRuntimeMigrationDirectory(ctx, cfg, namespace, archive, ledgerKey, manager, guard); err != nil {
			t.Fatal(err)
		}
	}
	p, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, ledgerKey, journalKey, cfg.Storage.DataDir, directory, guard, metadatastate.BaselineOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: 1, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: 8 << 20}, limits.MetadataJournalDiskBytes)
	if err != nil {
		t.Fatal("authenticated backup reconstruction", err)
	}
	defer p.Close()
	activation, err := ActivatePreparedMemoryRuntime(ctx, cfg, p, guard, manager, ledgerKey, journalKey)
	if activation != nil {
		defer activation.Close()
	}
	if err != nil {
		t.Fatal("all seven roots/common activation", err)
	}
	if err := activation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if selectedCompletionRAM > 0 {
		// Activation above authenticated and published all seven actual roots.
		// The subsequent Runtime now validates the selected aggregate profile.
		limits.MemoryStateRequired = true
		limits.CompletionRAMBytes = selectedCompletionRAM
		if err := cfg.Validate(config.LoadOptions{}); err != nil {
			t.Fatal("selected physical phase budget", err)
		}
	}
	return cfg
}

func TestMemoryNamespacePendingNamesRefuseLegacyStartupWithoutTrust(t *testing.T) {
	for _, suffix := range []string{"", ".preparing", ".interrupted-0", ".interrupted-1", ".interrupted-2", ".interrupted-3", ".unqualified-0", ".unqualified-1", ".unqualified-2", ".unqualified-3"} {
		t.Run("reserved"+suffix, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Storage.DataDir = t.TempDir()
			path := filepath.Join(cfg.Storage.DataDir, "memory-state"+suffix)
			// Even foreign/partial regular bytes are grounds to refuse. This
			// check must not parse them or credit them with migration authority.
			if err := os.WriteFile(path, []byte("unqualified candidate"), 0600); err != nil {
				t.Fatal(err)
			}
			pending, err := memoryMigrationNamespacePresent(cfg)
			if err != nil || !pending {
				t.Fatal("fixed namespace was missed", pending, err)
			}
			if active, err := memoryRuntimeActivated(cfg); active || !errors.Is(err, ErrMemoryStateActivationPending) {
				t.Fatal("legacy fallback accepted interrupted namespace", active, err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != "unqualified candidate" {
				t.Fatal("eligibility check changed candidate", err)
			}
		})
	}
	t.Run("virgin-before-mkdir", func(t *testing.T) {
		cfg := config.Config{}
		cfg.Storage.DataDir = t.TempDir()
		if pending, err := memoryMigrationNamespacePresent(cfg); err != nil || pending {
			t.Fatal(pending, err)
		}
		if active, err := memoryRuntimeActivated(cfg); err != nil || active {
			t.Fatal(active, err)
		}
	})
}
