package gateway

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
)

// A stream the idle limit ended may have run upstream in full: it is not sent
// to a fallback, even when the caller has seen nothing yet, and it settles as
// conservatively as any other ambiguous stream, fixed fee included.
func TestStreamEndedByTheIdleLimitStopsAndSettlesConservatively(t *testing.T) {
	stalled := provider.StreamIdleFailure(provider.ErrStreamIdle)
	if retryable(stalled) || !ambiguousProviderFailure(stalled) {
		t.Fatal("a stream the idle limit ended was treated as one that cannot have run upstream")
	}
	fixedFee := provider.Target{FixedRequestMicrosUSD: 250}
	settlement := streamSettlement(nil, stalled, false, 1_000, 2_000, 0, fixedFee, 5_000)
	if !settlement.TokenEstimated || !settlement.CostEstimated || settlement.CommittedMicrosUSD < fixedFee.FixedRequestMicrosUSD {
		t.Fatalf("a stream the idle limit ended was not settled conservatively: %#v", settlement)
	}
}

// Counted from the failure the attempt logs, and only for that failure.
func TestStreamIdleAbortsCountOnlyTheIdleLimit(t *testing.T) {
	service := &Service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, err := range []error{
		provider.StreamIdleFailure(provider.ErrStreamIdle),
		&provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "decode provider stream", Cause: errors.New("unexpected EOF")},
	} {
		attempt := &activeAttempt{service: service, run: &requestRun{requestID: "req_1"}}
		attempt.logProviderFailure(err)
	}
	if got := service.StreamIdleAborts(); got != 1 {
		t.Fatalf("idle aborts = %d, want 1", got)
	}
	if descriptor := describeProviderFailure(provider.StreamIdleFailure(provider.ErrStreamIdle), provider.Target{}); descriptor.Class != provider.ErrorTimeout || !descriptor.Ambiguous {
		t.Fatalf("idle failure described as %#v", descriptor)
	}
}
