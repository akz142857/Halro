package domain

import (
	"strings"
	"testing"
)

// The three Bedrock Mantle profiles carry a Beta capability ceiling, and until
// this check existed nothing enforced it: the Admin layer held the only list of
// immutable profiles and the Mantle ones were missing from it, so a stored
// binding could claim more than the profile supports and be believed by
// capability detection and the data-plane preflight.
func TestImmutableProfileBindingCannotExceedItsCeiling(t *testing.T) {
	cases := []struct {
		name    string
		profile ProviderProfileID
		widen   func(*ProviderCapabilities)
	}{
		{"mantle chat gains embeddings", ProfileBedrockMantleOpenAIChat, func(c *ProviderCapabilities) { c.Embeddings = true }},
		{"mantle responses gains reasoning", ProfileBedrockMantleOpenAIResponses, func(c *ProviderCapabilities) { c.Reasoning = true }},
		{"mantle messages gains json mode", ProfileBedrockMantleAnthropicMessages, func(c *ProviderCapabilities) { c.JSONMode = true }},
		{"mantle messages gains developer role", ProfileBedrockMantleAnthropicMessages, func(c *ProviderCapabilities) { c.DeveloperRole = true }},
		{"titan embed gains chat", ProfileBedrockInvokeTitanEmbedV2, func(c *ProviderCapabilities) { c.Chat = true }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			capabilities := DefaultProviderCapabilitiesForProfile(ProviderBedrock, test.profile)
			test.widen(&capabilities)
			binding := mantleBindingForTest(test.profile, capabilities)
			err := binding.Validate("prov_1", ProviderBedrock, false)
			if err == nil || !strings.Contains(err.Error(), "exceed the immutable operation profile") {
				t.Fatalf("widened %s binding was accepted: %v", test.profile, err)
			}
		})
	}
}

// Narrowing stays legal. The ceiling is a bound, not a fixed value: an operator
// who knows a deployment cannot do vision must still be able to say so.
func TestImmutableProfileBindingMayNarrowItsCeiling(t *testing.T) {
	profiles := []ProviderProfileID{
		ProfileBedrockMantleOpenAIChat,
		ProfileBedrockMantleOpenAIResponses,
		ProfileBedrockMantleAnthropicMessages,
	}
	for _, profile := range profiles {
		ceiling := DefaultProviderCapabilitiesForProfile(ProviderBedrock, profile)
		if err := mantleBindingForTest(profile, ceiling).Validate("prov_1", ProviderBedrock, false); err != nil {
			t.Fatalf("%s binding at its own ceiling was rejected: %v", profile, err)
		}
		narrowed := ceiling
		narrowed.Vision, narrowed.Tools = false, false
		if err := mantleBindingForTest(profile, narrowed).Validate("prov_1", ProviderBedrock, false); err != nil {
			t.Fatalf("%s binding narrower than its ceiling was rejected: %v", profile, err)
		}
	}
}

// A provider with no explicit bindings still declares capabilities through the
// legacy single-profile projection. That path has no binding to bound it, so
// the ceiling has to be checked against the instance directly.
func TestImmutableProfileLegacyProjectionCannotExceedItsCeiling(t *testing.T) {
	capabilities := DefaultProviderCapabilitiesForProfile(ProviderBedrock, ProfileBedrockMantleAnthropicMessages)
	capabilities.Embeddings = true
	instance := ProviderInstance{
		ID: "prov_1", Name: "mantle", Type: ProviderBedrock,
		BaseURL: "https://bedrock-mantle.us-east-1.api.aws", CredentialID: "cred_1",
		AccessSurface: SurfaceBedrockMantle, ProfileID: ProfileBedrockMantleAnthropicMessages,
		CredentialScheme: CredentialBedrockAPIKey, AllowedHosts: []string{"bedrock-mantle.us-east-1.api.aws"},
		Capabilities: capabilities, Enabled: true,
	}
	instance.CapabilityEvidence = EvidenceForCapabilities(capabilities, EvidenceDeclared)
	err := instance.Validate()
	if err == nil || !strings.Contains(err.Error(), "exceed the immutable operation profile") {
		t.Fatalf("widened legacy projection was accepted: %v", err)
	}
}

// The list is one spelling shared by domain, Admin and the loader. If a Mantle
// profile drops out of it the ceiling silently stops being enforced everywhere
// at once, which is the failure this whole test file exists for.
func TestMantleProfilesAreImmutableCapabilityProfiles(t *testing.T) {
	for _, profile := range []ProviderProfileID{
		ProfileBedrockMantleOpenAIChat,
		ProfileBedrockMantleOpenAIResponses,
		ProfileBedrockMantleAnthropicMessages,
	} {
		if !IsImmutableCapabilityProfile(profile) {
			t.Fatalf("%s is not treated as an immutable capability profile", profile)
		}
	}
	if IsImmutableCapabilityProfile(ProfileBedrockConverseText) {
		t.Fatal("Converse text was frozen; it is an operator-declared profile")
	}
}

// Not being an immutable profile means the operator chooses within a ceiling,
// not that there is no ceiling. Bounding only the immutable profiles left the
// bindings array as the way around it: PutProvider bounds the capabilities
// field against the profile, then replaces that field with the summary of these
// bindings, so a set that never met a ceiling became the connection's
// declaration. Two of the profiles below make that reach the wire — DeepSeek and
// openai-compatible have no per-field rejection behind the capability, so a
// capability declared here is a field really sent upstream.
func TestOperatorDeclaredProfileBindingCannotExceedItsCeiling(t *testing.T) {
	for _, test := range []struct {
		name    string
		profile ProviderProfileID
		widen   func(*ProviderCapabilities)
	}{
		{"converse text gains tools", ProfileBedrockConverseText, func(c *ProviderCapabilities) { c.Tools = true }},
		{"converse text gains vision", ProfileBedrockConverseText, func(c *ProviderCapabilities) { c.Vision = true }},
		{"gemini gains tools", ProfileGeminiText, func(c *ProviderCapabilities) { c.Tools = true }},
		{"deepseek gains vision", ProfileDeepSeekChat, func(c *ProviderCapabilities) { c.Vision = true }},
		{"openai compatible gains tools", ProfileOpenAICompatible, func(c *ProviderCapabilities) { c.Tools = true }},
		{"anthropic messages gains embeddings", ProfileAnthropicMessages, func(c *ProviderCapabilities) { c.Embeddings = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerType, defaults, ok := RegisteredProviderProfile(test.profile)
			if !ok {
				t.Fatalf("%s is not a registered profile", test.profile)
			}
			capabilities := DefaultProviderCapabilitiesForProfile(providerType, test.profile)
			test.widen(&capabilities)
			err := bindingForTest(defaults, capabilities).Validate("prov_1", providerType, false)
			if err == nil || !strings.Contains(err.Error(), "exceed what this profile can serve") {
				t.Fatalf("widened %s binding was accepted: %v", test.profile, err)
			}
		})
	}
}

// The bound is MaxProviderCapabilitiesForProfile, not the defaults, and the gap
// between them is load-bearing. provider_executed_tools is off in a new
// Anthropic connection and the operator may turn it on, because enabling it
// means accepting upstream egress that never passes through SafeTransport.
// Bounding bindings by the defaults would make that opt-in unreachable — the
// ceiling would have closed a door the design deliberately left open.
func TestAnthropicBindingMayOptIntoProviderExecutedTools(t *testing.T) {
	providerType, defaults, ok := RegisteredProviderProfile(ProfileAnthropicMessages)
	if !ok {
		t.Fatal("the Anthropic Messages profile is not registered")
	}
	capabilities := DefaultProviderCapabilitiesForProfile(providerType, ProfileAnthropicMessages)
	if capabilities.ProviderExecutedTools {
		t.Fatal("provider-executed tools is meant to be off until the operator opts in")
	}
	capabilities.ProviderExecutedTools = true
	if err := bindingForTest(defaults, capabilities).Validate("prov_1", providerType, false); err != nil {
		t.Fatalf("opting into provider-executed tools was rejected: %v", err)
	}
}

func bindingForTest(defaults ProviderProfileDefaults, capabilities ProviderCapabilities) ProviderProfileBinding {
	return ProviderProfileBinding{
		ID: DefaultProviderProfileBindingID("prov_1", defaults.ProfileID), ProviderID: "prov_1",
		ProfileID: defaults.ProfileID, AccessSurface: defaults.AccessSurface,
		CredentialScheme: defaults.CredentialScheme, Capabilities: capabilities,
		CapabilityEvidence: EvidenceForCapabilities(capabilities, EvidenceDeclared), Enabled: true,
	}
}

func mantleBindingForTest(profile ProviderProfileID, capabilities ProviderCapabilities) ProviderProfileBinding {
	surface := SurfaceBedrockMantle
	if !strings.Contains(string(profile), "mantle") {
		_, defaults, _ := RegisteredProviderProfile(profile)
		surface = defaults.AccessSurface
	}
	scheme := CredentialBedrockAPIKey
	if surface != SurfaceBedrockMantle {
		scheme = CredentialAWSSigV4Explicit
	}
	return ProviderProfileBinding{
		ID: DefaultProviderProfileBindingID("prov_1", profile), ProviderID: "prov_1", ProfileID: profile,
		AccessSurface: surface, CredentialScheme: scheme, Capabilities: capabilities,
		CapabilityEvidence: EvidenceForCapabilities(capabilities, EvidenceDeclared), Enabled: true,
	}
}
