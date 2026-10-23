package compatibility

import (
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestAnthropicSamplingIsJudgedByTheExactClaudeModel(t *testing.T) {
	type members struct{ temperature, topP, topK bool }
	for _, test := range []struct {
		name    string
		profile domain.ProviderProfileID
		model   string
		set     members
		want    []string
	}{
		{"nothing set asks nothing", domain.ProfileAnthropicMessages, "claude-opus-5-5", members{}, nil},
		{"Claude 3 accepts all three", domain.ProfileAnthropicMessages, "claude-3-5-sonnet-20241022", members{true, true, true}, nil},
		{"Claude 4 accepts temperature alone", domain.ProfileAnthropicMessages, "claude-sonnet-4-6", members{true, false, true}, nil},
		{"Claude 4 accepts top_p alone", domain.ProfileAnthropicMessages, "claude-haiku-4-5-20251001", members{false, true, false}, nil},
		{"Claude 4 refuses the pair", domain.ProfileAnthropicMessages, "claude-opus-4-6", members{true, true, false}, []string{"temperature", "top_p"}},
		{"undated Opus 4 is the dated one", domain.ProfileAnthropicMessages, "claude-opus-4-20250514", members{true, false, false}, nil},
		{"Opus 4.7 refuses every member", domain.ProfileAnthropicMessages, "claude-opus-4-7", members{true, true, true}, []string{"temperature", "top_p", "top_k"}},
		{"Opus 4.7 is not Opus 4", domain.ProfileAnthropicMessages, "claude-opus-4-7", members{true, false, false}, []string{"temperature"}},
		{"Opus 5.5 refuses temperature", domain.ProfileAnthropicMessages, "claude-opus-5-5", members{true, false, false}, []string{"temperature"}},
		{"Sonnet 5.5 refuses top_k", domain.ProfileAnthropicMessages, "claude-sonnet-5-5", members{false, false, true}, []string{"top_k"}},
		{"Fable refuses top_p", domain.ProfileAnthropicMessages, "claude-fable-5-1", members{false, true, false}, []string{"top_p"}},
		{"an unknown model fails closed", domain.ProfileAnthropicMessages, "claude-next", members{true, false, false}, []string{"temperature"}},
		{"subscription is the same wire", domain.ProfileAnthropicSubscriptionMessages, "claude-opus-4-8", members{true, false, false}, []string{"temperature"}},
		{"Mantle prefix and version tag come off", domain.ProfileBedrockMantleAnthropicMessages, "anthropic.claude-sonnet-4-5-20250929-v1:0", members{true, false, false}, nil},
		{"cross-region prefix comes off", domain.ProfileBedrockMantleAnthropicMessages, "us.anthropic.claude-haiku-4-5", members{true, true, false}, []string{"temperature", "top_p"}},
		{"Mantle Opus 5.5 refuses", domain.ProfileBedrockMantleAnthropicMessages, "anthropic.claude-opus-5-5", members{true, false, false}, []string{"temperature"}},
		{"Bedrock context variant comes off", domain.ProfileBedrockMantleAnthropicMessages, "anthropic.claude-sonnet-4-5-20250929-v1:0:200k", members{true, false, false}, nil},
		{"Haiku 5.5 refuses a member with no value", domain.ProfileAnthropicMessages, "claude-haiku-5-5", members{true, false, false}, []string{"temperature"}},
		{"other profiles are not judged here", domain.ProfileOpenAIChatEmbeddings, "claude-opus-5-5", members{true, true, true}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := AnthropicSamplingUnsupportedFields(test.profile, test.model, AnthropicSampling{Temperature: test.set.temperature, TopP: test.set.topP, TopK: test.set.topK})
			if !slices.Equal(got, test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

// The portable path reaches the rule through target filtering; this holds the
// two together so a refactor of either cannot leave routing blind to it.
func TestUnsupportedGenerateFieldsForTargetCarriesAnthropicSampling(t *testing.T) {
	temperature := 0.2
	request := semantic.GenerateRequest{Operation: semantic.OperationGenerate, Temperature: &temperature}
	if got := UnsupportedGenerateFieldsForTarget(domain.ProfileAnthropicMessages, "claude-opus-4-7", request); !slices.Contains(got, "temperature") {
		t.Fatalf("Opus 4.7 target admitted temperature: %v", got)
	}
	if got := UnsupportedGenerateFieldsForTarget(domain.ProfileAnthropicMessages, "claude-sonnet-4-6", request); slices.Contains(got, "temperature") {
		t.Fatalf("Sonnet 4.6 target refused temperature: %v", got)
	}
}

// Haiku 5.5 is the one model that refuses sampling but states its defaults:
// temperature 1 or top_p 0.99 is accepted, one at a time, and top_k never is.
func TestHaiku55AcceptsOnlyItsStatedDefaults(t *testing.T) {
	value := func(v float64) *float64 { return &v }
	for _, test := range []struct {
		name     string
		sampling AnthropicSampling
		want     []string
	}{
		{"temperature 1", AnthropicSampling{Temperature: true, TemperatureValue: value(1)}, nil},
		{"top_p 0.99", AnthropicSampling{TopP: true, TopPValue: value(0.99)}, nil},
		{"temperature 0", AnthropicSampling{Temperature: true, TemperatureValue: value(0)}, []string{"temperature"}},
		{"top_p 1", AnthropicSampling{TopP: true, TopPValue: value(1)}, []string{"top_p"}},
		{"both defaults together", AnthropicSampling{Temperature: true, TemperatureValue: value(1), TopP: true, TopPValue: value(0.99)}, []string{"temperature", "top_p"}},
		{"top_k", AnthropicSampling{TopK: true}, []string{"top_k"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := AnthropicSamplingUnsupportedFields(domain.ProfileAnthropicMessages, "claude-haiku-5-5", test.sampling)
			if !slices.Equal(got, test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
	// The portable path carries the value through to the same rule.
	request := semantic.GenerateRequest{Operation: semantic.OperationGenerate, Temperature: value(1)}
	if got := UnsupportedGenerateFieldsForTarget(domain.ProfileAnthropicMessages, "claude-haiku-5-5", request); slices.Contains(got, "temperature") {
		t.Fatalf("Haiku 5.5 refused its own default temperature: %v", got)
	}
}
