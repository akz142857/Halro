// Package routegate decides whether a route target may be used right now.
//
// It replaces three separate answers to that question. The registry filtered on
// an active probe keyed by deployment; the gateway held a circuit breaker keyed
// by target; and nothing at all covered an upstream that is healthy but will not
// serve this credential — out of quota, subscription lapsed, key revoked. The
// first two classified by the symptom Halro observed, so neither had anywhere to
// put the third, and the gap showed up as an exhausted upstream keeping its
// place at the front of the priority order and collecting a failed attempt from
// every request forever.
//
// Two ideas carry the replacement.
//
// The first is that a refusal declares its own scope. "This deployment is
// throwing 500s" and "this key is no longer valid" are facts about different
// things, and one credential commonly backs several deployments — so a refusal
// learned from one of them is knowledge about all of them, and rediscovering it
// per deployment is waste an operator reads as repeated upstream errors. A
// target is admissible only when none of the scopes it belongs to is suspended.
//
// The second is that suspensions expire on evidence rather than on hope. A
// window that doubles covers the transient cases; a credential refusal does not
// expire on a clock at all, because a dead key does not heal, and instead clears
// when the operator advances the credential's revision — so fixing it *is* the
// un-suspend, with no second step to remember.
package routegate

import (
	"fmt"

	"github.com/akz142857/Halro/internal/provider"
)

// ScopeKind is the kind of thing a refusal was about.
//
// Ordered narrow to wide in the sense that matters for blast radius: suspending
// a deployment removes one route's upstream, suspending a provider removes every
// deployment behind one endpoint. Getting a scope wrong in the narrow direction
// costs a few rediscoveries; getting it wrong in the wide direction takes out
// healthy routes. That asymmetry is why a reason widens only on evidence.
type ScopeKind string

const (
	// ScopeDeployment is one upstream model on one connection.
	ScopeDeployment ScopeKind = "deployment"
	// ScopeCredentialModel is one model as reached with one credential. Quota
	// and rate limits are metered here by most upstreams, and it is the narrow
	// reading of both — a credential-wide ceiling looks the same from here until
	// evidence says otherwise, and reading it narrowly costs a rediscovery per
	// model where reading it widely would strand models that still work.
	ScopeCredentialModel ScopeKind = "credential_model"
	// ScopeCredential is the secret itself: whether the account behind it may be
	// used at all.
	ScopeCredential ScopeKind = "credential"
	// ScopeProvider is one endpoint. Reserved for failures that never got an
	// answer out of it, since anything the upstream actually answered is
	// evidence about the thing that answered, not about the endpoint.
	ScopeProvider ScopeKind = "provider"
)

// Scope identifies one suspendable thing.
type Scope struct {
	Kind ScopeKind
	Key  string
}

func (s Scope) String() string { return string(s.Kind) + ":" + s.Key }

// scopesOf lists every scope a target belongs to, so admission can check all of
// them. A target missing an identity simply has no scope of that kind rather
// than one keyed by the empty string, which would pool unrelated targets
// together under a single suspension.
func scopesOf(target provider.Target) []Scope {
	scopes := make([]Scope, 0, 4)
	if target.DeploymentID != "" {
		scopes = append(scopes, Scope{Kind: ScopeDeployment, Key: target.DeploymentID})
	}
	if target.CredentialID != "" {
		if target.ProviderModel != "" {
			scopes = append(scopes, Scope{
				Kind: ScopeCredentialModel,
				Key:  credentialModelKey(target.CredentialID, target.ProviderModel),
			})
		}
		scopes = append(scopes, Scope{Kind: ScopeCredential, Key: target.CredentialID})
	}
	if target.ProviderID != "" {
		scopes = append(scopes, Scope{Kind: ScopeProvider, Key: target.ProviderID})
	}
	return scopes
}

// credentialModelKey is built with a separator that cannot appear in either
// half's identifier, so two different pairs cannot collide into one suspension.
func credentialModelKey(credentialID, providerModel string) string {
	return fmt.Sprintf("%s\x00%s", credentialID, providerModel)
}
