package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/vault"
)

func TestDoctorDiskReferenceCompleteHistoryReadOnlyAndSharedBudget(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	log, metadata, key := openSealingLedger(t, cfg)
	defer metadata.Close()
	defer clear(key)
	now := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 300; i++ {
		e := ledger.Event{Kind: ledger.EventRequestAccepted, EventID: fmt.Sprintf("doctor-history-%d", i), RequestID: fmt.Sprintf("doctor-request-%d", i), ProjectID: "doctor-project", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: now}
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		if i == 99 || i == 199 {
			if _, err := log.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 3; i++ {
		e := ledger.Event{Kind: ledger.EventRequestAccepted, EventID: fmt.Sprintf("doctor-pending-accept-%d", i), RequestID: fmt.Sprintf("doctor-pending-request-%d", i), ProjectID: "doctor-project", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: now}
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		amount := int64(10)
		e.Kind, e.EventID, e.AttemptID, e.ReservationMicrosUSD = ledger.EventReservationCreated, fmt.Sprintf("doctor-reserve-%d", i), fmt.Sprintf("doctor-attempt-%d", i), &amount
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			e.Kind, e.EventID = ledger.EventAttemptSettled, fmt.Sprintf("doctor-settle-%d", i)
			cost := int64(0)
			e.CommittedMicrosUSD = &cost
			e.Outcome = []string{"recovered_not_started", "recovered_started_unknown_result"}[i]
			e.FailurePhase = "accounting"
			if _, err := log.Append(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	head, _, _ := log.ChainHead()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	master, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	secretVault, err := vault.New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer secretVault.Close()
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	before := doctorTree(t, cfg.Storage.DataDir)
	for attempt := 0; attempt < 2; attempt++ {
		observation, err := inspectDoctorLedger(ctx, cfg, metadata, secretVault, master)
		if err != nil || !observation.LeasesVerified || observation.Head != head || observation.Pending.Watermark != head || observation.Pending.Count != 1 || observation.Pending.OldestAge < time.Hour || observation.RecoveredReleased != 1 || observation.RecoveredSettled != 1 || observation.ChainStatus != "pass" {
			t.Fatal("complete history diagnostic", observation, err)
		}
		if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) {
			t.Fatal("closed reference retained whole-job fees", s)
		}
		if files, err := os.ReadDir(parent); err != nil || len(files) != 0 {
			t.Fatal("closed reference scratch survived", files, err)
		}
		if after := doctorTree(t, cfg.Storage.DataDir); !reflect.DeepEqual(before, after) {
			t.Fatal("doctor changed authoritative files")
		}
	}
}

func TestDoctorDiskReferenceRefusesBeforeSourceAndCapsDerivative(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	occupied, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: cfg.ResourceBudget.RecoveryRAMBytes - resourcebudget.ClaimRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	// Remove the source so opening it would report NotExist instead of the
	// original Manager's refusal. Admission must happen first.
	missing := filepath.Join(t.TempDir(), "missing-data")
	refusedCfg := cfg
	refusedCfg.Storage.DataDir = missing
	observation, err := inspectDoctorLedger(ctx, refusedCfg, nil, nil, nil)
	if !errors.Is(err, resourcebudget.ErrExhausted) || errors.Is(err, os.ErrNotExist) || observation != (doctorLedgerObservation{}) {
		t.Fatal("source reached before shared admission", observation, err)
	}
	if err := occupied.Release(); err != nil {
		t.Fatal(err)
	}
	log, metadata, key := openSealingLedger(t, cfg)
	defer clear(key)
	defer metadata.Close()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	master, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	secretVault, err := vault.New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer secretVault.Close()
	cfg.ResourceBudget.LedgerProjectionDiskBytes = 4096
	ctx, pressure, err = commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := doctorTree(t, cfg.Storage.DataDir)
	observation, err = inspectDoctorLedger(ctx, cfg, metadata, secretVault, master)
	if err == nil || observation != (doctorLedgerObservation{}) {
		t.Fatal("tiny derivative published diagnostic success", observation, err)
	}
	if after := doctorTree(t, cfg.Storage.DataDir); !reflect.DeepEqual(before, after) {
		t.Fatal("ceiling failure changed native data")
	}
	if files, err := os.ReadDir(parent); err != nil || len(files) != 0 {
		t.Fatal("ceiling failure retained scratch", files, err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("ceiling refusal leaked fees", s)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := inspectDoctorLedger(canceled, cfg, metadata, secretVault, master); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
