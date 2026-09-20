package routegate

import (
	"time"

	"github.com/akz142857/Halro/internal/provider"
)

// Observation is what one attempt or one probe learned about an upstream.
//
// It is deliberately not the gateway's FailureDescriptor: this package must not
// depend on the protocol layer that builds one, and it needs far less. Every
// field here is either produced by Halro or an enumerated value — no upstream
// prose reaches this package, and nothing it keeps can be read back as one.
type Observation struct {
	// Reason is the canonical cross-provider conclusion, empty when the
	// classifier reached none.
	Reason provider.FailureReason
	// Class is how the adapter classified the failure. It is what decides
	// whether an upstream answered at all, and therefore whether the failure is
	// about one deployment or about the endpoint. The status alone cannot: an
	// adapter may classify a 5xx without filling one in, and a scope chosen from
	// a missing number would be the endpoint's by accident.
	Class provider.ErrorClass
	// Status is the upstream's HTTP status, or 0 for a failure that never got
	// one. Kept as evidence rather than as a signal.
	Status int
	// Code is the upstream's machine-readable code, already narrowed through
	// provider.SafeProviderIdentifier by the caller.
	Code string
	// RetryAfter is what the upstream asked for, when it asked. Zero otherwise.
	RetryAfter time.Duration
	// CredentialRevision is the revision in force when this was observed, so a
	// credential suspension can tell itself apart from a later one.
	CredentialRevision uint64
	// Malformed marks a response Halro could not read as the protocol it claims.
	// It is an availability signal rather than a refusal, and it carries a
	// status, so it needs saying separately.
	Malformed bool
}

// Recovery is how a suspension may end.
type Recovery string

const (
	// RecoverAfterWindow expires on the clock and then admits a probe.
	RecoverAfterWindow Recovery = "window"
	// RecoverOnCredentialRevision never expires on a clock. A dead credential
	// does not heal, and a backoff against one is just a slower way of hammering
	// it forever; what ends it is an operator replacing the secret, which
	// advances the revision.
	RecoverOnCredentialRevision Recovery = "credential_revision"
)

// Policy is how one kind of refusal is treated.
type Policy struct {
	// Scope is what this reason is a fact about.
	Scope ScopeKind
	// Threshold is how many consecutive observations suspend the scope. One for
	// anything the upstream stated outright — it said it will not serve this, and
	// counting to five changes nothing — and more for availability, where a
	// single 5xx is noise.
	Threshold int
	// Window is the first suspension, doubling up to MaxWindow on each failed
	// probe. Ignored when Recovery is RecoverOnCredentialRevision.
	Window    time.Duration
	MaxWindow time.Duration
	Recovery  Recovery
	// HonourRetryAfter takes the upstream's own number as the first window when
	// it gave one. It is the only case where the window is not Halro's guess.
	HonourRetryAfter bool
}

// The design this implements had one more field here: whether to let a request
// through a suspended scope when every candidate was suspended, rather than
// refusing outright. It is not implemented, because working it through turned it
// into the opposite of a favour.
//
// The suspension window is the only thing bounding how often a failing upstream
// is retried. Overriding it because nothing else is available removes that bound
// exactly when the upstream is least likely to answer, and the half-open slot
// caps concurrency rather than rate — so a busy alias would hand a steady stream
// of callers a slow failure instead of handing all of them a fast 503 with a
// Retry-After. The window is 30 seconds by default and ends in a probe either
// way, so what the override buys is a worse answer for the callers inside it.
//
// What the distinction does earn is the answer's shape rather than an extra
// attempt: an alias that is down because its quota is spent says so, and does
// not read as a transient outage. That belongs in the refusal, not here.

// DefaultPolicies is the table §4.3 of the design specifies.
//
// The numbers are a starting point rather than a conclusion, and are meant to be
// re-cut against the distribution halro_provider_failure_reason_total collects
// in production (#319). They are here rather than in a config file because a
// per-reason table an operator is invited to tune before anyone has measured it
// is a way of asking the wrong person.
func DefaultPolicies() map[provider.FailureReason]Policy {
	return map[provider.FailureReason]Policy{
		provider.FailureReasonRateLimited: {
			Scope: ScopeCredentialModel, Threshold: 1,
			Window: time.Second, MaxWindow: time.Minute,
			Recovery: RecoverAfterWindow, HonourRetryAfter: true,
		},
		provider.FailureReasonSubscriptionQuotaExhausted: {
			Scope: ScopeCredentialModel, Threshold: 1,
			Window: 15 * time.Minute, MaxWindow: 6 * time.Hour,
			Recovery: RecoverAfterWindow, HonourRetryAfter: true,
		},
		provider.FailureReasonEntitlementVerificationUnavailable: {
			Scope: ScopeCredential, Threshold: 1,
			Window: 30 * time.Second, MaxWindow: 5 * time.Minute,
			Recovery: RecoverAfterWindow, HonourRetryAfter: true,
		},
		provider.FailureReasonInvalidCredential: {
			Scope: ScopeCredential, Threshold: 1,
			Recovery: RecoverOnCredentialRevision,
		},
		provider.FailureReasonSubscriptionInactive: {
			Scope: ScopeCredential, Threshold: 1,
			Recovery: RecoverOnCredentialRevision,
		},
	}
}

// availabilityPolicy covers the failures that carry no canonical reason but are
// still evidence that something is not serving. Its threshold is the one the
// circuit breaker used, because that is the judgement it is replacing: a single
// 5xx is noise, a run of them is not.
func availabilityPolicy(scope ScopeKind, threshold int, window, maxWindow time.Duration) Policy {
	return Policy{
		Scope: scope, Threshold: threshold,
		Window: window, MaxWindow: maxWindow,
		Recovery: RecoverAfterWindow,
	}
}
