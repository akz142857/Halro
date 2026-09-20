package gateway

import (
	"sync"

	"github.com/akz142857/Halro/internal/provider"
)

// Counting upstream refusals by canonical reason.
//
// This exists to answer a question the code cannot answer about itself: how
// often does the classifier produce a reason at all, and what is arriving when
// it does not. `FailureReason` is emitted only where structured status and code
// evidence supports the conclusion (internal/provider/provider.go), and the only
// vendor-specific status semantics the tree encodes today is Kimi Code's 402 —
// so for every other upstream, "quota exhausted" and "subscription lapsed"
// currently arrive as some status with no reason attached, and nobody knows
// which status that is.
//
// Hence the second dimension. The interesting cell is not
// {reason=subscription_quota_exhausted}, which only appears once something
// already recognises it; it is {reason=unclassified, provider_status=402} —
// a refusal Halro took no meaning from. That is the observation that turns a
// classification table from a guess into a record.
//
// It is not a substitute for asking the upstreams directly. Real traffic shows
// what the installed routes happen to hit; it cannot show that a provider never
// exercised here answers differently.

// unclassifiedFailureReason is the label used where the classifier produced no
// canonical reason. An empty label value would read as "the metric was not
// filled in" rather than as the finding it is.
const unclassifiedFailureReason = "unclassified"

// maxTrackedFailureReasons bounds the series this can create. Reasons are a
// closed enum of five plus the unclassified bucket; statuses come from upstream
// responses, so they are bounded in practice and not by contract. Past the cap
// nothing new is tracked and the overflow count says how much was dropped,
// which is the same trade internal/sourcelimit makes for addresses.
const maxTrackedFailureReasons = 256

type failureReasonKey struct {
	reason string
	// status is the upstream's HTTP status, or 0 for a failure that never got
	// one — a transport refusal, a DNS answer, a connection that was never made.
	status int
}

type failureReasonCounters struct {
	mu       sync.Mutex
	counts   map[failureReasonKey]uint64
	overflow uint64
}

func (counters *failureReasonCounters) observe(descriptor FailureDescriptor) {
	// A caller that hung up did not teach anything about the upstream, and one
	// frontend deploy cancels every request in flight at once. reportBreaker
	// refuses to judge a target on this signal for the same reason.
	if descriptor.Phase == phaseClient {
		return
	}
	reason := string(descriptor.ProviderFailureReason)
	if reason == "" {
		reason = unclassifiedFailureReason
	}
	key := failureReasonKey{reason: reason, status: descriptor.ProviderStatus}
	counters.mu.Lock()
	defer counters.mu.Unlock()
	if counters.counts == nil {
		counters.counts = make(map[failureReasonKey]uint64, 16)
	}
	if _, known := counters.counts[key]; !known && len(counters.counts) >= maxTrackedFailureReasons {
		counters.overflow++
		return
	}
	counters.counts[key]++
}

// ProviderFailureReasonCount is one observed (reason, status) pair.
type ProviderFailureReasonCount struct {
	Reason string
	// Status is 0 for a failure that never carried an upstream status.
	Status int
	Count  uint64
}

// ProviderFailureReasons is the snapshot the metrics endpoint renders, plus what
// the cap dropped. Order is not defined; the caller sorts if it needs to.
type ProviderFailureReasons struct {
	Counts   []ProviderFailureReasonCount
	Overflow uint64
}

func (counters *failureReasonCounters) snapshot() ProviderFailureReasons {
	counters.mu.Lock()
	defer counters.mu.Unlock()
	result := ProviderFailureReasons{
		Counts:   make([]ProviderFailureReasonCount, 0, len(counters.counts)),
		Overflow: counters.overflow,
	}
	for key, count := range counters.counts {
		result.Counts = append(result.Counts, ProviderFailureReasonCount{
			Reason: key.reason, Status: key.status, Count: count,
		})
	}
	return result
}

// ProviderFailureReasons reports how upstream refusals classified, for the
// metrics endpoint.
func (s *Service) ProviderFailureReasons() ProviderFailureReasons {
	return s.failureReasons.snapshot()
}

// KnownFailureReasons is every label this metric can emit for a classified
// refusal, so the endpoint can publish a zero rather than leaving a reason
// absent until the first time it happens. A missing series and a series at zero
// read very differently to an alert.
func KnownFailureReasons() []string {
	return []string{
		string(provider.FailureReasonInvalidCredential),
		string(provider.FailureReasonEntitlementVerificationUnavailable),
		string(provider.FailureReasonSubscriptionInactive),
		string(provider.FailureReasonSubscriptionQuotaExhausted),
		string(provider.FailureReasonRateLimited),
		unclassifiedFailureReason,
	}
}
