package budget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
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
	run, _, err := manager.CreateRun(ctx, "prj_1", "key_1", workUnit.ID, 30, time.Hour, 2, testGovernanceIntent("run-1"))
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

func TestRunGovernanceDualAdmissionNeverExceedsRunBudget(t *testing.T) {
	const (
		workers     = 64
		reservation = int64(10)
		capMicros   = int64(100)
	)
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_dual", "key_dual", 1, testGovernanceIntent("wu-dual"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.CreateRun(ctx, "prj_dual", "key_dual", workUnit.ID, capMicros, time.Hour, 1, testGovernanceIntent("run-dual"))
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var admitted, refused atomic.Int64
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			request, beginErr := manager.BeginRequestAttributed(ctx, "prj_dual", "key_dual", fmt.Sprintf("req_%d", worker), "model", workUnit.ID, run.ID)
			if beginErr != nil {
				t.Errorf("begin %d: %v", worker, beginErr)
				return
			}
			_, reserveErr := manager.ReserveAttemptDetailed(ctx, request, 10_000, reservation, AttemptMetadata{})
			switch {
			case reserveErr == nil:
				admitted.Add(1)
			case errors.Is(reserveErr, ErrRunExceeded):
				refused.Add(1)
			default:
				t.Errorf("reserve %d: %v", worker, reserveErr)
			}
		}(worker)
	}
	close(start)
	group.Wait()

	got, ok := manager.Run("prj_dual", run.ID)
	if !ok || got.ReservedMicrosUSD != capMicros || got.BudgetState != domain.RunBudgetDepleted || got.RemainingMicrosUSD != 0 {
		t.Fatalf("run=%#v found=%t", got, ok)
	}
	if admitted.Load() != 10 || refused.Load() != workers-10 {
		t.Fatalf("admitted=%d refused=%d", admitted.Load(), refused.Load())
	}
}

func TestRunGovernanceProjectBudgetKeepsPriorityAndZeroStillEnforcesRun(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_priority", "key_priority", 1, testGovernanceIntent("wu-priority"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.CreateRun(ctx, "prj_priority", "key_priority", workUnit.ID, 5, time.Hour, 1, testGovernanceIntent("run-priority"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := manager.BeginRequestAttributed(ctx, "prj_priority", "key_priority", "req_priority", "model", workUnit.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReserveAttemptDetailed(ctx, request, 5, 10, AttemptMetadata{}); !errors.Is(err, ErrExceeded) {
		t.Fatalf("both caps reject error=%v, want project budget priority", err)
	}
	if _, err := manager.ReserveAttemptDetailed(ctx, request, 0, 6, AttemptMetadata{}); !errors.Is(err, ErrRunExceeded) {
		t.Fatalf("zero project cap error=%v, want run cap", err)
	}
	got, _ := manager.Run("prj_priority", run.ID)
	if got.ReservedMicrosUSD != 0 || got.RemainingMicrosUSD != 5 {
		t.Fatalf("rejected admissions changed run=%#v", got)
	}
}

func TestRunGovernanceSettlementAndCrossPeriodKeepLifetimeCap(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_lifetime", "key_lifetime", 1, testGovernanceIntent("wu-lifetime"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.CreateRun(ctx, "prj_lifetime", "key_lifetime", workUnit.ID, 100, 72*time.Hour, 1, testGovernanceIntent("run-lifetime"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.BeginRequestAttributed(ctx, "prj_lifetime", "key_lifetime", "req_first", "model", workUnit.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := manager.ReserveAttemptDetailed(ctx, first, 0, 100, AttemptMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 60, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }
	second, err := manager.BeginRequestAttributed(ctx, "prj_lifetime", "key_lifetime", "req_second", "model", workUnit.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReserveAttemptDetailed(ctx, second, 0, 41, AttemptMetadata{}); !errors.Is(err, ErrRunExceeded) {
		t.Fatalf("cross-period reservation error=%v", err)
	}
	if _, err := manager.ReserveAttemptDetailed(ctx, second, 0, 40, AttemptMetadata{}); err != nil {
		t.Fatalf("settlement did not restore headroom: %v", err)
	}
}

func TestRunGovernanceExpiryAndCloseOrderReservation(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_order", "key_order", 1, testGovernanceIntent("wu-order"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.CreateRun(ctx, "prj_order", "key_order", workUnit.ID, 100, time.Hour, 1, testGovernanceIntent("run-order"))
	if err != nil {
		t.Fatal(err)
	}
	period, err := manager.Periods().PeriodAt(manager.now())
	if err != nil {
		t.Fatal(err)
	}
	key := ledger.BalanceKey{ProjectID: "prj_order", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion}
	if err := manager.admitForRun(key, 0, 100, true, workUnit.ID, run.ID, manager.now()); err != nil {
		t.Fatal(err)
	}
	if pending, _ := manager.Run("prj_order", run.ID); pending.BudgetState != domain.RunBudgetFullyReserved || pending.RemainingMicrosUSD != 0 {
		t.Fatalf("pending projection=%#v", pending)
	}
	closed := make(chan error, 1)
	go func() {
		_, _, closeErr := manager.CloseRun(ctx, "prj_order", "key_order", workUnit.ID, run.ID, "completed", testGovernanceIntent("close-order"))
		closed <- closeErr
	}()
	select {
	case err := <-closed:
		t.Fatalf("close passed an earlier admission: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	manager.releaseAdmittedForRun(key, 100, run.ID)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	request := Request{RequestID: "req_after_close", ProjectID: "prj_order", WorkUnitID: workUnit.ID, RunID: run.ID, Period: period}
	if _, err := manager.ReserveAttemptDetailed(ctx, request, 0, 1, AttemptMetadata{}); !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("post-close reservation error=%v", err)
	}

	workUnit2, _, err := manager.CreateWorkUnit(ctx, "prj_expiry", "key_expiry", 1, testGovernanceIntent("wu-expiry"))
	if err != nil {
		t.Fatal(err)
	}
	run2, _, err := manager.CreateRun(ctx, "prj_expiry", "key_expiry", workUnit2.ID, 100, time.Hour, 1, testGovernanceIntent("run-expiry"))
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return run2.ExpiresAt }
	request2 := Request{RequestID: "req_at_expiry", ProjectID: "prj_expiry", WorkUnitID: workUnit2.ID, RunID: run2.ID, Period: period}
	if _, err := manager.ReserveAttemptDetailed(ctx, request2, 0, 1, AttemptMetadata{}); !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("expiry boundary error=%v", err)
	}
}

func TestRunGovernanceRejectsUnknownPriceAndIntegerOverflow(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_bounds", "key_bounds", 1, testGovernanceIntent("wu-bounds"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.CreateRun(ctx, "prj_bounds", "key_bounds", workUnit.ID, math.MaxInt64, time.Hour, 1, testGovernanceIntent("run-bounds"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := manager.BeginRequestAttributed(ctx, "prj_bounds", "key_bounds", "req_bounds", "model", workUnit.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	unknownSnapshot := domain.NewUnknownPriceSnapshot(manager.now())
	if _, err := manager.ReserveLeaseDetailed(ctx, request, 0, LeaseSpec{
		Mode: ledger.LeaseModeUnknownAllowed, PriceSnapshot: &unknownSnapshot,
		UnknownPolicyEvidence:       &domain.UnknownPricePolicyEvidence{ProjectID: "prj_bounds"},
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{}); !errors.Is(err, ErrRunPriceUnavailable) {
		t.Fatalf("unknown price error=%v", err)
	}
	attempt, err := manager.ReserveAttemptDetailed(ctx, request, 0, 1, AttemptMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: math.MaxInt64, Outcome: "provider_overage"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReserveAttemptDetailed(ctx, request, 0, 1, AttemptMetadata{}); err == nil || errors.Is(err, ErrRunExceeded) {
		t.Fatalf("overflow must fail as an accounting error, got %v", err)
	}
}

func TestRunGovernanceRecoveryKeepsStartedAttemptChargedToRun(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_recovery", "key_recovery", 1, testGovernanceIntent("wu-recovery"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.CreateRun(ctx, "prj_recovery", "key_recovery", workUnit.ID, 30, time.Hour, 1, testGovernanceIntent("run-recovery"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := manager.BeginRequestAttributed(ctx, "prj_recovery", "key_recovery", "req_recovery", "model", workUnit.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := manager.ReserveLeaseDetailed(ctx, request, 0, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 30,
		PriceSnapshot:       testPriceSnapshot(t, domain.BillingModeMetered),
		PreparedInputTokens: 10, PreparedOutputTokens: 10,
		RecoveryKey:                 "accounting-recovery-v1",
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecoverPendingLeases(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := manager.Run("prj_recovery", run.ID)
	if got.ReservedMicrosUSD != 0 || got.CommittedMicrosUSD != 30 || got.BudgetState != domain.RunBudgetDepleted {
		t.Fatalf("recovered Run=%#v", got)
	}
	stats := manager.RecoveryStats()
	if stats.ConservativelySettled != 1 || stats.Failures != 0 {
		t.Fatalf("recovery stats=%#v", stats)
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
