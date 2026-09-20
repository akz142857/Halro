package routegate

import (
	"errors"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/provider"
)

// ErrSuspended is returned by Admit when every scope check did not pass. The
// gateway treats it the way it treated an open circuit: this target is out, try
// the next candidate.
var ErrSuspended = errors.New("route target is suspended")

// Config is the availability half of the table. The reason-specific half is not
// configurable (see DefaultPolicies): a per-reason window an operator is invited
// to tune before anyone has measured the distribution is asking the wrong
// person, while these three are the breaker settings operators already have.
type Config struct {
	// AvailabilityThreshold is how many consecutive failures with no canonical
	// reason suspend a scope.
	AvailabilityThreshold int
	// AvailabilityWindow is the first suspension, doubling to the maximum.
	AvailabilityWindow    time.Duration
	MaxAvailabilityWindow time.Duration
	// HalfOpenMaxRequests is how many probes may be in flight through one
	// suspended scope once its window expires.
	HalfOpenMaxRequests int
}

func (c Config) withDefaults() Config {
	if c.AvailabilityThreshold <= 0 {
		c.AvailabilityThreshold = 5
	}
	if c.AvailabilityWindow <= 0 {
		c.AvailabilityWindow = 30 * time.Second
	}
	if c.MaxAvailabilityWindow < c.AvailabilityWindow {
		c.MaxAvailabilityWindow = 5 * time.Minute
	}
	if c.HalfOpenMaxRequests <= 0 {
		c.HalfOpenMaxRequests = 1
	}
	return c
}

// Evidence is what was observed when a scope was suspended. Identifiers and
// enumerations only: an upstream's own sentence never reaches this package, so
// nothing here can quote back a credential an upstream echoed into a refusal.
type Evidence struct {
	Reason             provider.FailureReason
	Status             int
	Code               string
	ObservedAt         time.Time
	CredentialRevision uint64
}

type scopeState struct {
	failures int
	policy   Policy
	// suspendedUntil is zero while the scope is merely accumulating failures.
	suspendedUntil time.Time
	indefinite     bool
	window         time.Duration
	probing        int
	evidence       Evidence
}

func (s *scopeState) suspended() bool { return s.indefinite || !s.suspendedUntil.IsZero() }

// probeable reports that the suspension has run its course and one request may
// test it. An indefinite suspension is never probeable: the thing that ends it
// is an operator, not a clock.
func (s *scopeState) probeable(now time.Time, halfOpenMax int) bool {
	if s.indefinite || s.suspendedUntil.IsZero() {
		return false
	}
	return !now.Before(s.suspendedUntil) && s.probing < halfOpenMax
}

// Gate is the single answer to "may this target be used right now".
type Gate struct {
	mu       sync.Mutex
	config   Config
	policies map[provider.FailureReason]Policy
	scopes   map[Scope]*scopeState
}

func New(config Config) *Gate {
	return &Gate{
		config:   config.withDefaults(),
		policies: DefaultPolicies(),
		scopes:   make(map[Scope]*scopeState),
	}
}

// policyFor maps one observation onto the treatment its reason earns.
//
// An observation with no canonical reason is still evidence that something is
// not serving, and which thing depends on whether an upstream answered at all: a
// status means the upstream spoke, so the failure is about the deployment that
// spoke badly; no status means nothing was reached, which is about the endpoint.
// Collapsing the two would suspend a whole provider because one model returned
// a 500, and on a large provider that is routine.
func (g *Gate) policyFor(observation Observation) (Policy, bool) {
	if policy, known := g.policies[observation.Reason]; known && observation.Reason != "" {
		return policy, true
	}
	scope := ScopeProvider
	if observation.Status > 0 || observation.Malformed {
		scope = ScopeDeployment
	}
	return availabilityPolicy(
		scope, g.config.AvailabilityThreshold,
		g.config.AvailabilityWindow, g.config.MaxAvailabilityWindow,
	), true
}

func (g *Gate) scopeAndPolicy(observation Observation, target provider.Target) (Scope, Policy, bool) {
	policy, ok := g.policyFor(observation)
	if !ok {
		return Scope{}, Policy{}, false
	}
	for _, scope := range scopesOf(target) {
		if scope.Kind == policy.Scope {
			return scope, policy, true
		}
	}
	// The target does not carry the identity this reason is about — an older
	// registration, or a test target with no credential. Nothing is suspended
	// rather than something adjacent standing in for it.
	return Scope{}, Policy{}, false
}

// Observe records one failed attempt or probe against the scope its reason is
// about, and suspends that scope once the reason's threshold is met.
//
// For an observer that took no lease — the active prober. An attempt reports
// through its Lease instead, which gives back the half-open slot it claimed
// before recording what it learned.
func (g *Gate) Observe(target provider.Target, observation Observation, now time.Time) {
	scope, policy, ok := g.scopeAndPolicy(observation, target)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observeLocked(scope, policy, observation, now)
}

func (g *Gate) observeLocked(scope Scope, policy Policy, observation Observation, now time.Time) {
	state := g.scopes[scope]
	if state == nil {
		state = &scopeState{}
		g.scopes[scope] = state
	}
	state.policy = policy
	state.failures++
	if state.failures < policy.Threshold && !state.suspended() {
		return
	}
	state.evidence = Evidence{
		Reason: observation.Reason, Status: observation.Status, Code: observation.Code,
		ObservedAt: now, CredentialRevision: observation.CredentialRevision,
	}
	if policy.Recovery == RecoverOnCredentialRevision {
		state.indefinite = true
		state.suspendedUntil = time.Time{}
		return
	}
	// A probe that failed doubles the window it just served; a first suspension
	// takes the policy's opening figure, or the upstream's own if it gave one.
	switch {
	case state.window == 0:
		state.window = policy.Window
		if policy.HonourRetryAfter && observation.RetryAfter > state.window {
			state.window = observation.RetryAfter
		}
	default:
		state.window *= 2
	}
	if policy.MaxWindow > 0 && state.window > policy.MaxWindow {
		state.window = policy.MaxWindow
	}
	state.suspendedUntil = now.Add(state.window)
}

// ObserveSuccess clears every scope this target belongs to.
//
// A request that completed proves more than one thing at once: the endpoint
// answered, the deployment served, and the credential was accepted. Clearing
// only the narrowest would leave a credential suspended by an earlier refusal
// while requests using it are visibly succeeding.
func (g *Gate) ObserveSuccess(target provider.Target, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, scope := range scopesOf(target) {
		if state := g.scopes[scope]; state != nil {
			delete(g.scopes, scope)
		}
	}
}

// Abandon gives back a probe slot claimed by an attempt that never reached the
// upstream — a local concurrency, budget, pricing or policy rejection, or a
// caller that went away. Such an attempt learned nothing, so it must neither
// clear the suspension nor count against it; holding the slot instead would
// strand the scope suspended past its own window.
func (g *Gate) abandon(scopes []Scope) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, scope := range scopes {
		if state := g.scopes[scope]; state != nil && state.probing > 0 {
			state.probing--
		}
	}
}

// staleLocked reports a credential suspension that the operator has already
// answered. The revision advances when the secret is replaced, so a suspension
// recorded against an older one is describing a credential that no longer
// exists — which is what makes fixing the key the whole of the fix.
func staleLocked(scope Scope, state *scopeState, target provider.Target) bool {
	if state.policy.Recovery != RecoverOnCredentialRevision {
		return false
	}
	if scope.Kind != ScopeCredential && scope.Kind != ScopeCredentialModel {
		return false
	}
	return target.CredentialRevision > state.evidence.CredentialRevision
}

// blockingScopes lists the scopes that would refuse this target, and whether
// every one of them would tolerate a probe if nothing else were available.
func (g *Gate) blockingScopes(target provider.Target, now time.Time) (blocked []Scope, probeable bool) {
	probeable = true
	for _, scope := range scopesOf(target) {
		state := g.scopes[scope]
		if state == nil || !state.suspended() {
			continue
		}
		if staleLocked(scope, state, target) {
			delete(g.scopes, scope)
			continue
		}
		if state.probeable(now, g.config.HalfOpenMaxRequests) {
			continue
		}
		blocked = append(blocked, scope)
		if !state.policy.ProbeWhenNothingElseIsLeft {
			probeable = false
		}
	}
	return blocked, probeable
}

// Filter removes targets that are suspended, preserving order.
//
// It runs during candidate resolution rather than only at dispatch so a
// suspended target costs nothing: no pricing lock, no price pin written and
// deleted, no upstream round trip. Admit still checks, because resolution and
// dispatch are not the same instant and the probe slot has to be claimed once.
//
// When filtering would leave nothing, one dropped target is handed back if every
// scope blocking it is the kind that might be wrong — availability, a rate limit
// that may have lifted. Reasons the upstream stated outright are not: an account
// with no quota answers the same way to the next request, so a refusal that
// costs no round trip is both faster for the caller and cheaper for everyone.
func (g *Gate) Filter(targets []provider.Target, now time.Time) []provider.Target {
	if len(targets) == 0 {
		return targets
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	admitted := make([]provider.Target, 0, len(targets))
	var fallback []provider.Target
	for _, target := range targets {
		blocked, probeable := g.blockingScopes(target, now)
		if len(blocked) == 0 {
			admitted = append(admitted, target)
			continue
		}
		if probeable {
			fallback = append(fallback, target)
		}
	}
	if len(admitted) == 0 && len(fallback) > 0 {
		return fallback[:1]
	}
	return admitted
}

// Lease is one attempt's claim on the scopes it is testing.
type Lease struct {
	once   sync.Once
	gate   *Gate
	target provider.Target
	// claimed are the scopes whose half-open slot this attempt took, and the
	// only ones Abandon has to give back.
	claimed []Scope
}

// Admit is the last check before an upstream call, and the one that claims a
// probe slot. It returns ErrSuspended for a target the gate will not serve.
func (g *Gate) Admit(target provider.Target, now time.Time) (*Lease, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var claimed []Scope
	for _, scope := range scopesOf(target) {
		state := g.scopes[scope]
		if state == nil || !state.suspended() {
			continue
		}
		if staleLocked(scope, state, target) {
			delete(g.scopes, scope)
			continue
		}
		if !state.probeable(now, g.config.HalfOpenMaxRequests) {
			// Slots taken on the way here belong to an attempt that is not going
			// to happen.
			for _, taken := range claimed {
				if held := g.scopes[taken]; held != nil && held.probing > 0 {
					held.probing--
				}
			}
			return nil, ErrSuspended
		}
		state.probing++
		claimed = append(claimed, scope)
	}
	return &Lease{gate: g, target: target, claimed: claimed}, nil
}

// Done reports what the attempt learned. A nil observation means the upstream
// answered; anything else is the refusal it answered with.
func (l *Lease) Done(observation *Observation, now time.Time) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if observation == nil {
			l.gate.ObserveSuccess(l.target, now)
			return
		}
		l.gate.releaseClaims(l.claimed)
		l.gate.Observe(l.target, *observation, now)
	})
}

// Abandon releases the claim without letting the attempt speak for anything.
func (l *Lease) Abandon() {
	if l == nil {
		return
	}
	l.once.Do(func() { l.gate.abandon(l.claimed) })
}

// releaseClaims hands back slots before Observe takes its own lock, so a probe
// that failed is counted once rather than being both released and decremented.
func (g *Gate) releaseClaims(scopes []Scope) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, scope := range scopes {
		if state := g.scopes[scope]; state != nil && state.probing > 0 {
			state.probing--
		}
	}
}

// SuspensionSnapshot is one suspended scope, for the metrics endpoint, the
// console and the Admin API.
type SuspensionSnapshot struct {
	Scope    Scope
	Reason   provider.FailureReason
	Evidence Evidence
	// Until is zero for a suspension no clock will end.
	Until      time.Time
	Indefinite bool
}

// Snapshot lists what is suspended right now. Identity is in here rather than in
// a metric label: an operator needs to know which credential, and Prometheus
// must not carry one.
func (g *Gate) Snapshot(now time.Time) []SuspensionSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make([]SuspensionSnapshot, 0, len(g.scopes))
	for scope, state := range g.scopes {
		if !state.suspended() {
			continue
		}
		result = append(result, SuspensionSnapshot{
			Scope: scope, Reason: state.evidence.Reason, Evidence: state.evidence,
			Until: state.suspendedUntil, Indefinite: state.indefinite,
		})
	}
	return result
}

// Clear removes one suspension. It is the operator's escape hatch for a scope
// the gate is holding down for a reason that has been dealt with outside Halro's
// view, and it is an administrative action: whoever wires it up owes an audit
// record, which the gate itself cannot write.
func (g *Gate) Clear(scope Scope) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, present := g.scopes[scope]; !present {
		return false
	}
	delete(g.scopes, scope)
	return true
}
