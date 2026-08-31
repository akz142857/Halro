package domain

import (
	"strings"
	"testing"
)

// A provider type is registered in two places that cannot see each other: the
// profile table, which the console's metadata endpoint enumerates, and the
// switch in ProviderInstance.Validate. Nothing held them together, so a type
// could be offered by the console and refused when its connection was validated.
//
// This is the domain half of the same defect the Admin write path had. That one
// was fixed by deriving the answer from the table; this switch stays a switch,
// because Validate reports several problems at once and reads better as one, so
// it gets a guard instead.
func TestEveryRegisteredProviderTypePassesInstanceValidation(t *testing.T) {
	for _, providerType := range AllProviderTypes() {
		defaults, ok := DefaultProviderProfile(providerType)
		if !ok {
			t.Errorf("%s has no default profile", providerType)
			continue
		}
		instance := ProviderInstance{
			ID: "prov_1", Name: "n", Type: providerType,
			BaseURL: "https://example.invalid", CredentialID: "cred_1",
			AccessSurface: defaults.AccessSurface, ProfileID: defaults.ProfileID,
			CredentialScheme: defaults.CredentialScheme,
			AllowedHosts:     []string{"example.invalid"},
			Capabilities:     DefaultProviderCapabilitiesForProfile(providerType, defaults.ProfileID),
			Enabled:          true,
		}
		// Azure is the one type that needs a member no other type has. Supplying
		// it keeps this test about the type switch rather than about Azure.
		if providerType == ProviderAzureOpenAI {
			instance.APIVersion = "2024-10-21"
		}
		if err := instance.Validate(); err != nil && strings.Contains(err.Error(), "provider type is not implemented") {
			t.Errorf("%s is in the profile table and refused by ProviderInstance.Validate", providerType)
		}
	}
}

// The default profile a type starts on has to be one an operator can actually
// reach. A withheld default would offer a type whose every write is refused.
func TestNoProviderTypeDefaultsToAWithheldProfile(t *testing.T) {
	for _, providerType := range AllProviderTypes() {
		defaults, ok := DefaultProviderProfile(providerType)
		if !ok {
			t.Errorf("%s has no default profile", providerType)
			continue
		}
		if IsWithheldProfile(defaults.ProfileID) {
			t.Errorf("%s defaults to %s, which this build withholds", providerType, defaults.ProfileID)
		}
	}
}
