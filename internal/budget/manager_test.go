package budget

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
)

func newTestManager(t *testing.T) (*Manager, *ledger.State, func()) {
	t.Helper()
	status := ledger.NewStatus()
	log, err := ledger.Open(filepath.Join(t.TempDir(), "usage.wal"), status)
	if err != nil {
		t.Fatal(err)
	}
	state := ledger.NewState()
	manager, err := New(log, state, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time {
		return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	}
	return manager, state, func() { _ = log.Close() }
}

func testPriceSnapshot(t *testing.T, mode domain.BillingMode) *domain.PriceSnapshot {
	t.Helper()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	price := domain.DeploymentPriceVersion{
		ID: "price_test", DeploymentID: "dep_test", Version: 1, Revision: 1,
		BillingMode: mode, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		EffectiveFrom: now.Add(-time.Hour), CreatedBy: "test", CreatedAt: now.Add(-time.Hour),
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Reference: "test", AssertedWithoutArchive: true},
	}
	if mode == domain.BillingModeMetered {
		price.InputMicrosPerMillion, price.OutputMicrosPerMillion = 1_000_000, 2_000_000
	}
	snapshot, err := domain.NewVersionedPriceSnapshot(price, now)
	if err != nil {
		t.Fatal(err)
	}
	return &snapshot
}

func TestTypedFreeLeasePersistsLifecycleWithoutReservingMoney(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	request, err := manager.BeginRequest(context.Background(), "project_free", "request_free")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := manager.ReserveLeaseDetailed(context.Background(), request, 1, LeaseSpec{
		Mode: ledger.LeaseModeFree, PriceSnapshot: testPriceSnapshot(t, domain.BillingModeFree),
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{DeploymentID: "dep_test"})
	if err != nil {
		t.Fatal(err)
	}
	if state.PendingReservations() != 1 || state.Balance("project_free", request.PeriodID).ReservedMicrosUSD != 0 {
		t.Fatalf("free lease state is incorrect")
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(context.Background(), attempt, Settlement{ProviderInputTokens: 10, ProviderOutputTokens: 20, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseDeepCopiesSnapshotAndFixedOnlySettlementIsValidated(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)
	*snapshot.InputMicrosPerMillion, *snapshot.OutputMicrosPerMillion, *snapshot.FixedRequestMicrosUSD = 0, 0, 7
	request, err := manager.BeginRequest(context.Background(), "project_fixed", "request_fixed")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := manager.ReserveLeaseDetailed(context.Background(), request, 100, LeaseSpec{Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 7, PriceSnapshot: snapshot, TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, AttemptMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	*snapshot.FixedRequestMicrosUSD = 99
	if *attempt.PriceSnapshot.FixedRequestMicrosUSD != 7 {
		t.Fatal("lease snapshot aliased caller memory")
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(context.Background(), attempt, Settlement{CommittedMicrosUSD: 0, Outcome: "success"}); err == nil {
		t.Fatal("fixed-only zero-token settlement bypassed immutable formula")
	}
}

func TestConcurrentAdjustmentIntentCommitPersistsOnlyAuthoritativeSequence(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	request, _ := manager.BeginRequest(context.Background(), "project_serial", "request_serial")
	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)
	attempt, err := manager.ReserveLeaseDetailed(context.Background(), request, 1_000, LeaseSpec{Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 3, PriceSnapshot: snapshot, PreparedInputTokens: 1, PreparedOutputTokens: 1, TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, AttemptMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(context.Background(), attempt, Settlement{CommittedMicrosUSD: 3, ProviderInputTokens: 1, ProviderOutputTokens: 1, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	base := AdjustmentSpec{AttemptID: attempt.AttemptID, Mode: ledger.AdjustmentModeExplicit, ExplicitDeltaMicrosUSD: 1, ExpectedSequence: 0, ExpectedNetCostMicrosUSD: 3, EvidenceDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", ReasonCode: "invoice_difference", Reason: "concurrent", CreatedBy: "admin"}
	var mu sync.Mutex
	persisted := 0
	failures := 0
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			spec := base
			fill := fmt.Sprintf("%064x", index+1)
			spec.IdempotencyKeyDigest, spec.RequestDigest = "sha256:"+fill, "sha256:"+fill
			_, _, err := manager.CommitAdjustmentWithIntent(context.Background(), spec, 0, 100, func(ledger.Event) error { mu.Lock(); persisted++; mu.Unlock(); return nil })
			if err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}(index)
	}
	wait.Wait()
	if persisted != 1 || failures != 1 || state.Balance(request.ProjectID, request.PeriodID).CommittedMicrosUSD != 4 {
		t.Fatalf("persisted=%d failures=%d balance=%#v", persisted, failures, state.Balance(request.ProjectID, request.PeriodID))
	}
}

func TestAppendOnlyAdjustmentIsIdempotentAndUpdatesOriginalPeriodBalance(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	request, err := manager.BeginRequest(context.Background(), "project_adjust", "request_adjust")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)
	attempt, err := manager.ReserveLeaseDetailed(context.Background(), request, 1_000, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 100, PriceSnapshot: snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{DeploymentID: "dep_test", ProviderID: "provider_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(context.Background(), attempt, Settlement{CommittedMicrosUSD: 50, ProviderInputTokens: 10, ProviderOutputTokens: 20, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	spec := AdjustmentSpec{AttemptID: attempt.AttemptID, Mode: ledger.AdjustmentModeExplicit, ExplicitDeltaMicrosUSD: 25,
		ExpectedSequence: 0, ExpectedNetCostMicrosUSD: 50,
		IdempotencyKeyDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		RequestDigest:        "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		EvidenceDigest:       "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		ReasonCode:           "invoice_difference", Reason: "provider invoice correction", CreatedBy: "admin_test"}
	event, replayed, err := manager.AdjustCost(context.Background(), spec, 60)
	if err != nil || replayed {
		t.Fatalf("adjust event=%#v replayed=%t err=%v", event, replayed, err)
	}
	if event.AdjustmentSequence != 1 || *event.NetCostBeforeMicrosUSD != 50 || event.AdjustmentDeltaMicrosUSD != 25 || *event.NetCostAfterMicrosUSD != 75 {
		t.Fatalf("adjustment=%#v", event)
	}
	balance := state.Balance(request.ProjectID, request.PeriodID)
	if balance.OriginalCommittedMicrosUSD != 50 || balance.AdjustmentDeltaMicrosUSD != 25 || balance.CommittedMicrosUSD != 75 {
		t.Fatalf("balance=%#v", balance)
	}
	replayedEvent, replayed, err := manager.AdjustCost(context.Background(), spec, 60)
	if err != nil || !replayed || replayedEvent.EventID != event.EventID || state.Balance(request.ProjectID, request.PeriodID).CommittedMicrosUSD != 75 {
		t.Fatalf("idempotent event=%#v replayed=%t err=%v", replayedEvent, replayed, err)
	}
	conflict := spec
	conflict.RequestDigest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if _, _, err := manager.AdjustCost(context.Background(), conflict, 60); err == nil {
		t.Fatal("expected idempotency collision")
	}
}

func TestRecoverStartedLeaseUsesFrozenPriceAndPreparedBounds(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	request, err := manager.BeginRequest(context.Background(), "project_recovery", "request_recovery")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := manager.ReserveLeaseDetailed(context.Background(), request, 10_000, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 50,
		PriceSnapshot: testPriceSnapshot(t, domain.BillingModeMetered), PreparedInputTokens: 10, PreparedOutputTokens: 20,
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{DeploymentID: "dep_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecoverPendingLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	balance := state.Balance("project_recovery", request.PeriodID)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 50 || state.PendingReservations() != 0 {
		t.Fatalf("recovered balance=%#v pending=%d", balance, state.PendingReservations())
	}
	stats := manager.RecoveryStats()
	if stats.PendingObserved != 1 || stats.ConservativelySettled != 1 || stats.Failures != 0 {
		t.Fatalf("recovery stats=%#v", stats)
	}
}

func TestUnknownLeaseRequiresExplicitNoCostGovernanceEvidenceAndStaysUnknown(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	request, err := manager.BeginRequest(context.Background(), "project_unknown", "request_unknown")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := domain.NewUnknownPriceSnapshot(time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC))
	evidence := &domain.UnknownPricePolicyEvidence{
		PolicyVersion: "unknown-price-v1", ProjectID: request.ProjectID, TokenGuardStatus: "no_cost_dimension",
		ReasonCode: "cost_governance_disabled", InstanceExplicitOptIn: true, CostGovernanceDisabled: true,
	}
	spec := LeaseSpec{Mode: ledger.LeaseModeUnknownAllowed, PriceSnapshot: &snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20, UnknownPolicyEvidence: evidence,
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if _, err := manager.ReserveLeaseDetailed(context.Background(), request, 1, spec, AttemptMetadata{}); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("unknown lease with cost budget error=%v", err)
	}
	attempt, err := manager.ReserveLeaseDetailed(context.Background(), request, 0, spec, AttemptMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(context.Background(), attempt, Settlement{ProviderInputTokens: 7, ProviderOutputTokens: 9, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	balance := state.Balance(request.ProjectID, request.PeriodID)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 0 || balance.UnknownAttempts != 1 {
		t.Fatalf("unknown balance=%#v", balance)
	}
	correction := testPriceSnapshot(t, domain.BillingModeMetered)
	event, _, err := manager.AdjustCost(context.Background(), AdjustmentSpec{AttemptID: attempt.AttemptID, Mode: ledger.AdjustmentModeReprice,
		CorrectionPriceSnapshot: correction, ExpectedSequence: 0, ExpectedNetCostMicrosUSD: 0,
		IdempotencyKeyDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RequestDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		EvidenceDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", ReasonCode: "late_price_evidence", Reason: "establish known cost", CreatedBy: "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if event.BaseSettlementMicrosUSD != nil || event.NetCostBeforeMicrosUSD != nil || *event.NetCostAfterMicrosUSD != 25 || event.AdjustmentDeltaMicrosUSD != 25 {
		t.Fatalf("unknown correction=%#v", event)
	}
	balance = state.Balance(request.ProjectID, request.PeriodID)
	if balance.CommittedMicrosUSD != 25 || balance.AdjustmentDeltaMicrosUSD != 25 || balance.UnknownAttempts != 0 {
		t.Fatalf("corrected balance=%#v", balance)
	}
}

func TestReservationAndSettlementAreReflectedAtomically(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	attempt, err := manager.BeginAttempt(context.Background(), "project_1", 1_000, "request_1", 400)
	if err != nil {
		t.Fatal(err)
	}
	balance := state.Balance("project_1", "2026-07-31")
	if balance.ReservedMicrosUSD != 400 || balance.CommittedMicrosUSD != 0 {
		t.Fatalf("unexpected reserved balance: %#v", balance)
	}
	if err := manager.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(context.Background(), attempt, Settlement{
		CommittedMicrosUSD:   350,
		ProviderInputTokens:  20,
		ProviderOutputTokens: 10,
		Outcome:              "success",
	}); err != nil {
		t.Fatal(err)
	}
	balance = state.Balance("project_1", "2026-07-31")
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 350 ||
		balance.InputTokens != 20 || balance.OutputTokens != 10 {
		t.Fatalf("unexpected settled balance: %#v", balance)
	}
}

func TestBudgetCheckIncludesConcurrentReservations(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	var wait sync.WaitGroup
	wait.Add(2)
	results := make(chan error, 2)
	for index := 0; index < 2; index++ {
		go func(index int) {
			defer wait.Done()
			_, err := manager.BeginAttempt(
				context.Background(),
				"project_1",
				500,
				"request_"+string(rune('a'+index)),
				400,
			)
			results <- err
		}(index)
	}
	wait.Wait()
	close(results)
	var successes, exceeded int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrExceeded):
			exceeded++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || exceeded != 1 {
		t.Fatalf("successes=%d exceeded=%d", successes, exceeded)
	}
	if got := state.Balance("project_1", "2026-07-31").ReservedMicrosUSD; got != 400 {
		t.Fatalf("reserved=%d", got)
	}
}

func TestConcurrentProjectsPreserveLedgerOrder(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	const projects = 16
	var wait sync.WaitGroup
	errorsByProject := make(chan error, projects)
	for index := 0; index < projects; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			projectID := fmt.Sprintf("project_%d", index)
			request, err := manager.BeginRequest(context.Background(), projectID, fmt.Sprintf("request_%d", index))
			if err == nil {
				err = manager.Finalize(context.Background(), request, "success")
			}
			errorsByProject <- err
		}(index)
	}
	wait.Wait()
	close(errorsByProject)
	for err := range errorsByProject {
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.Watermark().Sequence != projects*2 {
		t.Fatalf("watermark=%d", state.Watermark().Sequence)
	}
}

func TestThousandConcurrentReservationsNeverOversell(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	const requests = 1000
	const budgetLimit = 500
	var wait sync.WaitGroup
	var admitted, rejected int64
	var counters sync.Mutex
	start := make(chan struct{})
	wait.Add(requests)
	for index := range requests {
		go func() {
			defer wait.Done()
			<-start
			_, err := manager.BeginAttempt(
				context.Background(), "project_1", budgetLimit,
				fmt.Sprintf("request_%d", index), 1,
			)
			counters.Lock()
			defer counters.Unlock()
			switch {
			case err == nil:
				admitted++
			case errors.Is(err, ErrExceeded):
				rejected++
			default:
				t.Errorf("reserve: %v", err)
			}
		}()
	}
	close(start)
	wait.Wait()
	balance := state.Balance("project_1", "2026-07-31")
	if admitted != budgetLimit || rejected != requests-budgetLimit ||
		balance.CommittedMicrosUSD+balance.ReservedMicrosUSD > budgetLimit {
		t.Fatalf(
			"admitted=%d rejected=%d committed=%d reserved=%d",
			admitted, rejected, balance.CommittedMicrosUSD, balance.ReservedMicrosUSD,
		)
	}
}

func TestEstimateCostRoundsUpAndRejectsOverflow(t *testing.T) {
	cost, err := EstimateCostMicros(1, 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 2 {
		t.Fatalf("cost=%d", cost)
	}
	if _, err := EstimateCostMicros(1<<62, 0, 4, 0); err == nil {
		t.Fatal("expected overflow")
	}
}

func TestZeroBudgetIsUnlimited(t *testing.T) {
	manager, _, closeLog := newTestManager(t)
	defer closeLog()
	if _, err := manager.BeginAttempt(context.Background(), "project_1", 0, "request_1", 10_000); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetPeriodUsesConfiguredTimezoneAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	status := ledger.NewStatus()
	log, err := ledger.Open(filepath.Join(t.TempDir(), "usage.wal"), status)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	manager, err := New(log, ledger.NewState(), location)
	if err != nil {
		t.Fatal(err)
	}
	// The two instants are the repeated 01:30 hour on either side of the 2026
	// fall-back transition. Both must belong to the same local budget day.
	instants := []time.Time{
		time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 2, 5, 1, 0, 0, time.UTC),
	}
	index := 0
	manager.now = func() time.Time { return instants[index] }
	first, err := manager.BeginRequest(context.Background(), "project_1", "request_1")
	if err != nil {
		t.Fatal(err)
	}
	index = 1
	second, err := manager.BeginRequest(context.Background(), "project_1", "request_2")
	if err != nil {
		t.Fatal(err)
	}
	index = 2
	third, err := manager.BeginRequest(context.Background(), "project_1", "request_3")
	if err != nil {
		t.Fatal(err)
	}
	if first.PeriodID != "2026-11-01" || second.PeriodID != first.PeriodID ||
		third.PeriodID != "2026-11-02" {
		t.Fatalf("periods=%q,%q,%q", first.PeriodID, second.PeriodID, third.PeriodID)
	}
}
