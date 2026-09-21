package routegate

import (
	"errors"
	"strings"
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
	// persisted says a row for this scope is in the store, so a deletion has
	// something to remove and a probe-driven clear does not open a transaction
	// for a row that was never written.
	persisted bool
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
	// probes is the last active-probe verdict per deployment, kept for the
	// console and the metrics endpoint. It lives here rather than on the
	// registry because a reload replaces the registry and must not replace what
	// is known about upstream health — carrying it across a swap was extra
	// machinery that a gate outside the registry simply does not need.
	probes map[string]DeploymentProbe
	// transitions and probeOutcomes are counters rather than derived from
	// scopes, because both describe events the snapshot cannot: a suspension
	// that came and went between two scrapes happened, and a gauge that never
	// saw it reads as a quiet period.
	transitions   map[provider.FailureReason]uint64
	probeOutcomes map[probeOutcomeKey]uint64
	// pending is what the current locked section decided the store owes,
	// applied by flush once the mutex is released.
	pending []persistAction
	persist persistence
}

type probeOutcomeKey struct {
	reason  provider.FailureReason
	outcome string
}

func New(config Config) *Gate {
	return &Gate{
		config:        config.withDefaults(),
		policies:      DefaultPolicies(),
		scopes:        make(map[Scope]*scopeState),
		probes:        make(map[string]DeploymentProbe),
		transitions:   make(map[provider.FailureReason]uint64),
		probeOutcomes: make(map[probeOutcomeKey]uint64),
	}
}

// policyFor maps one observation onto the treatment its reason earns.
//
// An observation with no canonical reason is still evidence that something is
// not serving, and which thing depends on whether an upstream answered at all.
// A refused dial or a timeout before headers is about the endpoint; anything
// else — a 5xx, a body that would not parse — is one deployment answering badly,
// which on a large provider is routine and says nothing about the other models
// behind the same address. Collapsing the two would suspend a whole provider
// because one model returned a 500.
func (g *Gate) policyFor(observation Observation) (Policy, bool) {
	if policy, known := g.policies[observation.Reason]; known && observation.Reason != "" {
		return policy, true
	}
	scope := ScopeDeployment
	if observation.Status == 0 && !observation.Malformed &&
		(observation.Class == provider.ErrorConnect || observation.Class == provider.ErrorTimeout) {
		scope = ScopeProvider
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
	g.observeReporting(target, observation, now)
}

// observeReporting is Observe, and says whether the scope came out of service as
// a result.
func (g *Gate) observeReporting(target provider.Target, observation Observation, now time.Time) bool {
	scope, policy, ok := g.scopeAndPolicy(observation, target)
	if !ok {
		return false
	}
	defer g.flush()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.observeLocked(scope, policy, observation, now, target.CredentialRevision)
}

// observeLocked is the one place a scope is suspended, and therefore the one
// place the transition is counted.
//
// Counting in observeReporting instead left every probe-driven suspension out:
// ObserveProbe reaches here directly, at threshold one, so the health loop's
// main lever never appeared in a counter whose stated purpose is catching
// suspensions a between-scrapes gauge misses — and a 30-second window opened by
// a probe is exactly that kind.
//
// It reports whether this observation is what took the scope out of service.
func (g *Gate) observeLocked(
	scope Scope, policy Policy, observation Observation, now time.Time, credentialRevision uint64,
) bool {
	state := g.scopes[scope]
	if state == nil {
		state = &scopeState{}
		g.scopes[scope] = state
	}
	wasSuspended := state.suspended()
	state.policy = policy
	state.failures++
	if state.failures < policy.Threshold && !wasSuspended {
		return false
	}
	// The revision comes from the target, not from the caller's description of
	// the failure. It is a property of the thing being suspended rather than of
	// what went wrong, and an observation that forgot to carry it would record a
	// suspension against revision zero — which every live credential is already
	// ahead of, so it would read as stale the moment it was written.
	state.evidence = Evidence{
		Reason: observation.Reason, Status: observation.Status, Code: observation.Code,
		ObservedAt: now, CredentialRevision: credentialRevision,
	}
	if policy.Recovery == RecoverOnCredentialRevision {
		state.indefinite = true
		state.suspendedUntil = time.Time{}
		g.notePersistLocked(scope, state)
		if !wasSuspended {
			g.transitions[observation.Reason]++
		}
		return !wasSuspended
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
	// Written here rather than only on the first suspension: a failed probe
	// doubles the window, and a row still naming the old one would restore a
	// suspension that ends earlier than the gate decided it should.
	g.notePersistLocked(scope, state)
	if !wasSuspended {
		g.transitions[observation.Reason]++
	}
	return !wasSuspended
}

// ObserveSuccess clears every scope this target belongs to.
//
// A request that completed proves more than one thing at once: the endpoint
// answered, the deployment served, and the credential was accepted. Clearing
// only the narrowest would leave a credential suspended by an earlier refusal
// while requests using it are visibly succeeding.
func (g *Gate) ObserveSuccess(target provider.Target, now time.Time) {
	defer g.flush()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, scope := range scopesOf(target) {
		g.dropLocked(scope)
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

// blockingScopes lists the scopes that would refuse this target.
func (g *Gate) blockingScopes(target provider.Target, now time.Time) []Scope {
	var blocked []Scope
	for _, scope := range scopesOf(target) {
		state := g.scopes[scope]
		if state == nil || !state.suspended() {
			continue
		}
		if staleLocked(scope, state, target) {
			g.dropLocked(scope)
			continue
		}
		if state.probeable(now, g.config.HalfOpenMaxRequests) {
			continue
		}
		blocked = append(blocked, scope)
	}
	return blocked
}

// Filter removes targets that are suspended, preserving order.
//
// It runs during candidate resolution rather than only at dispatch so a
// suspended target costs nothing: no pricing lock, no price pin written and
// deleted, no upstream round trip. Admit still checks, because resolution and
// dispatch are not the same instant and the probe slot has to be claimed once.
//
// Filtering everything away is a real answer, not a failure of nerve. The
// suspension window is what bounds how often a failing upstream is retried, and
// letting a request through because nothing else is left would remove that bound
// exactly when the upstream is least likely to answer. The caller is better
// served by a fast refusal that says why, and the window ends in a probe anyway.
func (g *Gate) Filter(targets []provider.Target, now time.Time) []provider.Target {
	if len(targets) == 0 {
		return targets
	}
	defer g.flush()
	g.mu.Lock()
	defer g.mu.Unlock()
	admitted := make([]provider.Target, 0, len(targets))
	for _, target := range targets {
		if len(g.blockingScopes(target, now)) == 0 {
			admitted = append(admitted, target)
		}
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
	defer g.flush()
	g.mu.Lock()
	defer g.mu.Unlock()
	var claimed []Scope
	for _, scope := range scopesOf(target) {
		state := g.scopes[scope]
		if state == nil || !state.suspended() {
			continue
		}
		if staleLocked(scope, state, target) {
			g.dropLocked(scope)
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

// Done reports what the attempt learned, and says whether that took something
// out of service.
//
// The caller needs the answer. A refusal the gate now remembers is one the next
// candidate can be tried against, because the cost of that discovery has been
// paid once and will not be paid again until the suspension lifts — which is
// what makes walking on after a stated refusal safe, where without a memory it
// meant every request paying the same failed round trip.
//
// A nil observation means the upstream answered.
func (l *Lease) Done(observation *Observation, now time.Time) bool {
	if l == nil {
		return false
	}
	suspended := false
	l.once.Do(func() {
		if observation == nil {
			l.gate.recordProbeOutcome(l.claimed, "recovered")
			l.gate.ObserveSuccess(l.target, now)
			return
		}
		l.gate.recordProbeOutcome(l.claimed, "still_failing")
		l.gate.releaseClaims(l.claimed)
		suspended = l.gate.observeReporting(l.target, *observation, now)
	})
	return suspended
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

// Clear removes one suspension from the live gate, and only from there.
//
// It is the operator's escape hatch for a scope being held down over something
// already dealt with upstream, which makes it an administrative action: the
// caller owes an audit record and owes the stored row's removal, and owes them
// in one transaction, because a clear whose record and whose removal could
// disagree is the thing the …WithAuditIntent pairing exists to prevent. The
// gate can write neither, so it does not try — and it does not queue a store
// delete of its own, which would be a second uncommitted write racing the
// audited one.
func (g *Gate) Clear(scope Scope) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, present := g.scopes[scope]; !present {
		return false
	}
	delete(g.scopes, scope)
	return true
}

// recordProbeOutcome counts what the requests admitted through an expired window
// found. It is the figure that says whether the windows are set anywhere near
// right: a scope that recovers on nearly every probe is being suspended for too
// long, and one that almost never does is being probed too eagerly.
func (g *Gate) recordProbeOutcome(claimed []Scope, outcome string) {
	if len(claimed) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, scope := range claimed {
		state := g.scopes[scope]
		if state == nil {
			continue
		}
		g.probeOutcomes[probeOutcomeKey{reason: state.evidence.Reason, outcome: outcome}]++
	}
}

// SuspensionCounts is what the metrics endpoint renders: how many scopes are out
// right now, how many suspensions have begun, and what the probes found.
type SuspensionCounts struct {
	// Current is keyed by scope kind and reason, both enumerations. Identity
	// stays out of it — a Prometheus label carrying a credential id is a label
	// set that grows with the operator's account list and a secret-adjacent
	// identifier in a file everyone scrapes.
	Current     map[ScopeReason]int
	Transitions map[provider.FailureReason]uint64
	Probes      map[ProbeOutcome]uint64
}

// ScopeReason is one cell of the current-suspension gauge.
type ScopeReason struct {
	Kind   ScopeKind
	Reason provider.FailureReason
}

// ProbeOutcome is one cell of the probe-result counter.
type ProbeOutcome struct {
	Reason  provider.FailureReason
	Outcome string
}

// Counts snapshots everything the metrics endpoint needs in one lock.
func (g *Gate) Counts(now time.Time) SuspensionCounts {
	g.mu.Lock()
	defer g.mu.Unlock()
	counts := SuspensionCounts{
		Current:     make(map[ScopeReason]int),
		Transitions: make(map[provider.FailureReason]uint64, len(g.transitions)),
		Probes:      make(map[ProbeOutcome]uint64, len(g.probeOutcomes)),
	}
	for scope, state := range g.scopes {
		if !state.suspended() {
			continue
		}
		counts.Current[ScopeReason{Kind: scope.Kind, Reason: state.evidence.Reason}]++
	}
	for reason, count := range g.transitions {
		counts.Transitions[reason] = count
	}
	for key, count := range g.probeOutcomes {
		counts.Probes[ProbeOutcome{Reason: key.reason, Outcome: key.outcome}] = count
	}
	return counts
}

// ForgetOutdatedCredentials drops suspensions recorded against a credential
// revision the operator has already moved past.
//
// Without it the clear is traffic-dependent, and the traffic is exactly what the
// suspension stopped. A credential refusal has no window and no Retry-After —
// deliberately, since nothing is scheduled to fix it — so callers are being told
// not to come back. An operator who then replaces the secret would see
// halro_route_suspended and a critical HalroCredentialUnusable go on firing
// until some request happened to arrive for that scope and trip the staleness
// check on the way through. On a quiet alias that is never.
//
// Called on every registry reload, which is what a durable credential mutation
// already triggers. So replacing the secret really is the whole of the fix, in
// the way the runbook says it is.
func (g *Gate) ForgetOutdatedCredentials(revisions map[string]uint64) {
	defer g.flush()
	g.mu.Lock()
	defer g.mu.Unlock()
	for scope, state := range g.scopes {
		credentialID := ""
		switch scope.Kind {
		case ScopeCredential:
			credentialID = scope.Key
		case ScopeCredentialModel:
			credentialID, _, _ = strings.Cut(scope.Key, "\x00")
		default:
			continue
		}
		current, known := revisions[credentialID]
		if !known {
			// The credential is gone from the topology entirely — deleted, or
			// its provider disabled. Nothing it was suspended for can matter,
			// and keeping the entry would leak one scope per deleted credential
			// for the life of the process.
			g.dropLocked(scope)
			continue
		}
		if current > state.evidence.CredentialRevision {
			g.dropLocked(scope)
		}
	}
}
