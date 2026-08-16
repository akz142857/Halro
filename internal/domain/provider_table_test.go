package domain

import (
	"testing"
)

// Defaults must sit inside the ceiling, or a connection is born already claiming
// more than an operator would be allowed to turn on — and the Admin layer would
// refuse to save a form it had just filled in.
func TestProfileDefaultsWithinCeiling(t *testing.T) {
	for _, profile := range AllProviderProfiles() {
		if !ProviderCapabilitiesSubset(profile.Defaults, profile.Ceiling) {
			t.Errorf("%s defaults exceed its ceiling:\n defaults=%#v\n ceiling=%#v",
				profile.ID, profile.Defaults, profile.Ceiling)
		}
	}
}

// The type-level defaults may be narrower than the profile's — Anthropic's are,
// because files and batches ride with the profile rather than the type — but
// never wider. A type-level set that claimed more than the profile it starts on
// would hand bootstrap and the store's fill-in a connection the profile cannot
// serve.
func TestTypeDefaultsWithinDefaultProfile(t *testing.T) {
	for _, row := range providerTypeTable {
		profile, ok := profileIndex[row.DefaultProfile]
		if !ok {
			t.Fatalf("%s names a default profile that is not in the table: %s", row.Type, row.DefaultProfile)
		}
		if !ProviderCapabilitiesSubset(row.LegacyDefaults, profile.Ceiling) {
			t.Errorf("%s type defaults exceed the ceiling of %s:\n type=%#v\n ceiling=%#v",
				row.Type, profile.ID, row.LegacyDefaults, profile.Ceiling)
		}
	}
}

// Every row must be reachable through each of the three lookups that read the
// table, and the identity they report must be the row's own. A row that no
// lookup can reach is a profile that exists in the matrix and nowhere else.
func TestEveryProfileRowResolves(t *testing.T) {
	for _, profile := range AllProviderProfiles() {
		registeredType, defaults, ok := RegisteredProviderProfile(profile.ID)
		if !ok {
			t.Errorf("%s is not resolvable by ID", profile.ID)
			continue
		}
		if registeredType != profile.Type || defaults.AccessSurface != profile.AccessSurface ||
			defaults.CredentialScheme != profile.CredentialScheme || defaults.ProfileID != profile.ID {
			t.Errorf("%s resolved to a different identity: %#v (type %s)", profile.ID, defaults, registeredType)
		}
		if resolved, ok := ResolveProviderProfile(profile.Type, profile.ID); !ok || resolved.ProfileID != profile.ID {
			t.Errorf("%s did not resolve for its own type: %#v ok=%v", profile.ID, resolved, ok)
		}
		if _, ok := ResolveCredentialProfile(profile.Type, profile.AccessSurface, profile.CredentialScheme); !ok {
			t.Errorf("%s has no credential profile for its own surface and scheme", profile.ID)
		}
	}
}

// Several profiles share one (type, surface, scheme): OpenAI chat and media, the
// four Bedrock runtime profiles, the three Mantle profiles. A stored credential
// carries only surface and scheme, so resolution has to be deterministic and has
// to land on the primary — table order, not map order.
func TestCredentialResolutionPrefersThePrimaryProfile(t *testing.T) {
	cases := []struct {
		providerType ProviderType
		surface      AccessSurface
		scheme       CredentialScheme
		want         ProviderProfileID
	}{
		{ProviderOpenAI, SurfaceOpenAI, CredentialBearerStatic, ProfileOpenAIChatEmbeddings},
		{ProviderBedrock, SurfaceBedrockRuntime, CredentialAWSSigV4Explicit, ProfileBedrockConverseText},
		{ProviderBedrock, SurfaceBedrockMantle, CredentialBedrockAPIKey, ProfileBedrockMantleOpenAIChat},
		{ProviderBedrock, SurfaceBedrockAgentRuntime, CredentialAWSSigV4Explicit, ProfileBedrockAgentRerankCohere35},
	}
	for _, test := range cases {
		for attempt := 0; attempt < 8; attempt++ {
			resolved, ok := ResolveCredentialProfile(test.providerType, test.surface, test.scheme)
			if !ok || resolved.ProfileID != test.want {
				t.Fatalf("%s/%s resolved to %q, want %q (ok=%v)",
					test.surface, test.scheme, resolved.ProfileID, test.want, ok)
			}
		}
	}
}

// A default profile is what a connection starts on, so it must be a real row and
// must belong to the type that names it.
func TestDefaultProfileBelongsToItsType(t *testing.T) {
	for _, providerType := range AllProviderTypes() {
		defaults, ok := DefaultProviderProfile(providerType)
		if !ok {
			t.Errorf("%s has no default profile", providerType)
			continue
		}
		registeredType, _, ok := RegisteredProviderProfile(defaults.ProfileID)
		if !ok || registeredType != providerType {
			t.Errorf("%s defaults to %q, which belongs to %q", providerType, defaults.ProfileID, registeredType)
		}
	}
}

// An unregistered or empty profile ID falls back to the type. Callers that have
// not chosen a profile yet depend on it, and the fallback has to stay inside
// what the type's own default profile can serve.
func TestUnknownProfileFallsBackToType(t *testing.T) {
	for _, providerType := range AllProviderTypes() {
		byType := DefaultProviderCapabilities(providerType)
		if got := DefaultProviderCapabilitiesForProfile(providerType, ""); got != byType {
			t.Errorf("%s: empty profile did not fall back to the type: %#v", providerType, got)
		}
		if got := MaxProviderCapabilitiesForProfile(providerType, "no.such.profile.v1"); got != byType {
			t.Errorf("%s: unknown profile did not fall back to the type: %#v", providerType, got)
		}
	}
	if DefaultProviderCapabilities("no-such-type").AnyOperation() {
		t.Error("an unknown provider type produced capabilities")
	}
}

// The immutable flag decides which refusal an operator sees and, before that,
// whether narrowing is the only edit allowed. It is stated per row now, so this
// pins the set rather than leaving it to whoever edits the table next.
func TestImmutableProfileSet(t *testing.T) {
	want := map[ProviderProfileID]bool{
		ProfileOpenAIMediaResources:           true,
		ProfileBedrockInvokeTitanEmbedV2:      true,
		ProfileBedrockInvokeTitanImageV2:      true,
		ProfileBedrockAgentRerankCohere35:     true,
		ProfileBedrockAsyncNovaReel:           true,
		ProfileBedrockMantleOpenAIChat:        true,
		ProfileBedrockMantleOpenAIResponses:   true,
		ProfileBedrockMantleAnthropicMessages: true,
	}
	for _, profile := range AllProviderProfiles() {
		if profile.Immutable != want[profile.ID] {
			t.Errorf("%s immutable=%v, want %v", profile.ID, profile.Immutable, want[profile.ID])
		}
	}
	if !IsImmutableCapabilityProfile(ProfileOpenAIMediaResources) ||
		IsImmutableCapabilityProfile(ProfileOpenAIChatEmbeddings) ||
		IsImmutableCapabilityProfile("no.such.profile.v1") {
		t.Error("IsImmutableCapabilityProfile disagrees with the table")
	}
}

// Only the Anthropic Messages profile may be widened past its defaults, and only
// by provider-executed tools. Enabling that accepts upstream egress which never
// passes through SafeTransport, so it must stay an opt-in on one profile rather
// than drift into being every profile's ceiling.
func TestOnlyAnthropicMessagesHasAWiderCeiling(t *testing.T) {
	for _, profile := range AllProviderProfiles() {
		widened := profile.Defaults != profile.Ceiling
		if profile.ID == ProfileAnthropicMessages {
			if !widened || !profile.Ceiling.ProviderExecutedTools || profile.Defaults.ProviderExecutedTools {
				t.Errorf("anthropic messages ceiling is not provider-executed-tools opt-in: %#v", profile)
			}
			continue
		}
		if widened {
			t.Errorf("%s ceiling differs from its defaults:\n defaults=%#v\n ceiling=%#v",
				profile.ID, profile.Defaults, profile.Ceiling)
		}
		if profile.Ceiling.ProviderExecutedTools {
			t.Errorf("%s allows provider-executed tools", profile.ID)
		}
	}
}

// The dependency list handed to callers has to be the rule that is actually
// enforced, or a form built from it offers combinations the save will refuse.
func TestCapabilityRequiresChatMatchesValidation(t *testing.T) {
	for _, name := range CapabilityRequiresChat() {
		capabilities := ProviderCapabilities{}
		if !setCapabilityForTest(&capabilities, name) {
			t.Fatalf("%q is not a capability name", name)
		}
		if err := (ProviderInstance{
			ID: "prov_1", Name: "n", Type: ProviderOpenAI,
			BaseURL: "https://api.openai.com", CredentialID: "cred_1",
			Capabilities: capabilities,
		}).Validate(); err == nil {
			t.Errorf("%q was accepted without chat", name)
		}
	}
}

func setCapabilityForTest(c *ProviderCapabilities, name string) bool {
	switch name {
	case "streaming":
		c.Streaming = true
	case "tools":
		c.Tools = true
	case "vision":
		c.Vision = true
	case "json_mode":
		c.JSONMode = true
	case "developer_role":
		c.DeveloperRole = true
	case "reasoning":
		c.Reasoning = true
	case "stream_usage":
		c.StreamUsage = true
	case "provider_executed_tools":
		c.ProviderExecutedTools = true
	default:
		return false
	}
	return true
}
