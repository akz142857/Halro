package openai

import (
	"testing"

	"github.com/akz142857/Halro/internal/provider"
)

// The measured body, verbatim from api.minimax.io on 2026-08-31. HTTP 200.
const minimaxMeasured200Error = `{"base_resp":{"status_code":1004,"status_msg":"login fail: Please carry the API secret key in the 'Authorization' field of the request header"}}`

func TestMiniMaxGuardCatchesAFailureWearingA200(t *testing.T) {
	err := checkMiniMaxBaseResp([]byte(minimaxMeasured200Error))
	if err == nil {
		t.Fatal("a 200 carrying base_resp 1004 was read as a success; the attempt would settle as one")
	}
	if err.Class != provider.ErrorAuthentication {
		t.Fatalf("class is %q, want authentication", err.Class)
	}
	if err.Retryable {
		t.Fatal("an authentication failure was marked retryable")
	}
}

func TestMiniMaxGuardLetsOrdinarySuccessThrough(t *testing.T) {
	for _, payload := range []string{
		`{"id":"x","choices":[],"usage":{"total_tokens":1}}`,
		`{"base_resp":{"status_code":0,"status_msg":""}}`,
		`not json at all`,
		``,
	} {
		if err := checkMiniMaxBaseResp([]byte(payload)); err != nil {
			t.Fatalf("payload %q was refused as %v; a missing or zero envelope is not evidence of failure", payload, err)
		}
	}
}

// The class is what retry bounding, the circuit breaker and route failover read.
// A rate limit reported inside a 200 that arrives without its class is a route
// that keeps looking healthy while every request on it is being turned away.
func TestMiniMaxStatusCodesReachTheRightClass(t *testing.T) {
	cases := []struct {
		code      int64
		class     provider.ErrorClass
		retryable bool
		ambiguous bool
	}{
		{1002, provider.ErrorRateLimit, true, false},
		{1004, provider.ErrorAuthentication, false, false},
		{1008, provider.ErrorAuthentication, false, false},
		{1001, provider.ErrorTimeout, true, true},
		{1013, provider.ErrorProvider5xx, true, true},
		{1000, provider.ErrorProvider5xx, true, true},
		{1027, provider.ErrorBadRequest, false, false},
		{1039, provider.ErrorBadRequest, false, false},
		{2013, provider.ErrorBadRequest, false, false},
		// Unlisted. The request was sent and the outcome is unknown, which is
		// what ambiguous means; settling it as free would hide a real charge.
		{4242, provider.ErrorProvider5xx, false, true},
	}
	for _, want := range cases {
		got := classifyMiniMaxStatus(want.code)
		if got.Class != want.class || got.Retryable != want.retryable || got.Ambiguous != want.ambiguous {
			t.Errorf("code %d classified as {%s retryable=%v ambiguous=%v}, want {%s retryable=%v ambiguous=%v}",
				want.code, got.Class, got.Retryable, got.Ambiguous, want.class, want.retryable, want.ambiguous)
		}
		if got.ProviderCode == "" {
			t.Errorf("code %d carries no provider code", want.code)
		}
	}
}

// Insufficient balance arrives looking like a throttle. Retrying it cannot
// succeed, and every attempt is another call on the operator's credential.
func TestMiniMaxInsufficientBalanceIsNeverRetried(t *testing.T) {
	if classifyMiniMaxStatus(1008).Retryable {
		t.Fatal("1008 is retryable; retrying an empty balance only multiplies the calls")
	}
}
