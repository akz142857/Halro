package routegate

import (
	"context"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// PersistWindowThreshold is how long a window has to be before it is worth
// remembering across a restart.
//
// An availability window is thirty seconds and a restart is longer, so storing
// one buys nothing — it would have expired before the instance finished coming
// up. Quota reaches six hours, and re-admitting every exhausted account on
// restart is the exact tax this design removed. Five minutes sits between the
// two with room on both sides: above every availability window the default
// config can produce, below the opening window of the shortest reason-driven
// policy that has one.
const PersistWindowThreshold = 5 * time.Minute

// SuspensionStore is where the long refusals outlive the process.
//
// It is deliberately two calls and no read: the gate writes what it learns and
// reads the whole set once at open. Anything richer would be a second source of
// truth for state whose authority is the running gate.
type SuspensionStore interface {
	PutRouteSuspension(context.Context, domain.RouteSuspension) error
	DeleteRouteSuspension(ctx context.Context, kind, key string) error
}

// persistAction is one store write a locked section decided on and could not
// perform, because a bbolt commit under the admission mutex would put a disk
// write in front of every candidate resolution.
type persistAction struct {
	remove bool
	kind   string
	key    string
	record domain.RouteSuspension
}

// persistence is the gate's half of the store contract, separate from the
// admission mutex so a slow disk delays a write rather than every request.
type persistence struct {
	mu      sync.Mutex
	store   SuspensionStore
	onError func(error)
}

// UsePersistence attaches the store. A gate without one is fully functional and
// simply forgets its suspensions on restart, which is what every test and the
// offline tooling want.
func (g *Gate) UsePersistence(store SuspensionStore, onError func(error)) {
	g.persist.mu.Lock()
	defer g.persist.mu.Unlock()
	g.persist.store = store
	g.persist.onError = onError
}

// persistable reports whether this state has earned a row.
//
// Two conditions, and the first is the one that keeps availability out. A
// suspension with no canonical reason came from a 5xx run or a failed probe:
// both are re-learned within one interval of traffic, and both are keyed to a
// deployment the restart may not even have any more. What is left is the long
// refusals — a credential no clock will fix, and a window measured in hours.
func (s *scopeState) persistable() bool {
	if s.evidence.Reason == "" || !s.suspended() {
		return false
	}
	return s.indefinite || s.window > PersistWindowThreshold
}

// recordFor renders one state as the row that would restore it.
func recordFor(scope Scope, state *scopeState) domain.RouteSuspension {
	record := domain.RouteSuspension{
		ScopeKind:          string(scope.Kind),
		ScopeKey:           scope.Key,
		Reason:             string(state.evidence.Reason),
		ProviderStatus:     state.evidence.Status,
		ProviderCode:       state.evidence.Code,
		ObservedAt:         state.evidence.ObservedAt.UTC(),
		CredentialRevision: state.evidence.CredentialRevision,
		Indefinite:         state.indefinite,
		WindowSeconds:      int64(state.window / time.Second),
	}
	if !state.indefinite {
		record.Until = state.suspendedUntil.UTC()
	}
	return record
}

// notePersistLocked queues whatever this state now requires of the store, and
// is the only place scopeState.persisted moves.
//
// A delete is queued only for a state that was actually written. Without that
// check every passing probe and every retired deployment would open a bbolt
// transaction to remove a row that was never there — a disk write on the health
// loop for nothing.
func (g *Gate) notePersistLocked(scope Scope, state *scopeState) {
	if state.persistable() {
		state.persisted = true
		g.pending = append(g.pending, persistAction{
			kind: string(scope.Kind), key: scope.Key, record: recordFor(scope, state),
		})
		return
	}
	if state.persisted {
		state.persisted = false
		g.forgetPersistedLocked(scope)
	}
}

// notePersistIfMovedLocked is notePersistLocked for an observation that may
// have changed nothing.
//
// Concurrent attempts against one scope all report, and once the scope is
// suspended the second and third of them move only the observation timestamp.
// Writing the row again for each would put a bbolt commit on the request path
// for a suspension that already says what it will say — which is the cost this
// design promised not to pay. A row is rewritten only when what it restores
// moves: the suspension began, its window doubled, or it turned indefinite.
func (g *Gate) notePersistIfMovedLocked(
	scope Scope, state *scopeState, heldUntil time.Time, heldWindow time.Duration, heldIndefinite bool,
) {
	moved := state.indefinite != heldIndefinite ||
		state.window != heldWindow ||
		!state.suspendedUntil.Equal(heldUntil)
	if !moved && state.persisted {
		return
	}
	g.notePersistLocked(scope, state)
}

// forgetPersistedLocked queues the removal of a row for a scope leaving the map.
func (g *Gate) forgetPersistedLocked(scope Scope) {
	g.pending = append(g.pending, persistAction{
		remove: true, kind: string(scope.Kind), key: scope.Key,
	})
}

// dropLocked removes a scope from the map and queues the row's removal when
// there is one. Every deletion goes through here so no path can forget the
// store half.
func (g *Gate) dropLocked(scope Scope) {
	state := g.scopes[scope]
	if state == nil {
		return
	}
	delete(g.scopes, scope)
	if state.persisted {
		g.forgetPersistedLocked(scope)
	}
}

// flush performs the queued writes with the admission mutex released.
//
// Called through a deferred call registered before the one that unlocks, so it
// runs after it, holding no lock of its own on entry.
func (g *Gate) flush() {
	g.persist.mu.Lock()
	defer g.persist.mu.Unlock()
	g.drainLocked()
}

// drainLocked takes the queue and applies it, with the store lock already held.
//
// Taking the queue *inside* that lock is what keeps the writes in the order
// they were decided. Draining first and locking afterwards let two flushes swap
// places: one queues a suspension's row and is descheduled, another drops the
// scope and deletes, and the first then writes the row back — a suspension a
// success had ended, restored on the next start, and for a credential refusal
// restored indefinitely. Nothing would re-queue its removal, because the gate's
// own memory was already correct.
//
// A write failure is reported and dropped: the gate's state is right either
// way, and what is lost is that this suspension will not survive a restart.
func (g *Gate) drainLocked() {
	g.mu.Lock()
	actions := g.pending
	g.pending = nil
	g.mu.Unlock()
	if len(actions) == 0 || g.persist.store == nil {
		return
	}
	for _, action := range actions {
		var err error
		if action.remove {
			err = g.persist.store.DeleteRouteSuspension(context.Background(), action.kind, action.key)
		} else {
			err = g.persist.store.PutRouteSuspension(context.Background(), action.record)
		}
		if err != nil && g.persist.onError != nil {
			g.persist.onError(err)
		}
	}
}

// ClearDurable removes one suspension from the gate and from the store as one
// ordered step, with the durable half performed by the caller.
//
// The caller owns that half because it owes an audit record, and here a record
// commits with the change it describes. What it cannot own is the ordering: the
// gate writes rows of its own from failed attempts, and a clear that simply
// committed its delete and then wiped memory could be overtaken by one. A
// failure already queued lands after the delete, the row comes back, and the
// gate no longer remembers it — so the next start restores a suspension the
// operator has an audit record for having cleared.
//
// So the clear holds the store lock for the whole of it: everything already
// decided is written first, remove runs with no gate write able to interleave,
// and anything still queued for this scope is dropped with the scope itself. A
// fresh refusal afterwards suspends it again, which is the honest answer rather
// than a resurrected row.
func (g *Gate) ClearDurable(scope Scope, remove func() error) error {
	g.persist.mu.Lock()
	defer g.persist.mu.Unlock()
	g.drainLocked()
	if err := remove(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.scopes, scope)
	kept := g.pending[:0]
	for _, action := range g.pending {
		if action.kind == string(scope.Kind) && action.key == scope.Key {
			continue
		}
		kept = append(kept, action)
	}
	g.pending = kept
	return nil
}

// Restore loads the stored suspensions back into the gate.
//
// An expired window is restored as it was rather than being dropped: the gate
// already knows what to do with one — admit a probe through it — and
// special-casing expiry here would mean deciding on startup what the request
// path decides correctly anyway.
//
// A row whose reason no longer has a policy is discarded rather than kept. The
// gate would have nothing to end it with, so it would be a suspension only a
// human could clear, created by a version change nobody was told about.
func (g *Gate) Restore(suspensions []domain.RouteSuspension) {
	defer g.flush()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, stored := range suspensions {
		scope := Scope{Kind: ScopeKind(stored.ScopeKind), Key: stored.ScopeKey}
		policy, known := g.policies[provider.FailureReason(stored.Reason)]
		if !known {
			g.forgetPersistedLocked(scope)
			continue
		}
		state := &scopeState{
			failures:   policy.Threshold,
			policy:     policy,
			indefinite: stored.Indefinite,
			window:     time.Duration(stored.WindowSeconds) * time.Second,
			persisted:  true,
			evidence: Evidence{
				Reason:             provider.FailureReason(stored.Reason),
				Status:             stored.ProviderStatus,
				Code:               stored.ProviderCode,
				ObservedAt:         stored.ObservedAt,
				CredentialRevision: stored.CredentialRevision,
			},
		}
		if !stored.Indefinite {
			state.suspendedUntil = stored.Until
		}
		g.scopes[scope] = state
	}
}
