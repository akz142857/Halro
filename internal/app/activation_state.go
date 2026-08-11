package app

import (
	"context"
	"net/http"
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

// activationTracker records whether the live snapshots still reflect the store.
type activationTracker struct {
	mu         sync.Mutex
	staleSince time.Time
	reason     string
	generation uint64
}

// markStale records that a durable mutation did not reach the live snapshots.
// The first failure's time is kept: what matters is how long the runtime has
// been serving a snapshot it knows is behind, not when it last retried.
func (t *activationTracker) markStale(reason string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.staleSince.IsZero() {
		t.staleSince = now
	}
	t.reason = reason
}

// markCurrent records a successful activation and advances the generation the
// live snapshots were built at.
func (t *activationTracker) markCurrent() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.staleSince = time.Time{}
	t.reason = ""
	t.generation++
}

type activationStatus struct {
	Stale      bool      `json:"stale"`
	StaleSince time.Time `json:"stale_since,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Generation uint64    `json:"generation"`
}

func (t *activationTracker) status() activationStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return activationStatus{
		Stale: !t.staleSince.IsZero(), StaleSince: t.staleSince,
		Reason: t.reason, Generation: t.generation,
	}
}

// refuseWhileSnapshotsStale turns a known-stale runtime away from the data
// plane.
//
// It is placed ahead of the per-source limiter and the auth guard on purpose:
// the whole reason to refuse is that the authorization snapshot cannot be
// trusted, so nothing should be decided from it first. Health endpoints stay
// outside this group — an orchestrator has to be able to see the state, and
// readiness reports it separately.
func (r *Runtime) refuseWhileSnapshotsStale(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		status := r.activation.status()
		if !status.Stale {
			next.ServeHTTP(writer, request)
			return
		}
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"type":    "service_unavailable",
				"code":    "configuration_stale",
				"message": "a durable configuration change has not reached the running snapshots; requests are refused until it does",
			},
		})
	})
}

// runActivationRecovery retries activation while the runtime is stale.
//
// It exists because the failure that made the runtime stale is usually
// transient — a store read that lost its context, a snapshot build interrupted
// by shutdown pressure — and without a retry the instance would stay refusing
// traffic until an operator happened to make another mutation.
func (r *Runtime) runActivationRecovery(ctx context.Context) {
	ticker := time.NewTicker(activationRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !r.activation.status().Stale {
				continue
			}
			r.adminTopologyMu.Lock()
			err := r.activateTopology()
			r.adminTopologyMu.Unlock()
			if err != nil {
				continue
			}
			if err := r.reloadAdminAuth(ctx); err != nil {
				r.activation.markStale("auth snapshot: "+err.Error(), time.Now().UTC())
				continue
			}
			r.logger.Info("runtime snapshots caught up with the store after a failed activation")
		}
	}
}
