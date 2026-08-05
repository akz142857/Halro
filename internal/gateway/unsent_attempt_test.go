package gateway

import (
	"errors"
	"net"
	"testing"

	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/semantic"
)

// refusedConnection builds the error an adapter now produces when the dial
// itself fails, so this test tracks the adapters rather than restating them.
func refusedConnection() error {
	cause := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	return &provider.Error{
		Class:     provider.ErrorConnect,
		Retryable: true,
		Ambiguous: !provider.Unsent(cause),
		Message:   "provider request failed",
		Cause:     cause,
	}
}

// A provider that cannot be reached at all is the case multi-deployment routing
// exists for, yet it was the one case that never failed over: the adapters
// marked the attempt ambiguous, and retryable() treats ambiguity as "may have
// executed upstream" and stops. Routing around a dead provider is exactly what
// should happen, because a failed dial cannot have run anything.
func TestUnreachableProviderFailsOverToTheNextTarget(t *testing.T) {
	if !retryable(refusedConnection()) {
		t.Fatal("a refused connection did not fail over to the next deployment")
	}
}

// The same flag decided billing. An ambiguous attempt settles against estimated
// tokens because the provider may have done the work — but a dial that never
// connected did no work, so charging a whole estimated completion for it bills
// the project for a request the provider never saw.
func TestUnreachableProviderIsNotBilled(t *testing.T) {
	settlement := settlementForResult(
		semantic.GenerateResult{}, refusedConnection(),
		1_000, 2_000, provider.Target{}, 5_000,
	)
	if settlement.ProviderInputTokens != 0 || settlement.ProviderOutputTokens != 0 {
		t.Fatalf("a request that never reached the provider was billed for tokens: %#v", settlement)
	}
	if settlement.TokenEstimated || settlement.CostEstimated {
		t.Fatalf("a request that never reached the provider produced an estimated charge: %#v", settlement)
	}
	if settlement.Outcome != "provider_error" {
		t.Fatalf("settlement outcome = %q, want provider_error", settlement.Outcome)
	}
}

// The narrowness is the safeguard: once the request is on the wire, a failure
// still has to be treated as possibly executed, or a retry bills the caller for
// two completions.
func TestAttemptThatReachedTheProviderStillStopsAndSettles(t *testing.T) {
	cause := &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}
	inFlight := &provider.Error{
		Class:     provider.ErrorMalformed,
		Retryable: true,
		Ambiguous: !provider.Unsent(cause),
		Cause:     cause,
	}
	if retryable(inFlight) {
		t.Fatal("an attempt that may have executed upstream was retried")
	}
	settlement := settlementForResult(
		semantic.GenerateResult{}, inFlight,
		1_000, 2_000, provider.Target{}, 5_000,
	)
	if !settlement.TokenEstimated {
		t.Fatalf("an attempt that may have executed upstream was not settled: %#v", settlement)
	}
}
