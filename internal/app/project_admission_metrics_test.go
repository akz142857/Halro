package app

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestProjectRegistryMetricsShowOnlyLiveDefaultAccounting(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); _ = r.Close() })
	ctx := context.Background()
	assertLive := func(want int) {
		t.Helper()
		w := httptest.NewRecorder()
		if err := r.writeMetrics(ctx, w); err != nil {
			t.Fatal(err)
		}
		body := w.Body.String()
		if !strings.Contains(body, "# TYPE halro_accounting_project_lock_live gauge\n") || !strings.Contains(body, fmt.Sprintf("halro_accounting_project_lock_live %d\n", want)) {
			t.Fatal("wrong registry metric", want, body)
		}
		assertMetricsExpositionContract(t, body)
	}
	for i := 0; i < 1000; i++ {
		if _, err := r.accounting.ActiveRun(ctx, fmt.Sprint("cold_metrics_", i), "missing"); !errors.Is(err, budget.ErrRunNotFound) {
			t.Fatal(err)
		}
	}
	assertLive(0)
	if err := r.accounting.AddObserver(func(record ledger.Record) {
		if record.Event.Kind == ledger.EventReservationCreated && record.Event.ProjectID == bootstrap.ProjectID {
			close(entered)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	request, err := r.accounting.BeginRequest(ctx, bootstrap.ProjectID, "req_registry_metrics")
	if err != nil {
		t.Fatal(err)
	}
	type reserved struct {
		attempt budget.Attempt
		err     error
	}
	done := make(chan reserved, 1)
	go func() {
		attempt, err := r.accounting.ReserveAttempt(ctx, request, 100, 7)
		done <- reserved{attempt, err}
	}()
	awaitDrainSignal(t, entered)
	assertLive(1) // actual native Apply/observer still owns deferred money cleanup
	unblock()
	var result reserved
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not join observer")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	assertLive(0)
	balance, err := r.accounting.ReadBalance(ctx, ledger.BalanceKey{ProjectID: bootstrap.ProjectID, PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion})
	if err != nil || balance.ReservedMicrosUSD != 7 {
		t.Fatal("cold registry dropped persistent reservation", balance, err)
	}
	if err := r.accounting.Settle(ctx, result.attempt, budget.Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
	assertLive(0)
}
