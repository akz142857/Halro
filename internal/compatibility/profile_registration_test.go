package compatibility

import (
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// Adding a platform means registering it in several packages, and the cost of
// that is not the number of places — it is that forgetting one is silent.
//
// This layer was the silent one. A profile in the domain table with no field
// rules of its own falls back to the legacy set, which is fail-closed and
// therefore looks like it works: the platform serves plain text and refuses
// tools, images and structured output, with no refusal that names the omission
// and no test that fails. The other layers already speak up — the profile
// manifest is required by TestCeilingWithinProfileManifestOperations, and
// endpoint coverage by EndpointCompatibilityManifest.Validate — so this closes
// the last one.
//
// It walks the domain table rather than a list of its own, because a private
// list cannot be told when a profile is added.
func TestEveryProfileRegistersItsOwnFieldRules(t *testing.T) {
	registered := RegisteredGenerateProfiles()
	for _, profile := range domain.AllProviderProfiles() {
		if slices.Contains(registered, profile.ID) {
			continue
		}
		// An embeddings-only profile has no generate surface to declare anything
		// about; requiring an entry would be requiring an empty one.
		if !profile.Ceiling.Chat {
			continue
		}
		t.Errorf("%s serves chat but declares no generate field rules; register it in provider_fields.go", profile.ID)
	}

	// And nothing registered that the table no longer carries: a rule kept after
	// its profile was removed is a rule that will silently adopt the next profile
	// to reuse the identifier.
	for _, profileID := range registered {
		if _, _, ok := domain.RegisteredProviderProfile(profileID); !ok {
			t.Errorf("provider_fields.go registers %s, which the profile table does not carry", profileID)
		}
	}
}

// The published manifest is the other half of the same registration, and it is
// asserted from the profile's side here rather than only from the endpoint's.
// Validate refuses a manifest that omits a profile; this refuses a profile that
// appears in no manifest at all, which Validate cannot see.
func TestEveryChatProfileAppearsInAnEndpointManifest(t *testing.T) {
	covered := map[domain.ProviderProfileID]struct{}{}
	for _, manifest := range BuiltinEndpointManifests() {
		for _, profileID := range manifest.ProviderProfiles {
			covered[profileID] = struct{}{}
		}
	}
	for _, profile := range domain.AllProviderProfiles() {
		if _, ok := covered[profile.ID]; ok {
			continue
		}
		t.Errorf("%s is in the profile table and reachable through no endpoint manifest", profile.ID)
	}
}
