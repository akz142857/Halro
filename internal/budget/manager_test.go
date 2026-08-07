package budget

import (
	"bytes"
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

// testChainKey is a fixed 32-byte Ledger HMAC key for tests that append.
// Every event the ledger package writes is promoted to epoch 4 (ADR 0016),
// which requires a key to compute the frame MAC.
var testChainKey = bytes.Repeat([]byte{0x24}, 32)

func newTestManager(t *testing.T) (*Manager, *ledger.State, func()) {
	t.Helper()
	status := ledger.NewStatus()
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "usage.wal"), status, ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	state := ledger.NewState()
	manager, err := New(log, state, mustResolver(t, "UTC"))
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
	if state.PendingReservations() != 1 || state.Balance("project_free", request.Period.ID, request.Period.TimezoneVersion).ReservedMicrosUSD != 0 {
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
	balance := state.Balance("project_recovery", request.Period.ID, request.Period.TimezoneVersion)
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
	balance := state.Balance(request.ProjectID, request.Period.ID, request.Period.TimezoneVersion)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 0 || balance.UnknownAttempts != 1 {
		t.Fatalf("unknown balance=%#v", balance)
	}
}

func TestReservationAndSettlementAreReflectedAtomically(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	attempt, err := manager.BeginAttempt(context.Background(), "project_1", 1_000, "request_1", 400)
	if err != nil {
		t.Fatal(err)
	}
	balance := state.Balance("project_1", "2026-07-31", 1)
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
	balance = state.Balance("project_1", "2026-07-31", 1)
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
	if got := state.Balance("project_1", "2026-07-31", 1).ReservedMicrosUSD; got != 400 {
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
	balance := state.Balance("project_1", "2026-07-31", 1)
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
	const zoneName = "America/New_York"
	status := ledger.NewStatus()
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "usage.wal"), status, ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	manager, err := New(log, ledger.NewState(), mustResolver(t, zoneName))
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
	if first.Period.ID != "2026-11-01" || second.Period.ID != first.Period.ID ||
		third.Period.ID != "2026-11-02" {
		t.Fatalf("periods=%q,%q,%q", first.Period.ID, second.Period.ID, third.Period.ID)
	}
	// The identity travels with the request, so a reader of the ledger can
	// state the exact interval without consulting the setting.
	if first.Period.Timezone != zoneName || first.Period.TimezoneVersion == 0 {
		t.Fatalf("period identity = %#v", first.Period)
	}
}

// The manager buckets a request into its accounting period with its own clock. Callers in
// other packages cannot reach the unexported field, so without this option a test that
// pins some other clock still writes into the real current day and its assertion starts
// failing the next day. Guard the injection point.
func TestNewWithOptionsBucketsTheRequestByTheSuppliedClock(t *testing.T) {
	open := func(options Options) *Manager {
		t.Helper()
		// A manager owns its log: two of them applying to one file with separate states
		// wait on each other's sequence forever.
		log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "usage.wal"), ledger.NewStatus(), ledger.Options{ChainKey: testChainKey})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = log.Close() })
		manager, err := NewWithOptions(log, ledger.NewState(), mustResolver(t, "UTC"), options)
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	fixed := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

	pinned, err := open(Options{Now: func() time.Time { return fixed }}).
		BeginRequest(context.Background(), "project_clock", "req_clock")
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Period.ID != "2021-03-04" {
		t.Fatalf("period bucketed by the wrong clock: %q", pinned.Period.ID)
	}

	// Omitting the option keeps the wall clock, so production behaviour is untouched.
	live, err := open(Options{}).BeginRequest(context.Background(), "project_live", "req_live")
	if err != nil {
		t.Fatal(err)
	}
	if live.Period.ID != time.Now().In(time.UTC).Format("2006-01-02") {
		t.Fatalf("default clock was replaced: %q", live.Period.ID)
	}
}

// mustResolver pins the accounting timezone for a test. Period boundaries are
// the subject here, so the zone has to be stated rather than inherited from
// whatever the host is set to.
func mustResolver(t *testing.T, timezone string) *PeriodResolver {
	t.Helper()
	resolver, err := NewFixedPeriodResolver(timezone)
	if err != nil {
		t.Fatalf("resolver for %s: %v", timezone, err)
	}
	return resolver
}
