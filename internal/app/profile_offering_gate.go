package app

import (
	"fmt"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
)

// offeredProfile is the one answer to "may anything be created on this profile
// here", and it composes two refusals that are not the same kind of fact.
//
//   - Withheld is the build's answer. Evidence has not been gathered, so the
//     implementation exists and nothing may be created on it. No configuration
//     changes that, because no operator is in a position to supply the evidence.
//   - SubscriptionGated is the operator's answer. The upstream reserves the
//     credential for its own clients, so whether this instance may hold one is a
//     statement only the person whose subscription it is can make. Off in the
//     shipped default, which is what keeps the distributed artifact compliant
//     without the operator needing to know the question exists.
//
// Both are write gates, never read gates: an instance that turns a product off
// while a connection on it exists must still start, or the operator cannot
// delete that connection. Callers put this in front of creation and of the
// served matrix, and never in front of a load.
func offeredProfile(subscriptions config.ProviderSubscriptions, id domain.ProviderProfileID) bool {
	if domain.IsWithheldProfile(id) {
		return false
	}
	if !domain.IsSubscriptionGatedProfile(id) {
		return true
	}
	return subscriptionEnabled(subscriptions, id)
}

// subscriptionEnabled reads the operator's switch for a gated profile's product.
//
// The default arm fails closed on purpose: a gated row whose switch nobody wired
// is not offered, so adding a row without adding its member cannot quietly ship
// the product enabled. The reverse mistake — a member with no row — is refused
// in config.go, where the type says why.
func subscriptionEnabled(subscriptions config.ProviderSubscriptions, id domain.ProviderProfileID) bool {
	identity, ok := domain.IdentityForProfile(id)
	if !ok {
		return false
	}
	switch identity.Offering {
	case domain.OfferingAnthropicClaudeSubscription:
		return subscriptions.AnthropicClaude
	default:
		return false
	}
}

// subscriptionDisabledError is the refusal an operator meets when the product is
// implemented and offered by this build but switched off here. It carries a code
// so the console can say which switch to set, in the reader's own language,
// rather than printing an English sentence about a YAML member.
func subscriptionDisabledError(surface domain.AccessSurface) error {
	return claudeCredentialProductError{
		code: "provider_subscription_disabled",
		err: fmt.Errorf(
			"the %q product is off in this instance's configuration: set "+
				"provider_subscriptions.anthropic_claude to enable it, and read what "+
				"that means for the upstream's terms before you do",
			surface,
		),
	}
}
