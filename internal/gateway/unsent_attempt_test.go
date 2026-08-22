package gateway

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/safetransport"
	"github.com/akz142857/Halro/internal/semantic"
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

// The streaming path had the same defect one layer down: a definitively-unsent
// attempt settled with zero tokens, but the cost formula still added the
// deployment's fixed per-request fee, so fixed-fee pricing charged the project
// for every connect failure.
func TestUnreachableProviderStreamIsNotChargedTheFixedFee(t *testing.T) {
	fixedFee := provider.Target{FixedRequestMicrosUSD: 250}
	settlement := streamSettlement(
		nil, refusedConnection(), false,
		1_000, 2_000, 0, fixedFee, 5_000,
	)
	if settlement.CommittedMicrosUSD != 0 {
		t.Fatalf("a stream that never reached the provider committed cost: %#v", settlement)
	}
	if settlement.ProviderInputTokens != 0 || settlement.ProviderOutputTokens != 0 {
		t.Fatalf("a stream that never reached the provider was billed for tokens: %#v", settlement)
	}
	if settlement.TokenEstimated || settlement.CostEstimated {
		t.Fatalf("a stream that never reached the provider produced an estimated charge: %#v", settlement)
	}
	if settlement.Outcome != "provider_error" {
		t.Fatalf("settlement outcome = %q, want provider_error", settlement.Outcome)
	}
}

// An ambiguous streaming failure must keep settling conservatively — the
// request may have been served in full upstream, and that work includes the
// fixed fee.
func TestAmbiguousStreamFailureStillSettlesConservatively(t *testing.T) {
	cause := &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}
	inFlight := &provider.Error{
		Class:     provider.ErrorMalformed,
		Ambiguous: !provider.Unsent(cause),
		Cause:     cause,
	}
	fixedFee := provider.Target{FixedRequestMicrosUSD: 250}
	settlement := streamSettlement(
		nil, inFlight, false,
		1_000, 2_000, 0, fixedFee, 5_000,
	)
	if !settlement.TokenEstimated || !settlement.CostEstimated {
		t.Fatalf("an ambiguous stream failure was not settled conservatively: %#v", settlement)
	}
	if settlement.CommittedMicrosUSD < fixedFee.FixedRequestMicrosUSD {
		t.Fatalf("an ambiguous stream failure dropped the fixed fee: %#v", settlement)
	}
}

// policyRefusedConnection is the error an adapter produces when SafeTransport
// declines to dial at all — a Provider base URL resolving inside the network
// while private endpoints are off refuses every request this way.
func policyRefusedConnection() error {
	cause := fmt.Errorf("outbound host %q: %w: %w", "api.internal",
		safetransport.ErrRefusedBeforeSend, errors.New("address 10.0.0.5 is not allowed"))
	return &provider.Error{
		Class:     provider.ErrorConnect,
		Retryable: true,
		Ambiguous: !provider.Unsent(cause),
		Message:   "provider request failed",
		Cause:     cause,
	}
}

// Our own refusal used to be the worst-classified failure in the system: a real
// dial failure carries *net.OpError and was recognised as unsent, while the case
// where this process guaranteed nothing left it arrived as a plain error and was
// recorded as possibly-executed. That charged the project a whole estimated
// completion for a request no socket ever carried, and stopped the route from
// failing over to a deployment that works.
func TestProviderRefusedByPolicyFailsOverAndIsNotBilled(t *testing.T) {
	if !retryable(policyRefusedConnection()) {
		t.Fatal("a connection this gateway refused itself did not fail over to the next deployment")
	}
	settlement := settlementForResult(
		semantic.GenerateResult{}, policyRefusedConnection(),
		1_000, 2_000, provider.Target{}, 5_000,
	)
	if settlement.ProviderInputTokens != 0 || settlement.ProviderOutputTokens != 0 {
		t.Fatalf("a request refused before dialling was billed for tokens: %#v", settlement)
	}
	if settlement.TokenEstimated || settlement.CostEstimated {
		t.Fatalf("a request refused before dialling produced an estimated charge: %#v", settlement)
	}
}
