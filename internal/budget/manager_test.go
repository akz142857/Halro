package budget

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
