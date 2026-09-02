package budget

import (
	"context"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// poisonState pushes the read model's watermark past anything the manager will
// produce, so its next apply is rejected as out of order. That is the shape of
// a terminal apply failure: the state machine advances in sequence order and
// can never get past the record it choked on.
func poisonState(t *testing.T, state *ledger.State) {
	t.Helper()
	err := state.Apply(ledger.Record{
		Generation: 1,
		Sequence:   1_000_000,
		Event: ledger.Event{
			EventID:    "evt_poison",
			Kind:       ledger.EventRequestAccepted,
			RequestID:  "request_poison",
			ProjectID:  "project_poison",
			PeriodID:   "2026-08-06",
			OccurredAt: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("could not stage the poisoned watermark: %v", err)
	}
}

// Appending after a failed apply fsyncs a record nothing will ever apply, and
// leaves it in the log for replay to choke on at the next start — with a longer
// tail of unappliable records behind it each time, until the process cannot
// start at all. The refusal has to come before the write, not after it.
func TestAppendsStopAtTheFirstFailedApply(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()
	poisonState(t, state)

	// The first call is the one that discovers the failure. It writes, because
	// nothing knew yet; that record is the poison itself.
	if _, err := manager.BeginRequest(context.Background(), "project_a", "request_a"); err == nil {
		t.Fatal("a rejected apply was reported as success")
	}
	afterPoisoning := manager.log.Stats().Records

	// Everything after it must be refused without touching the log.
	for attempt := range 5 {
		if _, err := manager.BeginRequest(context.Background(), "project_a", "request_b"); err == nil {
			t.Fatalf("call %d succeeded against a manager that can no longer apply", attempt+1)
		}
	}
	if grew := manager.log.Stats().Records - afterPoisoning; grew != 0 {
		t.Fatalf("a manager that can no longer apply still wrote %d record(s)", grew)
	}
}

// Accounting is the gateway's fail-closed gate and it reads the ledger's
// status. Leaving that healthy while this manager can no longer apply anything
// would give two answers to one question.
func TestFailedApplyPutsAccountingIntoTheSharedUnavailableState(t *testing.T) {
	manager, state, closeLog := newTestManager(t)
	defer closeLog()

	if status := manager.log.Status().Load(); status != ledger.AccountingHealthy {
		t.Fatalf("status started at %v, want healthy", status)
	}
	poisonState(t, state)
	if _, err := manager.BeginRequest(context.Background(), "project_a", "request_a"); err == nil {
		t.Fatal("a rejected apply was reported as success")
	}
	if status := manager.log.Status().Load(); status != ledger.AccountingUnavailable {
		t.Fatalf("status = %v after a failed apply, want unavailable", status)
	}
}
