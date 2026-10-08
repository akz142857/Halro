package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/vault"
)

func TestRuntimeGovernanceProjectionDiskCeilingFollowsPhysicalOwner(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	if err := r.governance.manager.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.governance.owner.close(); err != nil {
		t.Fatal(err)
	}

	master, err := unlockMasterKey(context.Background(), r.config)
	if err != nil {
		t.Fatal(err)
	}
	ledgerKey, err := loadLedgerHMACKey(r.store, r.vault, master)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	governanceKey, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	clear(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(governanceKey)

	b := runtimeRAMBudgetFixture()
	b.TotalDiskBytes = 1 << 30
	b.BackgroundDiskBytes = 512 << 20
	b.RecoveryDiskBytes = 512 << 20
	b.RecoveryReserveDiskBytes = 512 << 20
	b.GovernanceProjectionDiskBytes = 256 << 20
	addProjectionMappingFixtureRAM(t, &b)
	b.GovernanceProjectionDiskBytes = 4 << 10
	b.RecoveryClaims = 6
	manager, err := resourcebudget.New(b.Limits(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pressure := &resourcePressureRuntime{budget: manager}
	cfg := r.config
	cfg.ResourceBudget = &b
	path := standaloneGovernanceProjectionPath(cfg)
	owner, err := prepareRuntimeGovernance(context.Background(), cfg, r.store, r.governance.log, governanceKey, nil, pressure)
	if owner != nil || !errors.Is(err, governance.ErrProjectionDiskLimit) {
		t.Fatal("undersized runtime Governance ceiling was accepted", owner, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("failed Governance candidate or disk charge leaked", err, manager.Snapshot())
	}

	b.GovernanceProjectionDiskBytes = 256 << 20
	cfg.ResourceBudget = &b
	owner, err = prepareRuntimeGovernance(context.Background(), cfg, r.store, r.governance.log, governanceKey, nil, pressure)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	ram, _ := accountinglimits.ProjectionMappingRAMBytes(b.GovernanceProjectionDiskBytes)
	if snapshot.Classes[resourcebudget.Background].RAM != ram || snapshot.Classes[resourcebudget.Background].Disk != b.GovernanceProjectionDiskBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 0 || snapshot.RecoveryClaims != 0 || snapshot.NormalClaims != 1 {
		t.Fatal("published Governance projection did not retain its Background Disk charge", snapshot)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned Governance projection remains after retirement", err)
	}
	if snapshot := manager.Snapshot(); snapshot.Used != (resourcebudget.Cost{}) || snapshot.Owners != 0 {
		t.Fatal("Governance owner removal did not refund its ceiling", snapshot)
	}
}

func TestRuntimeAuditJournalDiskCeilingRejectsWholeAppendAndRetainsCharge(t *testing.T) {
	r, _, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		pressure, b := runtimePressureFixture(), runtimeRAMBudgetFixture()
		b.TotalDiskBytes = 64 << 20
		b.BackgroundDiskBytes = 16 << 20
		b.RecoveryDiskBytes = 16 << 20
		b.RecoveryReserveDiskBytes = 16 << 20
		b.AuditLogDiskBytes = 4 << 10
		b.RecoveryClaims = 6
		cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	})
	if r.audit == nil || r.audit.Summary().Bytes >= 4<<10 {
		t.Fatal("fixture must open with Audit history below its configured ceiling", r.audit)
	}
	manager := r.resourcePressure.logicalBudget()
	if snapshot := manager.Snapshot(); snapshot.Classes[resourcebudget.Background].Disk != 4<<10 || snapshot.Classes[resourcebudget.Recovery].Disk != 0 {
		t.Fatal("Runtime did not publish the Audit journal ceiling as a Background owner", snapshot)
	}
	before := r.audit.Summary()
	event := audit.Event{EventID: "evt_audit_disk_ceiling", OccurredAt: time.Now().UTC(), ActorType: "system", Action: "disk.ceiling", Outcome: "success",
		Metadata: map[string]any{"payload": strings.Repeat("x", 6<<10)}}
	if _, err := r.audit.Append(context.Background(), event); !errors.Is(err, audit.ErrJournalDiskLimit) {
		t.Fatal("Runtime Audit append escaped its configured ceiling", err)
	}
	if after := r.audit.Summary(); after != before {
		t.Fatal("Audit capacity refusal changed the durable head", before, after)
	}
	path := r.config.AuditPath()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 4<<10 {
		t.Fatal("Runtime wrote beyond or removed its capped Audit journal", info, err)
	}
	if snapshot := manager.Snapshot(); snapshot.Classes[resourcebudget.Background].Disk != 4<<10 {
		t.Fatal("persistent Audit journal disk claim was refunded while its file remains", snapshot)
	}
}

func TestRuntimeGovernanceJournalDiskCeilingOwnsPersistentJournal(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		pressure, b := runtimePressureFixture(), runtimeRAMBudgetFixture()
		b.TotalDiskBytes = 64 << 20
		b.BackgroundDiskBytes = 16 << 20
		b.RecoveryDiskBytes = 16 << 20
		b.RecoveryReserveDiskBytes = 16 << 20
		b.GovernanceLogDiskBytes = 1
		b.RecoveryClaims = 6
		cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	})
	if r.governance.log == nil || r.governance.log.Summary().Bytes != 0 {
		t.Fatal("fixture must start with an empty authenticated Governance journal", r.governance.log)
	}
	manager := r.resourcePressure.logicalBudget()
	if snapshot := manager.Snapshot(); snapshot.Classes[resourcebudget.Background].Disk != 1 || snapshot.Classes[resourcebudget.Recovery].Disk != 0 {
		t.Fatal("Runtime did not publish the Governance journal ceiling as a Background owner", snapshot)
	}

	ctx := context.Background()
	project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	definition := domain.OutcomeDefinition{ID: "odef_journalcap", ProjectID: project.ID, Name: "journal_cap", Version: 1,
		DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
	if _, err := r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil); err != nil {
		t.Fatal(err)
	}
	unit, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, project.ID, bootstrap.KeyID, 10,
		[]domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}, budget.GovernanceIntent{Operation: "create_work_unit", IdempotencyKeyHash: outcomePageTestLabel("journal-cap-wu"), RequestFingerprint: outcomePageTestLabel("journal-cap-wu-fp")})
	if err != nil {
		t.Fatal(err)
	}
	input := governance.ReportInput{ProjectID: project.ID, WorkUnitID: unit.ID, DefinitionID: definition.ID, ReporterKeyID: bootstrap.KeyID,
		Value: "accepted", ObservedAt: now, Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("journal-cap"), RequestFingerprint: outcomePageTestLabel("journal-cap-fp")}}
	if _, _, err := r.governance.manager.Report(ctx, input); !errors.Is(err, governance.ErrJournalDiskLimit) {
		t.Fatal("Runtime Governance append escaped its configured journal ceiling", err)
	}
	if summary := r.governance.log.Summary(); summary.Bytes != 0 || summary.Records != 0 {
		t.Fatal("capacity refusal changed the authenticated Governance head", summary)
	}
	journalPath := r.config.GovernancePath()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(journalPath); err != nil || info.Size() != 0 {
		t.Fatal("Runtime.Close unexpectedly removed or grew the native Governance journal", info, err)
	}
	if snapshot := manager.Snapshot(); snapshot.Classes[resourcebudget.Background].Disk != 1 {
		t.Fatal("persistent journal disk claim was refunded while its file remains", snapshot)
	}
}
