package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/routegate"
)

func refusal(status int, code string) *provider.Error {
	return &provider.Error{StatusCode: status, ProviderCode: code}
}

// The gap this table closes: six of the nine upstreams carry an exhausted
// allowance on the same 429 as an ordinary rate limit, so a classifier that
// reads the status first answers rate_limited for every one of them. The quota
// policy — fifteen minutes doubling to six hours — was unreachable, and so was
// the alert that watches for it.
func TestAnExhaustedAllowanceIsNotReadAsARateLimit(t *testing.T) {
	openAI := provider.Target{ProfileID: domain.ProfileOpenAIChatEmbeddings}
	for _, code := range []string{
		"credit_balance_exhausted",
		"organization_spend_limit_exceeded",
		"project_spend_limit_exceeded",
	} {
		if got := canonicalProviderFailureReason(refusal(429, code), openAI); got != provider.FailureReasonSubscriptionQuotaExhausted {
			t.Fatalf("OpenAI %s reason = %q, want %q", code, got, provider.FailureReasonSubscriptionQuotaExhausted)
		}
	}
	kimi := provider.Target{ProfileID: domain.ProfileKimiChat}
	if got := canonicalProviderFailureReason(refusal(429, "exceeded_current_quota_error"), kimi); got != provider.FailureReasonSubscriptionQuotaExhausted {
		t.Fatalf("Kimi reason = %q, want quota", got)
	}
	gemini := provider.Target{ProfileID: domain.ProfileGeminiText}
	if got := canonicalProviderFailureReason(refusal(429, "quota_exceeded"), gemini); got != provider.FailureReasonSubscriptionQuotaExhausted {
		t.Fatalf("Gemini daily quota reason = %q, want quota", got)
	}
	// The same vendors' rate limits keep the reason they had.
	for _, item := range []struct {
		target provider.Target
		code   string
	}{
		{kimi, "rate_limit_reached_error"},
		{kimi, "engine_overloaded_error"},
		{gemini, "rate_limit_exceeded"},
		{openAI, "rate_limit_exceeded"},
	} {
		if got := canonicalProviderFailureReason(refusal(429, item.code), item.target); got != provider.FailureReasonRateLimited {
			t.Fatalf("%s reason = %q, want rate_limited", item.code, got)
		}
	}
}

// Anthropic splits its two conditions across two statuses, and only one of them
// is safe to read.
func TestAnthropicBillingIsQuotaAndTheSpendCapIsNot(t *testing.T) {
	anthropic := provider.Target{ProfileID: domain.ProfileAnthropicMessages}
	if got := canonicalProviderFailureReason(refusal(402, "billing_error"), anthropic); got != provider.FailureReasonSubscriptionQuotaExhausted {
		t.Fatalf("402 billing_error reason = %q, want quota", got)
	}
	// The monthly spend cap arrives as 429 rate_limit_error — the same status
	// and the same type as an ordinary rate limit, separable only by a missing
	// retry-after header. Reading an indefinite condition out of an absent
	// header would turn a transient limit into a suspension a human has to
	// clear, so it stays a rate limit until a real response says otherwise.
	if got := canonicalProviderFailureReason(refusal(429, "rate_limit_error"), anthropic); got != provider.FailureReasonRateLimited {
		t.Fatalf("429 rate_limit_error reason = %q, want rate_limited", got)
	}
}

// DeepSeek is the one vendor that separates the two by status, which is the
// cheap design the other eight did not give us.
func TestDeepSeekSeparatesExhaustionByStatus(t *testing.T) {
	deepseek := provider.Target{ProfileID: domain.ProfileDeepSeekChat}
	if got := canonicalProviderFailureReason(refusal(402, ""), deepseek); got != provider.FailureReasonSubscriptionQuotaExhausted {
		t.Fatalf("DeepSeek 402 reason = %q, want quota", got)
	}
	if got := canonicalProviderFailureReason(refusal(429, ""), deepseek); got != provider.FailureReasonRateLimited {
		t.Fatalf("DeepSeek 429 reason = %q, want rate_limited", got)
	}
	// A 402 from an upstream that has not been shown to mean this by it stays
	// unclassified. Reading someone else's 402 as an exhausted balance is the
	// guess this table exists to avoid.
	openAI := provider.Target{ProfileID: domain.ProfileOpenAIChatEmbeddings}
	if got := canonicalProviderFailureReason(refusal(402, ""), openAI); got != "" {
		t.Fatalf("an unattributed 402 was read as %q", got)
	}
}

// A bare number means nothing without knowing who said it.
func TestNumericBusinessCodesNeedTheirVendor(t *testing.T) {
	bigModel := provider.Target{ProfileID: domain.ProfileBigModelCNChatEmbeddings}
	if got := canonicalProviderFailureReason(refusal(429, "1113"), bigModel); got != provider.FailureReasonSubscriptionQuotaExhausted {
		t.Fatalf("BigModel 1113 reason = %q, want quota", got)
	}
	for _, code := range []string{"1302", "1305"} {
		if got := canonicalProviderFailureReason(refusal(429, code), bigModel); got != provider.FailureReasonRateLimited {
			t.Fatalf("BigModel %s reason = %q, want rate_limited", code, got)
		}
	}
	openAI := provider.Target{ProfileID: domain.ProfileOpenAIChatEmbeddings}
	if got := canonicalProviderFailureReason(refusal(429, "1113"), openAI); got != provider.FailureReasonRateLimited {
		t.Fatalf("another vendor's 1113 was read as BigModel's: %q", got)
	}
	// A target whose profile is not in the registry — an older registration, a
	// test target — reads only the codes that need no vendor.
	unknown := provider.Target{ProfileID: domain.ProviderProfileID("not.a.profile.v1")}
	if got := canonicalProviderFailureReason(refusal(429, "1113"), unknown); got != provider.FailureReasonRateLimited {
		t.Fatalf("an unregistered profile matched a vendor table: %q", got)
	}
	if got := canonicalProviderFailureReason(refusal(429, "credit_balance_exhausted"), unknown); got != provider.FailureReasonSubscriptionQuotaExhausted {
		t.Fatalf("an unregistered profile lost a code that needs no vendor: %q", got)
	}
}

// The conclusions that were already reachable keep their precedence: an adapter
// that decided for itself is not second-guessed, and Kimi Code's 402 is an
// entitlement check rather than a spent balance.
func TestExistingConclusionsKeepPrecedence(t *testing.T) {
	stated := &provider.Error{
		StatusCode: 429, ProviderCode: "credit_balance_exhausted",
		FailureReason: provider.FailureReasonRateLimited,
	}
	openAI := provider.Target{ProfileID: domain.ProfileOpenAIChatEmbeddings}
	if got := canonicalProviderFailureReason(stated, openAI); got != provider.FailureReasonRateLimited {
		t.Fatalf("the adapter's own conclusion was overridden: %q", got)
	}
	kimiCode := provider.Target{OfferingID: domain.OfferingKimiCode}
	if got := canonicalProviderFailureReason(refusal(402, ""), kimiCode); got != provider.FailureReasonEntitlementVerificationUnavailable {
		t.Fatalf("Kimi Code 402 reason = %q, want entitlement", got)
	}
	if got := canonicalProviderFailureReason(refusal(401, "subscription_inactive"), openAI); got != provider.FailureReasonSubscriptionInactive {
		t.Fatalf("subscription_inactive reason = %q", got)
	}
}

// The payoff, through the real path: a 429 that names an exhausted balance
// suspends the credential and its model under the quota policy, not the
// rate-limit one.
//
// The difference is the whole point. A rate limit is probed again in a second
// and doubles to a minute; an exhausted allowance is not over until somebody
// pays. Before this the second was treated as the first, which is a probe every
// minute against a condition that will not change for hours — and the alert
// that watches for it could never fire, because nothing ever produced the
// reason it keys on.
func TestAQuotaRefusalEarnsTheQuotaPolicyEndToEnd(t *testing.T) {
	f := newFixture(t, 10_000)
	defer f.close()
	gate := routegate.New(routegate.Config{})
	f.registry.SetEligibility(gate)
	service, err := NewServiceWithOptions(f.service.auth, f.registry, f.accounting, ServiceOptions{
		MaxAttempts: 1, RetryBaseDelay: time.Millisecond, RetryMaxDelay: time.Millisecond, RouteGate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.adapter.err = &provider.Error{
		Class: provider.ErrorRateLimit, Retryable: true, StatusCode: 429,
		ProviderCode: "credit_balance_exhausted", Message: "provider error (429)",
	}
	if _, err := service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the refusal did not reach the caller")
	}

	suspensions := gate.Snapshot(time.Now())
	if len(suspensions) != 1 {
		t.Fatalf("suspensions = %+v, want one", suspensions)
	}
	suspension := suspensions[0]
	if suspension.Reason != provider.FailureReasonSubscriptionQuotaExhausted {
		t.Fatalf("reason = %q, want %q", suspension.Reason, provider.FailureReasonSubscriptionQuotaExhausted)
	}
	// Credential-and-model, which is where most upstreams meter a quota, and
	// not the deployment the request happened to pick.
	if suspension.Scope.Kind != routegate.ScopeCredentialModel {
		t.Fatalf("scope kind = %q, want credential_model", suspension.Scope.Kind)
	}
	// The window is the quota policy's opening figure rather than the rate
	// limiter's one second. Asserted as a range because the clock moved between
	// the refusal and the snapshot.
	remaining := time.Until(suspension.Until)
	if remaining < 14*time.Minute || remaining > 15*time.Minute {
		t.Fatalf("window = %s, want the quota policy's 15 minutes rather than the rate limiter's second", remaining)
	}
}
