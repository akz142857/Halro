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
