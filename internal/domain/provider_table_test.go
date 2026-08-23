package domain

import (
	"slices"
	"strings"
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
// four Bedrock runtime profiles, the five Mantle profiles. A stored credential
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
		// The default /v1 route is the primary: it carries 38 of the 50 models the
		// account lists, and it is the one reached without a path prefix.
		{ProviderBedrock, SurfaceBedrockMantle, CredentialBedrockAPIKey, ProfileBedrockMantleChat},
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
		ProfileBedrockMantleChat:              true,
		ProfileBedrockMantleOpenAIChat:        true,
		ProfileBedrockMantleResponses:         true,
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

// A ceiling wider than its defaults is an opt-in an operator can reach, so each
// one is named here and nothing else may have a gap at all. Two exist, for two
// different reasons: provider-executed tools accept upstream egress that never
// passes through SafeTransport, and DeepSeek serves images on one model while
// answering every other with a 400. Both stay opt-ins on one profile rather than
// drifting into being every profile's ceiling — and provider-executed tools in
// particular must never appear in a second ceiling, whatever else is added here.
func TestOnlyNamedProfilesHaveAWiderCeiling(t *testing.T) {
	optIn := map[ProviderProfileID]func(ProviderCapabilities) ProviderCapabilities{
		ProfileAnthropicMessages: withProviderExecutedTools,
		ProfileDeepSeekChat:      withVision,
	}
	for _, profile := range AllProviderProfiles() {
		widen, named := optIn[profile.ID]
		if !named {
			if profile.Defaults != profile.Ceiling {
				t.Errorf("%s ceiling differs from its defaults:\n defaults=%#v\n ceiling=%#v",
					profile.ID, profile.Defaults, profile.Ceiling)
			}
			if profile.Ceiling.ProviderExecutedTools {
				t.Errorf("%s allows provider-executed tools", profile.ID)
			}
			continue
		}
		// The gap is exactly the declared opt-in — not merely non-empty, which
		// would let a second capability ride in beside the one that was reviewed.
		if profile.Defaults == profile.Ceiling || widen(profile.Defaults) != profile.Ceiling {
			t.Errorf("%s ceiling is not the opt-in it is named for:\n defaults=%#v\n ceiling=%#v",
				profile.ID, profile.Defaults, profile.Ceiling)
		}
		if profile.ID != ProfileAnthropicMessages && profile.Ceiling.ProviderExecutedTools {
			t.Errorf("%s allows provider-executed tools", profile.ID)
		}
	}
}

// The dependencies handed to callers have to be the rule that is actually
// enforced, or a form built from them offers combinations the save will refuse.
//
// Enforcement is split across two boundaries and the published contract has to
// cover both: ProviderInstance.Validate refuses streaming without chat, and
// modelcatalog.ValidateDependencies — which every deployment crosses — refuses
// the rest, including stream usage without streaming. Naming only one of them is
// how the published list came to be missing that link.
func TestCapabilityDependenciesAreEnforcedSomewhere(t *testing.T) {
	dependencies := CapabilityDependencies()
	if got := dependencies["stream_usage"]; len(got) != 1 || got[0] != "streaming" {
		t.Errorf("stream_usage depends on %v, want streaming — the deployment boundary enforces it", got)
	}
	for name, needs := range dependencies {
		if !slices.Contains(CapabilityNames(), name) {
			t.Errorf("%q has dependencies but is not a capability", name)
		}
		for _, need := range needs {
			if !slices.Contains(CapabilityNames(), need) {
				t.Errorf("%q depends on %q, which is not a capability", name, need)
			}
		}
	}
	// Every dependency has to bottom out, or a form applying them loops.
	for name := range dependencies {
		seen := map[string]bool{name: true}
		queue := append([]string(nil), dependencies[name]...)
		for len(queue) > 0 {
			next := queue[0]
			queue = queue[1:]
			if seen[next] {
				t.Fatalf("%q depends on itself through %q", name, next)
			}
			seen[next] = true
			queue = append(queue, dependencies[next]...)
		}
	}
	// The half this package enforces, checked against the real write boundary.
	for _, name := range []string{"streaming"} {
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

// Every profile's prefilled endpoint, pinned. A wrong prefill sends a credential
// to the wrong host, and it is the field an operator is least likely to re-read.
//
// Azure OpenAI and the compatibility type deliberately offer nothing. They used
// to inherit api.openai.com from the console, which no deployment of either ever
// wants: an Azure endpoint is the resource's own host, and a compatibility
// server is by definition somewhere Halro does not know about.
func TestResolvedEndpointsMatchWhatTheConsoleOffered(t *testing.T) {
	const region = "us-east-1"
	want := map[ProviderProfileID]string{
		ProfileOpenAIChatEmbeddings:           "https://api.openai.com",
		ProfileOpenAIMediaResources:           "https://api.openai.com",
		ProfileAzureChatEmbeddings:            "",
		ProfileOpenAICompatible:               "",
		ProfileAnthropicMessages:              "https://api.anthropic.com",
		ProfileDeepSeekChat:                   "https://api.deepseek.com",
		ProfileGeminiText:                     "https://generativelanguage.googleapis.com",
		ProfileBedrockConverseText:            "https://bedrock-runtime.us-east-1.amazonaws.com",
		ProfileBedrockInvokeTitanEmbedV2:      "https://bedrock-runtime.us-east-1.amazonaws.com",
		ProfileBedrockInvokeTitanImageV2:      "https://bedrock-runtime.us-east-1.amazonaws.com",
		ProfileBedrockAsyncNovaReel:           "https://bedrock-runtime.us-east-1.amazonaws.com",
		ProfileBedrockAgentRerankCohere35:     "https://bedrock-agent-runtime.us-east-1.amazonaws.com",
		ProfileBedrockMantleChat:              "https://bedrock-mantle.us-east-1.api.aws",
		ProfileBedrockMantleOpenAIChat:        "https://bedrock-mantle.us-east-1.api.aws",
		ProfileBedrockMantleResponses:         "https://bedrock-mantle.us-east-1.api.aws",
		ProfileBedrockMantleOpenAIResponses:   "https://bedrock-mantle.us-east-1.api.aws",
		ProfileBedrockMantleAnthropicMessages: "https://bedrock-mantle.us-east-1.api.aws",
	}
	for _, profile := range AllProviderProfiles() {
		expected, listed := want[profile.ID]
		if !listed {
			t.Errorf("%s has no expected endpoint; add it here when adding the row", profile.ID)
			continue
		}
		if got := ResolveBaseURL(profile.ID, region); got != expected {
			t.Errorf("%s endpoint is %q, want %q", profile.ID, got, expected)
		}
	}
}

// Only the Bedrock surfaces vary by region; a template without the placeholder
// must come back untouched rather than silently absorbing the value.
func TestRegionAppliesOnlyWhereTheTemplateAsksForIt(t *testing.T) {
	if got := ResolveBaseURL(ProfileBedrockConverseText, "eu-central-1"); got != "https://bedrock-runtime.eu-central-1.amazonaws.com" {
		t.Errorf("bedrock endpoint did not take the region: %q", got)
	}
	if got := ResolveBaseURL(ProfileBedrockMantleOpenAIChat, "ap-northeast-1"); got != "https://bedrock-mantle.ap-northeast-1.api.aws" {
		t.Errorf("mantle endpoint did not take the region: %q", got)
	}
	if got := ResolveBaseURL(ProfileOpenAIChatEmbeddings, "eu-central-1"); got != "https://api.openai.com" {
		t.Errorf("a region-free endpoint was rewritten: %q", got)
	}
	if got := ResolveBaseURL("no.such.profile.v1", "us-east-1"); got != "" {
		t.Errorf("an unregistered profile produced an endpoint: %q", got)
	}
	for _, profile := range AllProviderProfiles() {
		if strings.Contains(ResolveBaseURL(profile.ID, "us-east-1"), RegionPlaceholder) {
			t.Errorf("%s still carries the placeholder after resolution", profile.ID)
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
