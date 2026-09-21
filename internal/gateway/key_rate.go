package gateway

import (
	"github.com/akz142857/Halro/internal/auth"
)

// The built-in ceiling on what one authenticated caller may ask Halro to answer
// from its own state, per minute, on its Key and on its Project.
//
// These are constants, not configuration, and that is the whole point of them.
// Halro has two other request bounds and neither holds this ground. The
// per-source limiter runs before authentication, counts addresses rather than
// principals, and `gateway.source_rate_limit.requests_per_minute: 0` is a
// supported setting an install behind a shedding proxy may legitimately choose.
// The Project limiter charges RPM, TPM and concurrency for work that is about
// to reach a provider — it is a bound on spending, which is why a Project may
// set RPM to 0 and mean it. A request Halro answers without a provider call
// spends nothing, so neither bound is the right one to charge it against, and
// before this the two together could be configured down to nothing at all.
//
// The numbers match the control plane's governance reads, which have had a
// ceiling of exactly this shape since Run Governance shipped. They are set well
// above what an integration does and well below what a loop does: a client that
// lists its models on startup, or polls a deferred response every second, never
// approaches them.
const (
	keyRateClass = "gateway"

	// KeyCeilingRPM and ProjectCeilingRPM are exported because they are part of
	// the published contract rather than an implementation detail: the manifests
	// promise the bound, and an operator sizing a polling client needs the
	// number.
	KeyCeilingRPM     = 600
	ProjectCeilingRPM = 5_000
)

// admitKeyRate charges one request to the caller's Key and Project against the
// built-in ceiling.
//
// It is applied where a request may be answered without reaching
// beginRequestRun: model discovery, and the whole resource plane — files,
// batches, async invocations and deferred responses — where a retrieval, a
// cancellation, or a 404 for an identifier that names nothing all return before
// any accounting is opened. For the calls on that plane that do go on to reach
// an upstream, this sits under the Project limiter rather than replacing it.
//
// The inference path is deliberately not charged here. Every request that
// proceeds on it reaches the Project limiter, which is the bound that belongs
// to it; the refusals that come earlier — an unknown key, an alias the Project
// may not name — are a hash and a slice scan, strictly less work than this
// would add, and adding a second process-wide lock to the hottest path to
// bound them would cost more than it saves.
func (s *Service) admitKeyRate(principal auth.AuthResult) error {
	allowed, retryAfter := s.keyRate.Allow(
		s.now(), keyRateClass, principal.Key.ID, principal.Project.ID,
		KeyCeilingRPM, ProjectCeilingRPM,
	)
	if allowed {
		return nil
	}
	s.rejections.keyRate.Add(1)
	refusal := gatewayError("rate_limit_exceeded", "gateway key request rate exceeded", 429, nil)
	refusal.RetryAfter = retryAfter
	return refusal
}
