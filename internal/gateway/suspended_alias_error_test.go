package gateway

import (
	"context"
	"errors"
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

func routegateObservation(reason provider.FailureReason, status int) routegate.Observation {
	class := provider.ErrorProvider5xx
	if reason != "" {
		class = provider.ErrorBadRequest
	}
	return routegate.Observation{Reason: reason, Status: status, Class: class}
}
