package gateway

import (
	"testing"

	anthropicwire "github.com/akz142857/Halro/internal/compatibility/anthropic"
	"github.com/akz142857/Halro/internal/domain"
)

// Native Anthropic mode is spread across three lists that cannot see each other:
// the schema registry that validates and forwards the caller's bytes
// (compatibility/anthropic), the gateway's own (profile, surface) pairing that
// decides whether native is even offered, and the beta-header allowlist in
// domain.
//
// Two of them must agree exactly, and the third is deliberately a subset. None
// of that was held anywhere, so a platform could be routed into native mode with
// no schema to validate it — a byte-for-byte forward with no inspection, which
// is the opposite of what native mode is allowed to be.
func TestNativeAnthropicListsAgree(t *testing.T) {
	registry, err := anthropicwire.NewNativeSchemaRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range domain.AllProviderProfiles() {
		_, hasSchema := registry.Schema(profile.ID, 1)
		offersNative := isNativeAnthropicProfile(profile.ID, profile.AccessSurface)
		switch {
		case offersNative && !hasSchema:
			// The dangerous direction: the gateway would forward the caller's
			// bytes with nothing registered to validate them.
			t.Errorf("%s is offered as native and has no native schema to validate its payloads", profile.ID)
		case hasSchema && !offersNative:
			t.Errorf("%s registers a native schema the gateway will never select", profile.ID)
		}
		// Betas are a subset on purpose: a profile may serve native and still be
		// unable to carry a beta header, which is MiniMax's case. The subset
		// direction is what has to hold.
		if domain.ProfileSendsAnthropicBetas(profile.ID) && !offersNative {
			t.Errorf("%s may send anthropic-beta headers and is not a native profile", profile.ID)
		}
	}
}

// A native profile that is withheld would be unreachable by every write path and
// still carry the machinery, which is a claim nobody can act on.
func TestNoNativeProfileIsWithheld(t *testing.T) {
	for _, profile := range domain.AllProviderProfiles() {
		if isNativeAnthropicProfile(profile.ID, profile.AccessSurface) && profile.Withheld {
			t.Errorf("%s is offered as native and withheld from every write path", profile.ID)
		}
	}
}
