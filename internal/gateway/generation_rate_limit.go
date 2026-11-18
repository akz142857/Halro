package gateway

import (
	"errors"
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/routegate"
)

type generationFailure struct {
	kind    routegate.RefusalKind
	retryAt time.Time
}

type generationFailures map[string]generationFailure

func (failures *generationFailures) record(targetID string, refusal routegate.Refusal, now time.Time) {
	if *failures == nil {
		*failures = make(generationFailures)
	}
	failure := generationFailure{kind: refusal.Kind}
	if refusal.RetryAfter > 0 {
		failure.retryAt = now.Add(refusal.RetryAfter)
	}
	(*failures)[targetID] = failure
}

func (failure generationFailure) retryAfter(now time.Time) time.Duration {
	if failure.retryAt.IsZero() {
		return 0
	}
	return max(time.Second, failure.retryAt.Sub(now))
}

func (failures generationFailures) allRateLimited(count int, now time.Time) (time.Duration, bool) {
	if count == 0 || count != len(failures) {
		return 0, false
	}
	var earliest time.Duration
	for _, failure := range failures {
		if failure.kind != routegate.RefusalRateLimited {
			return 0, false
		}
		if delay := failure.retryAfter(now); delay > 0 && (earliest == 0 || delay < earliest) {
			earliest = delay
		}
	}
	return earliest, true
}

func (failures generationFailures) otherSuspension(now time.Time) (routegate.Refusal, bool) {
	for _, kind := range []routegate.RefusalKind{
		routegate.RefusalCredentialUnusable, routegate.RefusalQuotaExhausted, routegate.RefusalUnavailable,
	} {
		var found bool
		var earliest time.Duration
		for _, failure := range failures {
			if failure.kind == kind {
				delay := failure.retryAfter(now)
				if !found || delay < earliest {
					earliest = delay
				}
				found = true
			}
		}
		if found {
			return routegate.Refusal{Kind: kind, RetryAfter: earliest}, true
		}
	}
	return routegate.Refusal{}, false
}

func (failures generationFailures) suspendedError(count int, now time.Time) error {
	if retryAfter, ok := failures.allRateLimited(count, now); ok {
		return upstreamRateLimitError(retryAfter)
	}
	if refusal, ok := failures.otherSuspension(now); ok {
		return suspendedAliasError(refusal)
	}
	return gatewayError("provider_unavailable", "no healthy deployment is available for this model; retry shortly", http.StatusServiceUnavailable, nil)
}

func definitiveRateLimit(err error, target provider.Target) (*provider.Error, bool) {
	var upstream *provider.Error
	if !errors.As(err, &upstream) || upstream.Ambiguous || upstream.StatusCode != http.StatusTooManyRequests || upstream.Class != provider.ErrorRateLimit {
		return nil, false
	}
	return upstream, canonicalProviderFailureReason(upstream, target) == provider.FailureReasonRateLimited
}

func upstreamRateLimitError(retryAfter time.Duration) *Error {
	err := gatewayError("provider_rate_limit", "provider rate limit exceeded", http.StatusTooManyRequests, nil)
	err.RetryAfter = retryAfter
	return err
}
