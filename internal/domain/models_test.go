package domain

import "testing"

// The allowlist is only ever read by the native Anthropic Messages path, which
// is a property of the profile, not the surface. Bedrock Mantle also carries
// OpenAI chat and responses profiles, and a token stored on one of those is the
// stored-but-never-sent setting this check exists to prevent.
func TestAnthropicBetasAreValidatedAgainstTheProfileNotTheSurface(t *testing.T) {
	for _, testCase := range []struct {
		profile ProviderProfileID
		allowed bool
	}{
		{ProfileAnthropicMessages, true},
		{ProfileBedrockMantleAnthropicMessages, true},
		{ProfileBedrockMantleOpenAIChat, false},
		{ProfileBedrockMantleOpenAIResponses, false},
		{ProfileOpenAIChatEmbeddings, false},
	} {
		if got := ProfileSendsAnthropicBetas(testCase.profile); got != testCase.allowed {
			t.Fatalf("%s: sends betas=%v, want %v", testCase.profile, got, testCase.allowed)
		}
	}
}

// Defaults are what a new connection starts with; the ceiling is what an
// operator may turn on. Collapsing them made every optional capability either
// always-on or unreachable.
func TestProviderExecutedToolsIsReachableButNotADefault(t *testing.T) {
	if DefaultProviderCapabilitiesForProfile(ProviderAnthropic, ProfileAnthropicMessages).ProviderExecutedTools {
		t.Fatal("upstream egress must not be on by default")
	}
	if !MaxProviderCapabilitiesForProfile(ProviderAnthropic, ProfileAnthropicMessages).ProviderExecutedTools {
		t.Fatal("the Anthropic profile must allow the operator to enable it")
	}
	// The Mantle ceiling is fixed by the build; widening it is a separate review.
	if MaxProviderCapabilitiesForProfile(ProviderBedrock, ProfileBedrockMantleAnthropicMessages).ProviderExecutedTools {
		t.Fatal("the Mantle ceiling was widened without a contract review")
	}
}
