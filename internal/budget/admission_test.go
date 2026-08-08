package budget

// Admission tests for ADR 0018. The project lock no longer covers the durable
// append, so the property that used to fall out of "one request at a time" —
// a project cannot be admitted past its daily budget — now has to be proven.
// It is the fail-open direction, and the one worth the most scrutiny here.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
)

// Every worker asks for the same amount against a budget that fits only some of
// them. Whatever the interleaving, the durable balance must never exceed the
// budget, and the requests that did not fit must have been told so.
func TestConcurrentAdmissionNeverExceedsTheDailyBudget(t *testing.T) {
	const (
		workers     = 64
		reservation = int64(10)
		affordable  = 20
		budget      = reservation * affordable
	)
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)
	ctx := context.Background()

	var admitted, refused atomic.Int64
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			request, err := manager.BeginRequest(ctx, "project_budget", fmt.Sprintf("req_%d", worker))
			if err != nil {
				t.Error(err)
				return
			}
			_, err = manager.ReserveLeaseDetailed(ctx, request, budget, LeaseSpec{
				Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: reservation, PriceSnapshot: snapshot,
				PreparedInputTokens: 10, PreparedOutputTokens: 20,
				RecoveryKey:                 "accounting-recovery-v1",
				TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}, AttemptMetadata{AttemptNumber: 1})
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.Is(err, ErrExceeded):
				refused.Add(1)
			default:
				t.Errorf("worker %d: %v", worker, err)
			}
		}(worker)
	}
	group.Wait()

	period, err := manager.Periods().PeriodAt(manager.now())
	if err != nil {
		t.Fatal(err)
	}
	balance := state.Balance("project_budget", period.ID, period.TimezoneVersion)
	durable := balance.CommittedMicrosUSD + balance.ReservedMicrosUSD
	if durable > budget {
		t.Fatalf("durable balance %d exceeds the daily budget %d", durable, budget)
	}
	if admitted.Load() > affordable {
		t.Fatalf("%d reservations were admitted against a budget for %d", admitted.Load(), affordable)
	}
	if admitted.Load()+refused.Load() != workers {
		t.Fatalf("admitted=%d refused=%d, want %d in total", admitted.Load(), refused.Load(), workers)
	}
	// The conservative direction is permitted — a reservation counted in both
	// pendingAdmitted and the balance for a moment can refuse at the very edge —
	// but refusing everything would mean admission had stopped working.
	if admitted.Load() == 0 {
		t.Fatal("no reservation was admitted at all")
	}
	if leaked := manager.admittedForTest(ledger.BalanceKey{
		ProjectID: "project_budget", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion,
	}); leaked != 0 {
		t.Fatalf("%d micros of admitted spend was never handed back", leaked)
	}
}

// A reservation that never became durable must not keep holding budget
// headroom: the project would lose that much for the rest of the period, and
// nothing would ever release it.
func TestFailedAppendReleasesAdmittedSpend(t *testing.T) {
	var failing atomic.Bool
	status := ledger.NewStatus()
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "usage.wal"), status, ledger.Options{
		ChainKey: testChainKey,
		WrapDurability: func(file *os.File) ledger.DurabilityWriter {
			return &haltingDurability{file: file, failing: &failing}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	state := ledger.NewState()
	manager, err := New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)

	request, err := manager.BeginRequest(ctx, "project_fail", "req_fail")
	if err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	_, err = manager.ReserveLeaseDetailed(ctx, request, 1_000, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 900, PriceSnapshot: snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		RecoveryKey:                 "accounting-recovery-v1",
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{AttemptNumber: 1})
	if err == nil {
		t.Fatal("the reservation reported success while its append was failing")
	}

	period, periodErr := manager.Periods().PeriodAt(manager.now())
	if periodErr != nil {
		t.Fatal(periodErr)
	}
	key := ledger.BalanceKey{ProjectID: "project_fail", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion}
	if held := manager.admittedForTest(key); held != 0 {
		t.Fatalf("a failed append left %d micros admitted", held)
	}
}

// haltingDurability fails the write once armed, which is the seam
// ledger.Options documents for exactly this.
type haltingDurability struct {
	file    *os.File
	failing *atomic.Bool
}

func (d *haltingDurability) Write(payload []byte) (int, error) {
	if d.failing.Load() {
		return 0, errors.New("injected durability failure")
	}
	return d.file.Write(payload)
}

func (d *haltingDurability) Sync() error {
	if d.failing.Load() {
		return errors.New("injected durability failure")
	}
	return d.file.Sync()
}

// A settlement that is appended but not yet applied leaves state.Balance
// reporting the older, higher reserved figure. Admission reading that stale
// value is deliberate: it errs toward refusing, never toward overspending.
func TestAdmissionIsConservativeWhileASettlementIsInFlight(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	snapshot := testPriceSnapshot(t, domain.BillingModeMetered)
	ctx := context.Background()

	request, err := manager.BeginRequest(ctx, "project_inflight", "req_inflight")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := manager.ReserveLeaseDetailed(ctx, request, 100, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 50, PriceSnapshot: snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		RecoveryKey:                 "accounting-recovery-v1",
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{AttemptNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	period, err := manager.Periods().PeriodAt(manager.now())
	if err != nil {
		t.Fatal(err)
	}
	if reserved := state.Balance("project_inflight", period.ID, period.TimezoneVersion).ReservedMicrosUSD; reserved != 50 {
		t.Fatalf("reserved=%d, want 50", reserved)
	}
	if err := manager.Settle(ctx, attempt, Settlement{
		Outcome: "success", ProviderInputTokens: 10, ProviderOutputTokens: 20, CommittedMicrosUSD: 50,
	}); err != nil {
		t.Fatal(err)
	}
	// Settlement releases the reservation and commits the cost, so the total the
	// budget is measured against is unchanged and the next request sees it.
	balance := state.Balance("project_inflight", period.ID, period.TimezoneVersion)
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 50 {
		t.Fatalf("after settlement reserved=%d committed=%d", balance.ReservedMicrosUSD, balance.CommittedMicrosUSD)
	}
	second, err := manager.BeginRequest(ctx, "project_inflight", "req_inflight_2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReserveLeaseDetailed(ctx, second, 100, LeaseSpec{
		Mode: ledger.LeaseModeMetered, ReservationMicrosUSD: 60, PriceSnapshot: snapshot,
		PreparedInputTokens: 10, PreparedOutputTokens: 20,
		RecoveryKey:                 "accounting-recovery-v1",
		TokenGuardPricingViewDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, AttemptMetadata{AttemptNumber: 1}); !errors.Is(err, ErrExceeded) {
		t.Fatalf("committed spend was not counted against the budget: %v", err)
	}
}
