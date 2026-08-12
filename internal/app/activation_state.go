package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Activation staleness
//
// The store commit is the commit point for an admin mutation, which means a
// failed activation leaves a mutation that is durable but not yet in force. For
// an addition that is merely late. For a revocation — a disabled key, a deleted
// project, a removed route — it is the fail-open direction: the live snapshot
// still authorizes what the operator just took away.
//
// The data plane therefore refuses while the snapshots are known to be behind
// the store, rather than serving from a snapshot it knows is stale. Refusing is
// recoverable and visible; serving a revoked credential is neither. A retry
// loop clears the state as soon as an activation succeeds, so this is an
// outage that heals itself once the underlying cause does.

// activationRetryInterval is how often a stale runtime tries to catch up.
//
// It is short because the state it clears refuses live traffic, and the work is
// one store read plus a snapshot swap — cheap next to what it is holding up.
const activationRetryInterval = 5 * time.Second

type staleErrorProtocol uint8

const (
	staleErrorOpenAI staleErrorProtocol = iota
	staleErrorAnthropic
)

type activationDomain string

const (
	activationDomainTopology   activationDomain = "topology"
	activationDomainAuth       activationDomain = "auth"
	activationDomainRedaction  activationDomain = "redaction"
	activationDomainTokenGuard activationDomain = "token_guard"
)

var activationDomains = [...]activationDomain{
	activationDomainTopology,
	activationDomainAuth,
	activationDomainRedaction,
	activationDomainTokenGuard,
}

type activationDomainState struct {
	staleSince time.Time
	reason     string
}

// activationTracker records which live snapshots no longer reflect the store.
//
// The domains are independent. A routing-registry activation cannot prove that
// a redaction or Token Guard snapshot caught up, so a success only clears the
// domain it actually rebuilt. The runtime remains fail-closed while any domain
// is stale.
type activationTracker struct {
	mu         sync.Mutex
	stale      map[activationDomain]activationDomainState
	generation uint64
}

// markStale records that a durable mutation did not reach the live snapshots.
// The first failure's time is kept: what matters is how long the runtime has
// been serving a snapshot it knows is behind, not when it last retried.
func (t *activationTracker) markStale(domain activationDomain, reason string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stale == nil {
		t.stale = make(map[activationDomain]activationDomainState)
	}
	state := t.stale[domain]
	if state.staleSince.IsZero() {
		state.staleSince = now
	}
	state.reason = reason
	t.stale[domain] = state
}

// markCurrent records a successful activation and advances the generation the
// live snapshots were built at.
func (t *activationTracker) markCurrent(domain activationDomain) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.stale, domain)
	t.generation++
}

type activationDomainStatus struct {
	Domain     activationDomain `json:"domain"`
	Stale      bool             `json:"stale"`
	StaleSince time.Time        `json:"stale_since,omitempty"`
	Reason     string           `json:"reason,omitempty"`
}

type activationStatus struct {
	Stale      bool                     `json:"stale"`
	StaleSince time.Time                `json:"stale_since,omitempty"`
	Reason     string                   `json:"reason,omitempty"`
	Generation uint64                   `json:"generation"`
	Domains    []activationDomainStatus `json:"domains"`
}

func (t *activationTracker) status() activationStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	status := activationStatus{
		Stale: len(t.stale) > 0, Generation: t.generation,
		Domains: make([]activationDomainStatus, 0, len(activationDomains)),
	}
	reasons := make([]string, 0, len(t.stale))
	for _, domain := range activationDomains {
		state, stale := t.stale[domain]
		status.Domains = append(status.Domains, activationDomainStatus{
			Domain: domain, Stale: stale, StaleSince: state.staleSince, Reason: state.reason,
		})
		if !stale {
			continue
		}
		if status.StaleSince.IsZero() || state.staleSince.Before(status.StaleSince) {
			status.StaleSince = state.staleSince
		}
		reasons = append(reasons, string(domain)+": "+state.reason)
	}
	status.Reason = strings.Join(reasons, "; ")
	return status
}

// refuseWhileSnapshotsStale turns a known-stale runtime away from the data
// plane.
//
// It is placed ahead of the per-source limiter and the auth guard on purpose:
// the whole reason to refuse is that the authorization snapshot cannot be
// trusted, so nothing should be decided from it first. Health endpoints stay
// outside this group — an orchestrator has to be able to see the state, and
// readiness reports it separately.
func (r *Runtime) refuseWhileSnapshotsStale(protocol staleErrorProtocol) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			status := r.activation.status()
			if !status.Stale {
				next.ServeHTTP(writer, request)
				return
			}
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("Retry-After", strconv.FormatInt(int64(activationRetryInterval/time.Second), 10))
			message := "a durable configuration change has not reached the running snapshots; requests are refused until it does"
			if protocol == staleErrorAnthropic {
				writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
					"type": "error",
					"error": map[string]any{
						"type":    "overloaded_error",
						"message": "configuration_stale: " + message,
					},
				})
				return
			}
			writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]any{
					"type": "service_unavailable", "code": "configuration_stale",
					"message": message, "param": nil,
				},
			})
		})
	}
}

// runActivationRecovery retries post-commit work owned by the runtime.
//
// Activation is retried only while stale. Durable Admin audit intents are
// checked every tick independently: an audit append failure does not make a
// serving snapshot stale, but it must not wait for another process start.
// The interval is a parameter so a test can drive the loop instead of waiting
// on it. The production call site passes activationRetryInterval, which is also
// the value the Retry-After header advertises.
func (r *Runtime) runActivationRecovery(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.recoverAdminAuditIntents(); err != nil {
				r.logger.Error("pending admin audit delivery retry failed", "error", err)
			}
			if r.activation.status().Stale {
				r.recoverActivationDomains()
			}
		}
	}
}

// recoverActivationDomains replays every snapshot domain after any activation
// failure. Replaying all four is deliberate: the tracker may contain more than
// one failed domain, and a successful domain may have changed again while the
// runtime was refusing traffic. Each activation only clears its own marker.
func (r *Runtime) recoverActivationDomains() {
	r.adminTopologyMu.Lock()
	_ = r.activateTopology()
	r.adminTopologyMu.Unlock()
	r.activateAuthSnapshot()
	r.activateRedactionPolicies()
	r.activateTokenGuardPolicies()
	if !r.activation.status().Stale {
		r.logger.Info("runtime snapshots caught up with the store after a failed activation")
	}
}
