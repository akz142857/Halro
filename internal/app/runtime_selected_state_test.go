package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/governance"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/startupaudit"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

func selectedRuntimeBudgetFixture() config.ResourceBudget {
	b := runtimeRAMBudgetFixture()
	b.TotalRAMBytes, b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes = 8<<30, 2<<30, 2<<30
	b.ConfigurationRAMBytes, b.ActiveRAMBytes, b.CacheRAMBytes, b.IORAMBytes, b.BackgroundRAMBytes, b.CompletionRAMBytes = 256<<20, 256<<20, 256<<20, 512<<20, 1<<30, 256<<20
	b.MaxClaims, b.RecoveryClaims, b.MaxOwners = 128, 32, 16
	b.ControlRAMBytes = int64(b.MaxClaims) * resourcebudget.ClaimRAMBytes
	b.TotalDiskBytes, b.BackgroundDiskBytes, b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 32<<30, 16<<30, 8<<30, 8<<30
	b.ConfigurationDiskBytes, b.ActiveDiskBytes, b.CacheDiskBytes, b.IODiskBytes, b.CompletionDiskBytes = 128<<20, 128<<20, 128<<20, 1<<30, 128<<20
	b.PersistentInventoryDiskBytes, b.LedgerAppendDiskBytes = 512<<20, 64<<20
	b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.GovernanceProjectionDiskBytes = 64<<20, 64<<20, 64<<20
	b.LedgerSegmentIndexDiskBytes = 16 << 20
	b.MetadataStoreDiskBytes, b.MetadataJournalDiskBytes, b.AuditLogDiskBytes, b.GovernanceLogDiskBytes = 64<<20, 64<<20, 64<<20, 64<<20
	b.UsageArchiveDiskBytes, b.BackupWorkspaceDiskBytes, b.UsageArchiveReadRAMBytes = 64<<20, 128<<20, 8<<20
	return b
}

func TestDefaultRuntimeUsesActivatedSevenRootsAndReopensActualBusiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	cfg := r.config
	instanceID := r.instanceID
	originalAdmin, err := r.store.AdminBootstrapCompletion(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	configPath, backupPath := filepath.Join(t.TempDir(), "config.yaml"), filepath.Join(t.TempDir(), "source.hmbk")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	backupKey := bytes.Repeat([]byte{0x39}, 32)
	if _, err := CreateBackup(ctx, cfg, configPath, backupPath, backupKey); err != nil {
		t.Fatal(err)
	}
	pressure, limits := runtimePressureFixture(), selectedRuntimeBudgetFixture()
	// Offline construction uses a fixed functional lease; Runtime still samples continuously.
	pressure.MaxAge = config.Duration(time.Minute)
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal("selected budget fixture", err)
	}
	manager, err := resourcebudget.New(limits.Limits(pressure.MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	// Controlled pressure is isolated to this functional test; default Open
	// still uses the real OS sampler, including fail-closed unknown signals.
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
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
	if err := PrepareMemoryRuntimeMigrationDirectory(ctx, cfg, namespace, archive, ledgerKey, manager, guard); err != nil {
		t.Fatal("durable preparation namespace", err)
	}
	interrupted, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, ledgerKey, journalKey, cfg.Storage.DataDir, directory, guard, metadatastate.BaselineOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: 1, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: 8 << 20}, limits.MetadataJournalDiskBytes)
	if err != nil {
		t.Fatal("actual preparation before common seal", err)
	}
	if err := interrupted.Close(); err != nil {
		t.Fatal(err)
	}
	namespace.Restart = true
	if err := PrepareMemoryRuntimeMigrationDirectory(ctx, cfg, namespace, archive, ledgerKey, manager, guard); err != nil {
		t.Fatal("restart actual pre-seal preparation", err)
	}
	p, err := metadatastate.PrepareFormat2FromBackup(ctx, archive, ledgerKey, journalKey, cfg.Storage.DataDir, directory, guard, metadatastate.BaselineOptions{Identity: identity, InstanceID: instanceID, KeyGeneration: 1, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: 8 << 20}, limits.MetadataJournalDiskBytes)
	if err != nil {
		t.Fatal("real format1 backup migration", err)
	}
	defer p.Close()
	activation, err := ActivatePreparedMemoryRuntime(ctx, cfg, p, guard, manager, ledgerKey, journalKey)
	if activation != nil {
		defer activation.Close()
	}
	if err != nil {
		t.Fatal("complete offline activation", err)
	}
	if err := activation.Close(); err != nil {
		t.Fatal(err)
	}
	// Exercise the two interrupted activation boundaries with real encrypted
	// provenance and saved roots. Ordinary startup must refuse a pending marker.
	marker := memoryStateBootstrapPath(cfg)
	if err := os.Rename(marker, marker+".pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryRuntimeActivated(cfg); !errors.Is(err, ErrMemoryStateActivationPending) {
		t.Fatal(err)
	}
	for phase := 0; phase < 2; phase++ {
		if phase == 1 {
			if err := os.Link(marker, marker+".pending"); err != nil {
				t.Fatal(err)
			}
		}
		resumeBudget, err := resourcebudget.New(limits.Limits(cfg.ResourcePressure.MaxAge.Value()))
		if err != nil {
			t.Fatal(err)
		}
		if err := resumeBudget.SetPressure(resourcebudget.Healthy); err != nil {
			t.Fatal(err)
		}
		receipt, err := ResumeMemoryRuntimeActivation(ctx, cfg, guard, resumeBudget)
		if err != nil || receipt.Generation != 1 || receipt.Revision != 1 || receipt.Digest == ([32]byte{}) {
			t.Fatal("actual interrupted activation resume", phase, receipt, err)
		}
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	var sampledPressure atomic.Bool
	auditRelease := make(chan struct{})
	auditEntered := make(chan struct{})
	var releaseAudit sync.Once
	var enterAudit sync.Once
	defer releaseAudit.Do(func() { close(auditRelease) })
	opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample {
		sample := runtimeLowSample()
		if sampledPressure.Load() {
			sample.RuntimeUsed = 150
		}
		return sample
	}), selectedAuditProgress: func(ctx context.Context, _ startupaudit.Progress) error {
		enterAudit.Do(func() { close(auditEntered) })
		select {
		case <-auditRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	type openedRuntime struct {
		runtime *Runtime
		err     error
	}
	opened := make(chan openedRuntime, 1)
	go func() {
		runtime, err := OpenWithOptions(ctx, cfg, logger, opts)
		opened <- openedRuntime{runtime, err}
	}()
	// Closing the old Runtime exported actual business to a nonempty Usage
	// archive. Restoring its window must wait for the complete native audit;
	// releasing this gate after Open returns would deadlock the fixture.
	select {
	case <-auditEntered:
	case result := <-opened:
		if result.runtime != nil {
			result.runtime.Close()
		}
		t.Fatal("archive startup returned before the historical audit gate", result.err)
	case <-ctx.Done():
		t.Fatal("archive startup did not reach the historical audit", ctx.Err())
	}
	select {
	case result := <-opened:
		if result.runtime != nil {
			result.runtime.Close()
		}
		t.Fatal("archive startup accepted an incomplete native audit", result.err)
	default:
	}
	releaseAudit.Do(func() { close(auditRelease) })
	result := <-opened
	selected, err := result.runtime, result.err
	if err != nil {
		t.Fatal("default application startup from activated state", err)
	}
	defer selected.Close()
	if selected.selectedState == nil || selected.accountingState.projection != nil || selected.usage.Aggregate != nil || selected.governance.owner != nil || selected.store != selected.selectedState.services.Metadata || selected.accounting != selected.selectedState.services.Accounting {
		t.Fatal("application kept a fresh/legacy owner instead of the selected common generation")
	}
	if _, err := selected.selectedState.generation.Owners.Ledger.SealReference(ctx); err != nil {
		t.Fatal("archive startup did not complete the required historical audit", err)
	}
	assertSelectedRuntimeHTTPCompletion(t, selected, credentials, &sampledPressure)
	if err := selected.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal("default Runtime historical readers did not retire", err)
	}
	opts.selectedAuditProgress = nil
	liveRevisions := assertSelectedRuntimeLiveCheckpoint(t, ctx, selected)
	if _, err := selected.store.PutProject(ctx, domain.Project{ID: "prj_after_migration", Name: "Selected Runtime", Enabled: true}, 0, nil); err != nil {
		t.Fatal("real Runtime Metadata DAO", err)
	}
	outcome := assertSelectedRuntimeGovernance(t, ctx, selected, credentials.ProjectID, credentials.KeyID)
	assertSelectedRuntimeConcurrentBusiness(t, ctx, selected, credentials.ProjectID, credentials.KeyID)
	q, err := selected.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_after_migration", "chat")
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
		t.Fatal("actual selected business drain", err)
	}
	if _, err := selected.usageExporter.ExportDiskStep(ctx, selected.usage.disk, usage.DefaultDiskExportStepLimits()); err != nil {
		t.Fatal("real selected Runtime archive publication", err)
	}
	if err := selected.Close(); err != nil {
		t.Fatal("first selected Runtime close", err)
	}
	again, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("actual second default Runtime startup", err)
	}
	defer again.Close()
	if again.selectedState.generation.Seal.Revision != 2+liveRevisions {
		t.Fatal("drained shutdown did not publish successor common seal", again.selectedState.generation.Seal.Revision)
	}
	if project, err := again.store.GetProject(ctx, "prj_after_migration"); err != nil || project.Name != "Selected Runtime" {
		t.Fatal("second Runtime lost authoritative metadata", project, err)
	}
	if saved, found, err := again.selectedState.generation.Owners.Governance.ReadOutcome(ctx, outcome.ID); err != nil || !found || saved.Value != outcome.Value {
		t.Fatal("default reopen lost Outcome", saved, found, err)
	}
	for id, expected := range map[string]int64{request.RequestID: 7, q.RequestID: 9} {
		if summary, found, err := again.usage.disk.ReadRequest(ctx, id); err != nil || !found || summary.CostMicrosUSD != expected {
			t.Fatal("second Runtime lost accounting/Usage facts", summary, found, err)
		}
	}
	if err := again.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("third Runtime startup", err)
	}
	defer third.Close()
	if third.selectedState.generation.Seal.Revision != 3+liveRevisions {
		t.Fatal("inactive cache replacement did not advance common seal")
	}
	if _, err := third.store.GetProject(ctx, "prj_after_migration"); err != nil {
		t.Fatal(err)
	}
	if err := third.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	newCommandContext := func() context.Context {
		pressure, err := newResourcePressureWithSource(cfg, opts.resourceSampleSource)
		if err != nil {
			t.Fatal(err)
		}
		return context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
	}
	commandPressure, err := newResourcePressureWithSource(cfg, opts.resourceSampleSource)
	if err != nil {
		t.Fatal(err)
	}
	commandCtx := context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: commandPressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
	key, err := CreateProjectKey(commandCtx, cfg, credentials.ProjectID, "Actual selected offline command")
	if err != nil || key.KeyID == "" {
		t.Fatal("offline key command after selected Runtime shutdown", key, err)
	}

	if err := DisableProjectKey(newCommandContext(), cfg, key.KeyID); err != nil {
		t.Fatal("disable actual selected key", err)
	}
	if result, err := BootstrapAdminWithOptions(newCommandContext(), cfg, "admin", []byte("Selected-Admin-Password!23"), BootstrapAdminOptions{OperationID: originalAdmin.OperationID, IfNeeded: true}); err != nil || result != BootstrapAdminAlreadyCompleted {
		t.Fatal("bootstrap actual selected admin", err)
	}
	if err := ResetAdminPassword(newCommandContext(), cfg, "admin", []byte("Selected-New-Password!24")); err != nil {
		t.Fatal("reset actual selected admin", err)
	}
	if err := ResetAdminMFA(newCommandContext(), cfg, "admin"); err != nil {
		t.Fatal("reset actual selected MFA", err)
	}
	fourth, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("default Runtime after offline command", err)
	}
	defer fourth.Close()
	if stored, err := fourth.store.GetGatewayKey(ctx, key.KeyID); err != nil || stored.Enabled {
		t.Fatal("offline key did not reach selected Runtime", err)
	}
	if admin, err := fourth.store.GetAdminUser(ctx, "admin"); err != nil || !adminauth.VerifyPassword(admin, []byte("Selected-New-Password!24")) {
		t.Fatal("offline password not retained", err)
	}
	if fourth.selectedState.generation.Seal.Revision != 9+liveRevisions {
		t.Fatal("offline command did not publish actual common successor", fourth.selectedState.generation.Seal.Revision)
	}
	unsealedProject := domain.Project{ID: "prj_unsealed_suffix", Name: "Actual unsealed Metadata suffix", Enabled: true}
	if _, err := fourth.store.PutProject(ctx, unsealedProject, 0, nil); err != nil {
		t.Fatal(err)
	}
	unsealedRequest, err := fourth.accounting.BeginRequest(ctx, credentials.ProjectID, "req_unsealed_actual_close")
	if err != nil {
		t.Fatal(err)
	}
	if err := fourth.Close(); err != nil {
		t.Fatal(err)
	}
	// Direct Close now joins the actual business owners and publishes their
	// drained cut. Reopen must select that exact successor before orphan recovery.
	drained := fourth.selectedState.generation.Seal
	if fourth.backgroundWait.drainState.Load() != businessDrainComplete || drained.Revision != 10+liveRevisions {
		t.Fatal("direct Close did not publish the actual complete drain", drained)
	}
	fifth, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("default Runtime after unsealed actual native/COW writes", err)
	}
	defer fifth.Close()
	if fifth.selectedState.generation.Seal != drained {
		t.Fatal("reopen did not select the actual drained common publication")
	}
	if got, err := fifth.store.GetProject(ctx, unsealedProject.ID); err != nil || got.Name != unsealedProject.Name {
		t.Fatal("saved Metadata root did not reduce actual unsealed suffix", got, err)
	}
	if recovered, found, err := fifth.selectedState.generation.Owners.Ledger.ReadRequest(ctx, unsealedRequest.RequestID); err != nil || !found || recovered.Finalized == nil {
		t.Fatal("saved Ledger root did not finalize actual orphan once", recovered, found, err)
	}
	if err := fifth.Close(); err != nil {
		t.Fatal(err)
	}
	sealBeforeDiagnostics, err := os.ReadFile(filepath.Join(cfg.Storage.DataDir, "active.seal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLedger(newCommandContext(), cfg); err != nil {
		t.Fatal("selected read-only Ledger verify", err)
	}
	if _, err := VerifyAudit(newCommandContext(), cfg); err != nil {
		t.Fatal("selected read-only Audit verify", err)
	}
	if _, err := VerifyAuditAnchors(newCommandContext(), cfg, nil); err != nil {
		t.Fatal("selected read-only Audit anchor verify", err)
	}
	if _, err := VerifyUsage(newCommandContext(), cfg); err != nil {
		t.Fatal("selected read-only Usage verify", err)
	}
	if report, err := Doctor(newCommandContext(), cfg); err != nil || !report.Healthy {
		t.Fatal("selected read-only Doctor", report, err)
	}
	if sealAfter, err := os.ReadFile(filepath.Join(cfg.Storage.DataDir, "active.seal")); err != nil || !bytes.Equal(sealBeforeDiagnostics, sealAfter) {
		t.Fatal("diagnostics republished common cut", err)
	}
	for _, name := range []string{"memory-audit-0.cache.pending", "memory-audit-1.cache.pending", "memory-governance-0.cache.pending", "memory-governance-1.cache.pending"} {
		if _, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("normal shutdown left cache candidate", name, err)
		}
	}
	assertSelectedRuntimeRejectsDamagedSavedState(t, ctx, cfg, logger, opts)
	selectedBackupPath := filepath.Join(t.TempDir(), "selected-state.hmbk")
	archiveSummary, err := CreateBackupSummary(newCommandContext(), cfg, configPath, selectedBackupPath, backupKey, CreateBackupOptions{})
	if err != nil {
		t.Fatal("actual selected state backup", err)
	}
	restoredCfg := cfg
	restoredCfg.Storage.DataDir = filepath.Join(t.TempDir(), "restored-selected")
	if _, err := RestoreBackup(ctx, restoredCfg, selectedBackupPath, backupKey, archiveSummary.BackupID); err != nil {
		t.Fatal("actual selected backup restore", err)
	}
	restored, err := OpenWithOptions(ctx, restoredCfg, logger, opts)
	if err != nil {
		t.Fatal("default Runtime from restored selected backup", err)
	}
	defer restored.Close()
	if saved, err := restored.store.GetGatewayKey(ctx, key.KeyID); err != nil || saved.Enabled {
		t.Fatal("selected backup lost offline key disable", saved, err)
	}
	if saved, err := restored.store.GetProject(ctx, "prj_after_migration"); err != nil || saved.Name != "Selected Runtime" {
		t.Fatal("selected backup lost actual Metadata", saved, err)
	}
	if saved, found, err := restored.governance.owner.projection.ReadOutcome(ctx, outcome.ID); err != nil || !found || saved.Value != outcome.Value {
		t.Fatal("selected backup lost actual Outcome", saved, found, err)
	}
	if summary, found, err := restored.usage.disk.ReadRequest(ctx, q.RequestID); err != nil || !found || summary.CostMicrosUSD != 9 {
		t.Fatal("selected backup lost accounting/Usage", summary, found, err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
}

func configWithoutBudget(cfg config.Config) config.Config {
	cfg.ResourceBudget, cfg.ResourcePressure = nil, nil
	return cfg
}

func assertSelectedRuntimeConcurrentBusiness(t *testing.T, ctx context.Context, r *Runtime, projectID, keyID string) {
	t.Helper()
	var group sync.WaitGroup
	failures := make(chan error, 5)
	run := func(work func() error) {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := work(); err != nil {
				failures <- err
			}
		}()
	}
	for worker := 0; worker < 2; worker++ {
		run(func() error {
			for n := 0; n < 2; n++ {
				id := fmt.Sprintf("req_selected_parallel_%d_%d", worker, n)
				q, err := r.accounting.BeginRequestDetailed(ctx, projectID, keyID, id, "chat")
				if err != nil {
					return fmt.Errorf("parallel Begin: %w", err)
				}
				a, err := r.accounting.ReserveAttempt(ctx, q, 20, 3)
				if err != nil {
					return fmt.Errorf("parallel Reserve: %w", err)
				}
				if err := r.accounting.Settle(ctx, a, budget.Settlement{CommittedMicrosUSD: 1, Outcome: "success"}); err != nil {
					return fmt.Errorf("parallel Settle: %w", err)
				}
				if err := r.accounting.Finalize(ctx, q, "success"); err != nil {
					return fmt.Errorf("parallel Finalize: %w", err)
				}
			}
			return nil
		})
	}
	run(func() error {
		for n := 0; n < 4; n++ {
			if _, err := r.store.PutProject(ctx, domain.Project{ID: fmt.Sprintf("prj_parallel_%d", n), Name: "Parallel", Enabled: true}, 0, nil); err != nil {
				return fmt.Errorf("parallel Metadata write: %w", err)
			}
		}
		return nil
	})
	for worker := 0; worker < 2; worker++ {
		run(func() error {
			for n := 0; n < 20; n++ {
				if _, err := r.store.GetProject(ctx, "prj_after_migration"); err != nil {
					return fmt.Errorf("parallel Metadata read: %w", err)
				}
				if _, _, err := r.accountingState.reader.ReadPosition(ctx); err != nil {
					return fmt.Errorf("parallel Ledger read: %w", err)
				}
				if _, err := r.selectedState.generation.Owners.Governance.ReadHead(ctx); err != nil {
					return fmt.Errorf("parallel Governance read: %w", err)
				}
				if _, err := r.usage.disk.ReadStats(ctx); err != nil && !errors.Is(err, usage.ErrDiskUsageBusy) {
					return fmt.Errorf("parallel Usage read: %w", err)
				}
			}
			return nil
		})
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

func assertSelectedRuntimeGovernance(t *testing.T, ctx context.Context, r *Runtime, projectID, keyID string) domain.Outcome {
	t.Helper()
	project, err := r.store.GetProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	def, err := r.store.PutOutcomeDefinition(ctx, domain.OutcomeDefinition{ID: "odef_defaultselected", ProjectID: projectID, Name: "accepted", Version: 1, DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}, project.Revision, 0, nil)
	if err != nil {
		t.Fatal("actual default definition", err)
	}
	unit, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, projectID, keyID, 4, []domain.OutcomeDefinitionRef{{ID: def.ID, Version: def.Version}}, budget.GovernanceIntent{Operation: "work_unit.create", IdempotencyKeyHash: "sha256:" + strings.Repeat("3", 64), RequestFingerprint: "sha256:" + strings.Repeat("4", 64)})
	if err != nil {
		t.Fatal("actual default work unit", err)
	}
	input := governance.ReportInput{ProjectID: projectID, WorkUnitID: unit.ID, DefinitionID: def.ID, Value: "accepted", ReporterKeyID: keyID, ObservedAt: now, Intent: governance.Intent{IdempotencyKeyHash: "sha256:" + strings.Repeat("5", 64), RequestFingerprint: "sha256:" + strings.Repeat("6", 64)}}
	outcome, replayed, err := r.governance.manager.Report(ctx, input)
	if err != nil || replayed {
		t.Fatal("actual default Outcome", outcome, replayed, err)
	}
	if duplicate, replayed, err := r.governance.manager.Report(ctx, input); err != nil || !replayed || duplicate.ID != outcome.ID {
		t.Fatal("actual default Outcome retry", duplicate, replayed, err)
	}
	return outcome
}

func assertSelectedRuntimeRejectsDamagedSavedState(t *testing.T, ctx context.Context, cfg config.Config, logger *slog.Logger, opts OpenOptions) {
	t.Helper()
	sealPath := filepath.Join(cfg.Storage.DataDir, "active.seal")
	seal, err := os.ReadFile(sealPath)
	if err != nil {
		t.Fatal(err)
	}
	// These are tiny fixture artifacts; production verification remains streaming.
	for _, path := range []string{memoryStateBootstrapPath(cfg), sealPath, filepath.Join(cfg.Storage.DataDir, "memory-audit-1.cache"), filepath.Join(memoryStateDirectory(cfg), "metadata-root", "pages")} {
		t.Run("refuse_damage_"+filepath.Base(filepath.Dir(path))+"_"+filepath.Base(path), func(t *testing.T) {
			original, err := os.ReadFile(path)
			if err != nil || len(original) == 0 {
				t.Fatal("read actual fixture artifact", path, err)
			}
			damaged := bytes.Clone(original)
			damaged[0] ^= 0x80
			if err := os.WriteFile(path, damaged, 0600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Error(err)
				}
			}()
			opened, err := OpenWithOptions(ctx, cfg, logger, opts)
			if opened != nil {
				_ = opened.Close()
				t.Fatal("damaged selected artifact opened business", path)
			}
			if err == nil {
				t.Fatal("damaged selected artifact silently fell back", path)
			}
			if path != sealPath {
				actual, err := os.ReadFile(sealPath)
				if err != nil || !bytes.Equal(actual, seal) {
					t.Fatal("failed startup republished common authority", err)
				}
			}
		})
	}
}
