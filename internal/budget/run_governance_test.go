package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestRunGovernanceLifecycleAttributesSettlement(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 2, testGovernanceIntent("wu-1"))
	if err != nil {
		t.Fatal(err)
	}
	// S1 records the Run cap for attribution but does not use it for monetary
	// admission yet: that dual-budget decision is the S2 milestone.
	run, _, err := manager.CreateRun(ctx, "prj_1", "key_1", workUnit.ID, 20, time.Hour, 2, testGovernanceIntent("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := manager.BeginRequestAttributed(ctx, "prj_1", "key_1", "req_1", "model", workUnit.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := manager.ReserveLeaseDetailed(ctx, request, 10_000, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 30,
		PriceSnapshot:               testPriceSnapshot(t, domain.BillingModeMetered),
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 30, ProviderInputTokens: 10, ProviderOutputTokens: 10, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	got, ok := manager.Run("prj_1", run.ID)
	if !ok || got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 30 {
		t.Fatalf("run after settlement = %#v, found=%t", got, ok)
	}
}

func TestRunGovernanceRejectsCrossProjectAndClosedRunAttachment(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 1, testGovernanceIntent("wu-2"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.CreateRun(ctx, "prj_1", "key_1", workUnit.ID, 100, time.Hour, 1, testGovernanceIntent("run-2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginRequestAttributed(ctx, "prj_2", "key_2", "req_cross", "model", workUnit.ID, run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-project attach error=%v", err)
	}
	if _, _, err := manager.CloseRun(ctx, "prj_1", "key_1", workUnit.ID, run.ID, "completed", testGovernanceIntent("close-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginRequestAttributed(ctx, "prj_1", "key_1", "req_closed", "model", workUnit.ID, run.ID); !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("closed Run attach error=%v", err)
	}
}

func TestRunGovernanceEnforcesActiveResourceLimits(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 1, testGovernanceIntent("wu-3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 1, testGovernanceIntent("wu-4")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("open Work Unit limit error=%v", err)
	}
	if _, _, err := manager.CreateRun(ctx, "prj_1", "key_1", workUnit.ID, 100, time.Hour, 1, testGovernanceIntent("run-3")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.CreateRun(ctx, "prj_1", "key_1", workUnit.ID, 100, time.Hour, 1, testGovernanceIntent("run-4")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("active Run limit error=%v", err)
	}
}

func testGovernanceIntent(operation string) GovernanceIntent {
	return GovernanceIntent{
		Operation:          operation,
		IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
}
