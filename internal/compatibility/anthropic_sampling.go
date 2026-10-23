package compatibility

import (
	"regexp"
	"slices"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
)

// anthropicSamplingProfiles are the profiles that put a request on Anthropic's
// own Messages wire, where the sampling members are judged by the exact Claude
// model named rather than by the profile.
var anthropicSamplingProfiles = []domain.ProviderProfileID{
	domain.ProfileAnthropicMessages,
	domain.ProfileAnthropicSubscriptionMessages,
	domain.ProfileBedrockMantleAnthropicMessages,
}

// claudeSamplingPairModels are the Claude 4 models that still accept a sampling
// member, but only one of temperature and top_p: naming both is a 400 from
// Claude 4 onward. Every later model refuses the members outright — Opus 4.7,
// 4.8, 5 and 5.5 and Fable at any value, Sonnet 5 and 5.5 and Haiku 5.5 at any
// value but the default.
//
// The list is of models known to accept, not of models known to refuse, and
// that direction is the point. Anthropic has removed the members from every
// release since Opus 4.7, so a model this table has not met is far more likely
// to refuse them than to honor them, and forwarding one turns into an upstream
// 400 after the budget is reserved, on a request that cannot fall back because
// a bad request is not retryable. A model missing here costs a caller one
// clear refusal before provider I/O instead.
//
// Source: Anthropic's model migration guide as bundled with the claude-api
// skill, reviewed 2026-10-23. Not yet confirmed against a real upstream.
var claudeSamplingPairModels = []string{
	"claude-opus-4", "claude-opus-4-0", "claude-opus-4-1", "claude-opus-4-5", "claude-opus-4-6",
	"claude-sonnet-4", "claude-sonnet-4-0", "claude-sonnet-4-5", "claude-sonnet-4-6",
	"claude-haiku-4-5",
}

// claudeSamplingDefaults are the models that refuse every sampling value but
// their own default, where Anthropic states what that default is. Haiku 5.5's
// are stated: temperature 1 or top_p 0.99, one of them, never top_k. Sonnet 5
// and 5.5 have the same exception with no published values, so they are not
// here and are refused like any other model since Opus 4.7 — a guessed default
// that is wrong is the post-reservation 400 this table exists to prevent.
var claudeSamplingDefaults = map[string]struct{ temperature, topP float64 }{
	"claude-haiku-5-5": {temperature: 1, topP: 0.99},
}

// claudeModelSuffix is what a provider model identifier may carry after its
// family name: a release date, and on Bedrock a version tag that can itself
// carry a context-window variant (`-v1:0`, `-v1:0:200k`).
var claudeModelSuffix = regexp.MustCompile(`(-\d{8})?(-v\d+(:[0-9a-z]+)*)?$`)

// normalizeClaudeModel reduces a provider model identifier to its family name:
// the Bedrock vendor prefix (and a cross-region one ahead of it) and the date
// and version suffixes come off, so `us.anthropic.claude-sonnet-4-5-20250929-v1:0`
// and `claude-sonnet-4-5` are judged alike.
func normalizeClaudeModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.Index(model, "anthropic."); index >= 0 {
		model = model[index+len("anthropic."):]
	}
	return claudeModelSuffix.ReplaceAllString(model, "")
}

// AnthropicSampling is which sampling members a request names. A member is
// named when its key is present, an explicit null included: native mode
// forwards the caller's bytes, and whether Anthropic reads a null as absent is
// not established. The values are kept only where they are numbers, for the
// models that accept their own defaults.
type AnthropicSampling struct {
	Temperature, TopP, TopK bool
	TemperatureValue        *float64
	TopPValue               *float64
}

func (sampling AnthropicSampling) any() bool {
	return sampling.Temperature || sampling.TopP || sampling.TopK
}

// AnthropicSamplingUnsupportedFields names the sampling members the exact model
// behind an Anthropic-wire target refuses. Other profiles answer nothing here;
// their sampling rules are per profile.
func AnthropicSamplingUnsupportedFields(profileID domain.ProviderProfileID, providerModel string, sampling AnthropicSampling) []string {
	if !slices.Contains(anthropicSamplingProfiles, profileID) || !sampling.any() {
		return nil
	}
	model := normalizeClaudeModel(providerModel)
	var fields []string
	add := func(condition bool, field string) {
		if condition {
			fields = append(fields, field)
		}
	}
	pair := sampling.Temperature && sampling.TopP
	defaults, hasDefaults := claudeSamplingDefaults[model]
	switch {
	case strings.HasPrefix(model, "claude-3-"):
		// The Claude 3 generation predates every sampling restriction.
	case slices.Contains(claudeSamplingPairModels, model):
		// Either member alone is accepted, so neither is singled out: the caller
		// chooses which to drop.
		add(pair, "temperature")
		add(pair, "top_p")
	case hasDefaults:
		add(sampling.Temperature && (pair || !equalValue(sampling.TemperatureValue, defaults.temperature)), "temperature")
		add(sampling.TopP && (pair || !equalValue(sampling.TopPValue, defaults.topP)), "top_p")
		add(sampling.TopK, "top_k")
	default:
		add(sampling.Temperature, "temperature")
		add(sampling.TopP, "top_p")
		add(sampling.TopK, "top_k")
	}
	return fields
}

func equalValue(value *float64, want float64) bool {
	return value != nil && *value == want
}
