package app

import (
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// Bedrock Mantle serves one host through three routes and a model reaches
// exactly one of them. Nothing in a request says which: /v1 and /openai/v1 both
// speak the OpenAI wire shape, and GET /v1/models/{id} answers with id, status,
// owned_by and data_retention and no route at all. The profile is therefore the
// only thing that can fix it.
//
// The pairing below is not derivable from the model identifier, which is why it
// is a table and not a prefix match. Measured against a real account on
// 2026-08-21 over all 50 models the account lists:
//
//   - openai.gpt-oss-20b and openai.gpt-oss-120b answer on /v1, while
//     openai.gpt-5.6-sol and every other openai.gpt-5.x answer on /openai/v1;
//   - google.gemma-3-* answer on /v1 and google.gemma-4-* on /openai/v1;
//   - xai.grok-4.3 answers on /openai/v1 despite sharing no vendor prefix with
//     anything else there.
//
// Each of those pairs would be classified wrongly by any rule that reads the
// model identifier.
func TestMantleOperationPathPrefixIsFixedByTheProfile(t *testing.T) {
	cases := []struct {
		profile domain.ProviderProfileID
		want    string
	}{
		{domain.ProfileBedrockMantleChat, ""},
		{domain.ProfileBedrockMantleResponses, ""},
		{domain.ProfileBedrockMantleOpenAIChat, "openai/v1"},
		{domain.ProfileBedrockMantleOpenAIResponses, "openai/v1"},
		// The Anthropic Messages profile does not go through this helper — its
		// adapter carries the whole anthropic/v1/messages path itself — so the
		// helper must not claim a route for it.
		{domain.ProfileBedrockMantleAnthropicMessages, ""},
		{domain.ProfileOpenAIChatEmbeddings, ""},
	}
	for _, test := range cases {
		if got := mantleOperationPathPrefix(test.profile); got != test.want {
			t.Errorf("%s prefix is %q, want %q", test.profile, got, test.want)
		}
	}
}

// Every Mantle profile has to be reachable through the surface helper, because
// the target-kind and endpoint branches now ask that question instead of
// spelling the list out. A profile missing here is a profile the console offers
// and the catalogue then classifies as a Bedrock foundation model.
func TestEveryMantleProfileIsRecognisedBySurface(t *testing.T) {
	mantle := []domain.ProviderProfileID{
		domain.ProfileBedrockMantleChat,
		domain.ProfileBedrockMantleOpenAIChat,
		domain.ProfileBedrockMantleResponses,
		domain.ProfileBedrockMantleOpenAIResponses,
		domain.ProfileBedrockMantleAnthropicMessages,
	}
	for _, profile := range mantle {
		if !domain.IsBedrockMantleProfile(profile) {
			t.Errorf("%s is not recognised as a Bedrock Mantle profile", profile)
		}
	}
	for _, profile := range []domain.ProviderProfileID{domain.ProfileBedrockConverseText, domain.ProfileOpenAIChatEmbeddings, "no.such.profile.v1"} {
		if domain.IsBedrockMantleProfile(profile) {
			t.Errorf("%s is wrongly recognised as a Bedrock Mantle profile", profile)
		}
	}
}
