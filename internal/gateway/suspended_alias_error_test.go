package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/routegate"
)

// An alias with nothing left to try has to say which kind of nothing.
//
// Rerouting silently around an exhausted upstream is how an outage is discovered
// when the *last* provider dies. One sentence for every cause is the same
// failure in slower motion: a caller told "temporarily unavailable" retries
// through a spent quota, and an operator reading the same words goes looking for
// a network problem.
func TestAnAliasWithNothingLeftSaysWhichKindOfNothing(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		reason         provider.FailureReason
		status         int
		wantMessage    string
		wantRetryAfter bool
	}{
		{
			name: "quota", reason: provider.FailureReasonSubscriptionQuotaExhausted, status: 402,
			wantMessage: "exhausted its quota", wantRetryAfter: true,
		},
		{
			name: "dead credential", reason: provider.FailureReasonInvalidCredential, status: 401,
			// No Retry-After: nothing is scheduled to fix this, and a hint that
			// says otherwise invites a retry loop that cannot succeed.
			wantMessage: "unusable credential", wantRetryAfter: false,
		},
		{
			name: "availability", status: 503,
			wantMessage: "temporarily out of service", wantRetryAfter: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, 10_000_000)
			defer f.close()
			target, ok := f.registry.Resolve("chat")
			if !ok {
				t.Fatal("fixture target missing")
			}
			for attempt := 0; attempt < 5; attempt++ {
				f.gate.Observe(target, routegateObservation(testCase.reason, testCase.status), time.Now())
			}

			_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
			var classified *Error
			if !errors.As(err, &classified) {
				t.Fatalf("unclassified error: %v", err)
			}
			if classified.Code != "all_candidates_suspended" || classified.HTTPStatus != 503 {
				t.Fatalf("code=%q status=%d", classified.Code, classified.HTTPStatus)
			}
			if !strings.Contains(classified.Message, testCase.wantMessage) {
				t.Fatalf("message = %q, want it to mention %q", classified.Message, testCase.wantMessage)
			}
			if hasRetry := classified.RetryAfter > 0; hasRetry != testCase.wantRetryAfter {
				t.Fatalf("retry_after=%v, want present=%v", classified.RetryAfter, testCase.wantRetryAfter)
			}
			// The caller is across a trust boundary: which of an operator's
			// accounts ran dry is not something a Gateway key may enumerate by
			// reading error bodies.
			for _, identity := range []string{"cred_fixture", "dep_target_1", "target_1", "provider-model"} {
				if strings.Contains(classified.Message, identity) {
					t.Fatalf("the refusal named %q: %q", identity, classified.Message)
				}
			}
			if f.adapter.calls != 0 {
				t.Fatalf("a suspended alias still called upstream %d times", f.adapter.calls)
			}
		})
	}
}

// Two reasons at once must report the one that will not fix itself. Naming the
// transient half invites a retry that cannot work and leaves the credential
// unmentioned — which is the failure this whole phase exists to prevent.
func TestTheRefusalNamesTheProblemThatWillNotFixItself(t *testing.T) {
	f := newFixture(t, 10_000_000)
	defer f.close()
	target, ok := f.registry.Resolve("chat")
	if !ok {
		t.Fatal("fixture target missing")
	}
	now := time.Now()
	f.gate.Observe(target, routegateObservation(provider.FailureReasonRateLimited, 429), now)
	f.gate.Observe(target, routegateObservation(provider.FailureReasonInvalidCredential, 401), now)

	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	var classified *Error
	if !errors.As(err, &classified) || !strings.Contains(classified.Message, "unusable credential") {
		t.Fatalf("the transient reason was reported over the permanent one: %v", err)
	}
	if classified.RetryAfter != 0 {
		t.Fatalf("a dead credential was given a retry hint of %v", classified.RetryAfter)
	}
}

func TestLiveRateLimitCombinesOnlyCompatibleGateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name         string
		backup       routegate.Observation
		jsonRequired bool
		status       int
		code         string
		delay        time.Duration
	}{
		{"another rate limit", routegate.Observation{Reason: provider.FailureReasonRateLimited, Status: 429, RetryAfter: 3 * time.Second}, false, 429, "provider_rate_limit", 3 * time.Second},
		{"outage", routegate.Observation{Status: 503}, false, 503, "all_candidates_suspended", 5 * time.Minute},
		{"incompatible outage", routegate.Observation{Status: 503}, true, 429, "provider_rate_limit", 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := frozen()
			f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
			primary := f.registry.ResolveAll("chat")[0]
			primary.Capabilities.JSONObject = true
			backup := primary
			backup.ID, backup.DeploymentID, backup.CredentialID = "target_2", "dep_target_2", "cred_2"
			backup.Priority = 1
			backup.Capabilities.JSONObject = false
			backupAdapter := &fakeAdapter{response: f.adapter.response}
			backup.Adapter = backupAdapter
			registry := provider.NewRegistry()
			for _, target := range []provider.Target{primary, backup} {
				if err := registry.Register(target); err != nil {
					t.Fatal(err)
				}
			}
			f.registry.Replace(registry)
			for range 10 {
				// Rate-limit cases use a single explicit observation. Availability
				// needs multiple consecutive failures to trip its threshold.
				f.gate.Observe(backup, tc.backup, now)
				if tc.backup.Status == http.StatusTooManyRequests {
					break
				}
			}
			f.adapter.err = &provider.Error{Class: provider.ErrorRateLimit, StatusCode: 429, Retryable: true, RetryAfter: 7 * time.Second}
			request := chatRequest()
			if tc.jsonRequired {
				request.ResponseFormat = json.RawMessage(`{"type":"json_object"}`)
			}
			_, err := f.service.Chat(context.Background(), f.plaintext, request)
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.HTTPStatus != tc.status || refusal.Code != tc.code || refusal.RetryAfter != tc.delay {
				t.Fatalf("result = %#v, want %d %s and %s", refusal, tc.status, tc.code, tc.delay)
			}
			if f.adapter.calls != 1 || backupAdapter.calls != 0 {
				t.Fatalf("upstream attempts = (%d, %d), want (1, 0)", f.adapter.calls, backupAdapter.calls)
			}
		})
	}
}

func routegateObservation(reason provider.FailureReason, status int) routegate.Observation {
	class := provider.ErrorProvider5xx
	if reason != "" {
		class = provider.ErrorBadRequest
	}
	return routegate.Observation{Reason: reason, Status: status, Class: class}
}

// A 429 on a target's retry does not turn the 5xx it answered first into a
// rate limit: the caller would back off from a provider that is failing.
func TestARetriedRateLimitDoesNotHideAnEarlierFailure(t *testing.T) {
	now := frozen()
	var failures generationFailures
	failures.record("target_a", routegate.Refusal{}, time.Time{})
	failures.record("target_a", routegate.Refusal{Kind: routegate.RefusalRateLimited, RetryAfter: 7 * time.Second}, now)
	if _, ok := failures.allRateLimited(1, now); ok {
		t.Fatal("a 5xx followed by a 429 was reported as a rate limit")
	}
}

// A target whose end is unknown says nothing about when; it must not erase the
// Retry-After another target of the same kind supplied.
func TestAnUnknownEndDoesNotHideAKnownRetryAfter(t *testing.T) {
	now := frozen()
	var failures generationFailures
	failures.record("target_a", routegate.Refusal{Kind: routegate.RefusalQuotaExhausted, RetryAfter: 10 * time.Minute}, now)
	failures.record("target_b", routegate.Refusal{Kind: routegate.RefusalQuotaExhausted}, now)
	refusal, ok := failures.otherSuspension(now)
	if !ok || refusal != (routegate.Refusal{Kind: routegate.RefusalQuotaExhausted, RetryAfter: 10 * time.Minute}) {
		t.Fatalf("refusal = %#v, want quota with the known ten minutes", refusal)
	}
}

// The caller is told no longer a wait than the gate itself honours: one bad
// header must not send a client away for a day while the route recovers in
// fifteen minutes.
func TestAnUpstreamHintToTheCallerIsBounded(t *testing.T) {
	mapped := mapProviderError(&provider.Error{Class: provider.ErrorRateLimit, StatusCode: http.StatusTooManyRequests, RetryAfter: provider.MaxRetryAfter})
	var classified *Error
	if !errors.As(mapped, &classified) || classified.RetryAfter != routegate.MaxUpstreamRetryAfter {
		t.Fatalf("mapped = %#v, want Retry-After %v", mapped, routegate.MaxUpstreamRetryAfter)
	}
	now := frozen()
	f := newDeferredFixtureAt(t, func() time.Time { return now }, nil)
	f.adapter.err = &provider.Error{Class: provider.ErrorRateLimit, StatusCode: http.StatusTooManyRequests, Retryable: true, RetryAfter: provider.MaxRetryAfter}
	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	if !errors.As(err, &classified) || classified.HTTPStatus != http.StatusTooManyRequests || classified.RetryAfter > routegate.MaxUpstreamRetryAfter {
		t.Fatalf("chat = %v (Retry-After %v), want a 429 bounded by %v", err, classified.RetryAfter, routegate.MaxUpstreamRetryAfter)
	}
}
