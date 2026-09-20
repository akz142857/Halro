package routegate

import (
	"time"

	"github.com/akz142857/Halro/internal/provider"
)

// RefusalKind is what an operator or a caller is told about an alias that has no
// candidates left, bucketed so it can go in a response and a metric.
//
// The bucket matters more than the reason. "Out of quota" and "this key is dead"
// send an operator to the billing page and the credential store respectively,
// and both send them somewhere entirely different from "the upstream is having
// a bad minute" — while a caller needs only to know whether waiting will help.
type RefusalKind string

const (
	// RefusalQuotaExhausted is an upstream that answered and said no more. Time
	// will not necessarily fix it; somebody topping up an account will.
	RefusalQuotaExhausted RefusalKind = "quota_exhausted"
	// RefusalCredentialUnusable is a secret the upstream will not accept.
	// Nothing but an operator replacing it will change that, which is why it
	// carries no retry hint at all.
	RefusalCredentialUnusable RefusalKind = "credential_unusable"
	// RefusalUnavailable is the ordinary case: something is not answering well,
	// and the window will end in a probe.
	RefusalUnavailable RefusalKind = "unavailable"
)

func refusalKindFor(reason provider.FailureReason) RefusalKind {
	switch reason {
	case provider.FailureReasonSubscriptionQuotaExhausted:
		return RefusalQuotaExhausted
	case provider.FailureReasonInvalidCredential, provider.FailureReasonSubscriptionInactive:
		return RefusalCredentialUnusable
	default:
		return RefusalUnavailable
	}
}

// severity orders the kinds by how much they change what somebody does next. An
// alias that is down for two reasons should say the one that will not fix
// itself, because reporting the transient half invites a retry that cannot work
// and leaves the credential unmentioned.
func (k RefusalKind) severity() int {
	switch k {
	case RefusalCredentialUnusable:
		return 3
	case RefusalQuotaExhausted:
		return 2
	default:
		return 1
	}
}

// Refusal explains an alias with nothing left to try.
type Refusal struct {
	Kind RefusalKind
	// RetryAfter is how long until the earliest suspension could admit a probe,
	// and zero when none of them will end on their own. Zero is the honest
	// answer for a dead credential: telling a caller to come back in an hour
	// would be inventing a recovery nobody has scheduled.
	RetryAfter time.Duration
}

// Explain says why every candidate for an alias is out.
//
// It takes the alias's whole target list, not the filtered one, because by the
// time anyone asks there is nothing left to inspect. The answer is deliberately
// free of identity — it reaches a Gateway caller across a trust boundary, and
// which credential ran dry is not theirs to learn. The console and the Admin
// API read Snapshot for that.
func (g *Gate) Explain(targets []provider.Target, now time.Time) (Refusal, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	refusal, found := Refusal{}, false
	for _, target := range targets {
		for _, scope := range scopesOf(target) {
			state := g.scopes[scope]
			if state == nil || !state.suspended() || staleLocked(scope, state, target) {
				continue
			}
			kind := refusalKindFor(state.evidence.Reason)
			wait := time.Duration(0)
			if !state.indefinite && state.suspendedUntil.After(now) {
				wait = state.suspendedUntil.Sub(now)
			}
			switch {
			case !found, kind.severity() > refusal.Kind.severity():
				refusal, found = Refusal{Kind: kind, RetryAfter: wait}, true
			case kind.severity() == refusal.Kind.severity():
				// Same kind from several scopes: the soonest one is the one a
				// caller can act on, and a zero means one of them never ends.
				if refusal.RetryAfter != 0 && (wait == 0 || wait < refusal.RetryAfter) {
					refusal.RetryAfter = wait
				}
			}
		}
	}
	return refusal, found
}
