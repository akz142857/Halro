package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// At its limit the state refuses new requests, Work Units and Runs, while the
// work already admitted reserves again, settles and finishes; once that work
// has shrunk the state, new requests are accepted again.
func TestHotStateLimitRefusesNewWorkAndLetsAdmittedWorkFinish(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	workUnit, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 2, testGovernanceIntent("wu-1"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.CreateRun(ctx, "prj_1", "key_1", workUnit.ID, 1_000, time.Hour, 2, testGovernanceIntent("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := manager.BeginRequestAttributed(ctx, "prj_1", "key_1", "req_1", "model", workUnit.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reserve := func() Attempt {
		t.Helper()
		attempt, err := manager.ReserveLeaseDetailed(ctx, request, 10_000, LeaseSpec{
			Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 30,
			PriceSnapshot:               testPriceSnapshot(t, domain.BillingModeMetered),
			TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}, AttemptMetadata{})
		if err != nil {
			t.Fatalf("reserve for an admitted request: %v", err)
		}
		return attempt
	}
	first := reserve()
	manager.hotStateLimit = state.HotStateBytes()

	if _, err := manager.BeginRequestDetailed(ctx, "prj_1", "key_1", "req_2", "model"); !errors.Is(err, ErrHotStateFull) {
		t.Fatalf("new request err=%v", err)
	}
	if _, _, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 2, testGovernanceIntent("wu-2")); !errors.Is(err, ErrHotStateFull) {
		t.Fatalf("new work unit err=%v", err)
	}
	if _, _, err := manager.CreateRun(ctx, "prj_1", "key_1", workUnit.ID, 1_000, time.Hour, 2, testGovernanceIntent("run-2")); !errors.Is(err, ErrHotStateFull) {
		t.Fatalf("new run err=%v", err)
	}
	// A retried creation grows nothing and is answered as before.
	if replayed, replay, err := manager.CreateWorkUnit(ctx, "prj_1", "key_1", 2, testGovernanceIntent("wu-1")); err != nil || !replay || replayed.ID != workUnit.ID {
		t.Fatalf("idempotent replay %#v replay=%t err=%v", replayed, replay, err)
	}
	second := reserve()
	for _, attempt := range []Attempt{first, second} {
		if err := manager.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 30, ProviderInputTokens: 10, ProviderOutputTokens: 10, Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	stats := manager.HotStateStats()
	if stats.Bytes >= stats.Limit || stats.RequestRejections != 1 || stats.WorkUnitRejections != 1 || stats.RunRejections != 1 {
		t.Fatalf("stats %#v", stats)
	}
	if _, err := manager.BeginRequestDetailed(ctx, "prj_1", "key_1", "req_3", "model"); err != nil {
		t.Fatalf("a request after the state shrank: %v", err)
	}
}
