package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func frozen() time.Time { return time.Date(2026, 9, 21, 12, 0, 30, 0, time.UTC) }

// The bound this endpoint has is the built-in one. Discovery takes no Project
// RPM slot — there is no provider call to pace — so without this a key could
// ask for the list as fast as it can open sockets, on an install whose
// per-source limiter its operator turned off.
func TestDiscoveryIsBoundedByTheBuiltInKeyCeiling(t *testing.T) {
	f := newFixtureAt(t, 0, ledger.Options{}, frozen)
	defer f.close()
	for attempt := range KeyCeilingRPM {
		if _, err := f.service.Models(context.Background(), f.plaintext); err != nil {
			t.Fatalf("request %d inside the ceiling was refused: %v", attempt, err)
		}
	}
	_, err := f.service.Models(context.Background(), f.plaintext)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 429 || refusal.Code != "rate_limit_exceeded" {
		t.Fatalf("err = %v, want 429 rate_limit_exceeded", err)
	}
	// The caller is told when to come back, and the hint is the remainder of
	// the window rather than zero.
	if refusal.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %s, want the 30s left in the window", refusal.RetryAfter)
	}
	if _, err := f.service.Model(context.Background(), f.plaintext, "chat"); !errors.As(err, &refusal) || refusal.HTTPStatus != 429 {
		t.Fatalf("retrieval err = %v, want the same 429: the two endpoints share one budget", err)
	}
	if rejections := f.service.RejectionMetrics().KeyRate; rejections != 2 {
		t.Fatalf("KeyRate rejections = %d, want 2", rejections)
	}
	// Nothing about the refusal touches the Project's own limits, which is the
	// point of not charging them: a key polling the model list cannot exhaust
	// the RPM an inference request needs.
	if metrics := f.service.RejectionMetrics(); metrics.RPM != 0 || metrics.TPM != 0 {
		t.Fatalf("discovery consumed the Project limiter: %+v", metrics)
	}
}

// The window is fixed rather than rolling, so the budget comes back whole at
// the top of the next minute.
func TestTheKeyCeilingIsRestoredWhenTheWindowRolls(t *testing.T) {
	now := frozen()
	f := newFixtureAt(t, 0, ledger.Options{}, func() time.Time { return now })
	defer f.close()
	for range KeyCeilingRPM {
		if _, err := f.service.Models(context.Background(), f.plaintext); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.Models(context.Background(), f.plaintext); err == nil {
		t.Fatal("the ceiling did not apply")
	}
	now = now.Add(30 * time.Second)
	if _, err := f.service.Models(context.Background(), f.plaintext); err != nil {
		t.Fatalf("the budget was not restored by the new window: %v", err)
	}
}

// A retrieval for an identifier that names nothing is the cheapest request the
// resource plane answers and the one worth repeating: it returns before any
// accounting is opened, so the Project limiter never sees it. The ceiling is
// charged at authentication, ahead of the lookup, for exactly that reason.
func TestDeferredRetrievalOfAnUnknownIdentifierIsCharged(t *testing.T) {
	f := newDeferredFixtureAt(t, frozen, nil)
	for attempt := range KeyCeilingRPM {
		_, _, err := f.service.DeferredResponse(context.Background(), f.plaintext, "resp_does_not_exist")
		var failure *Error
		if !errors.As(err, &failure) || failure.HTTPStatus != 404 {
			t.Fatalf("request %d: err = %v, want 404", attempt, err)
		}
	}
	_, _, err := f.service.DeferredResponse(context.Background(), f.plaintext, "resp_does_not_exist")
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.HTTPStatus != 429 {
		t.Fatalf("err = %v, want 429 once the ceiling is reached", err)
	}
}
