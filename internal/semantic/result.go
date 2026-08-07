package semantic

import (
	"encoding/json"
	"errors"
)

type UsageSource string

const (
	UsageProviderReported UsageSource = "provider_reported"
	UsageLocallyEstimated UsageSource = "locally_estimated"
)

// Usage normalizes provider token reporting onto one convention, because the
// upstreams disagree about what their own prompt-token field means: Anthropic
// reports input_tokens *excluding* cache reads and writes, while OpenAI reports
// prompt_tokens *including* them. Recording either shape verbatim makes the same
// number mean different things per provider and mis-prices every cached request.
//
// The convention here: InputTokens is every prompt token the provider processed,
// whatever tier it was billed at. CachedInputTokens and CacheWriteInputTokens are
// breakdown *subsets* of InputTokens, never additions to it, so
// InputTokens - CachedInputTokens - CacheWriteInputTokens is the span billed at
// the ordinary input rate. Adapters translate into this shape; everything
// downstream may assume it.
//
// ReasoningTokens is likewise a subset of OutputTokens wherever the provider
// bills reasoning at the output rate, so it is reported for observability and
// must not be added to OutputTokens when pricing.
type Usage struct {
	InputTokens           int64       `json:"input_tokens"`
	CachedInputTokens     int64       `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens int64       `json:"cache_write_input_tokens,omitempty"`
	OutputTokens          int64       `json:"output_tokens"`
	ReasoningTokens       int64       `json:"reasoning_tokens,omitempty"`
	AudioTokens           int64       `json:"audio_tokens,omitempty"`
	TotalTokens           int64       `json:"total_tokens"`
	Source                UsageSource `json:"source,omitempty"`
}

// UncachedInputTokens is the prompt span billed at the ordinary input rate.
func (usage Usage) UncachedInputTokens() int64 {
	return usage.InputTokens - usage.CachedInputTokens - usage.CacheWriteInputTokens
}

func (usage Usage) Validate() error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 || usage.AudioTokens < 0 || usage.TotalTokens < usage.InputTokens+usage.OutputTokens {
		return errors.New("semantic usage is invalid")
	}
	if usage.CachedInputTokens < 0 || usage.CacheWriteInputTokens < 0 {
		return errors.New("semantic usage cache tokens are invalid")
	}
	// The cache tiers partition InputTokens. A sum that overruns it means an
	// adapter added the tiers on top instead of translating into the subset
	// convention, which would double-count the cached span when pricing.
	if usage.CachedInputTokens+usage.CacheWriteInputTokens > usage.InputTokens {
		return errors.New("semantic usage cache tokens exceed input tokens")
	}
	if usage.Source != UsageProviderReported && usage.Source != UsageLocallyEstimated {
		return errors.New("semantic usage source is invalid")
	}
	return nil
}

type TranslationLoss string

const (
	TranslationNone        TranslationLoss = "none"
	TranslationDeclared    TranslationLoss = "declared"
	TranslationUnsupported TranslationLoss = "unsupported"
)

type GenerateChoice struct {
	Index             int     `json:"index"`
	Message           Message `json:"message"`
	Termination       string  `json:"termination,omitempty"`
	NativeTermination string  `json:"native_termination,omitempty"`
}

type GenerateResult struct {
	ID                string           `json:"id"`
	Created           int64            `json:"created"`
	Model             string           `json:"model"`
	Choices           []GenerateChoice `json:"choices"`
	Usage             *Usage           `json:"usage,omitempty"`
	ProviderRequestID string           `json:"provider_request_id,omitempty"`
	ProviderID        string           `json:"provider_id,omitempty"`
	DeploymentID      string           `json:"deployment_id,omitempty"`
	Translation       TranslationLoss  `json:"translation"`
	MappingRevision   uint64           `json:"mapping_revision"`
}

func (result GenerateResult) Validate() error {
	if result.ID == "" || result.Model == "" || result.MappingRevision == 0 || result.Translation == "" || len(result.Choices) == 0 {
		return errors.New("semantic generate result is invalid")
	}
	if result.Translation != TranslationNone && result.Translation != TranslationDeclared && result.Translation != TranslationUnsupported {
		return errors.New("semantic translation loss is invalid")
	}
	for _, choice := range result.Choices {
		if choice.Index < 0 {
			return errors.New("semantic generate choice index is invalid")
		}
		if err := choice.Message.Validate(); err != nil {
			return err
		}
	}
	if result.Usage != nil {
		return result.Usage.Validate()
	}
	return nil
}

type Embedding struct {
	Index int             `json:"index"`
	Value json.RawMessage `json:"value"`
}

type EmbeddingResult struct {
	Model             string          `json:"model"`
	Data              []Embedding     `json:"data"`
	Usage             *Usage          `json:"usage,omitempty"`
	ProviderRequestID string          `json:"provider_request_id,omitempty"`
	ProviderID        string          `json:"provider_id,omitempty"`
	DeploymentID      string          `json:"deployment_id,omitempty"`
	Translation       TranslationLoss `json:"translation"`
	MappingRevision   uint64          `json:"mapping_revision"`
}

func (result EmbeddingResult) Validate() error {
	if result.Model == "" || result.MappingRevision == 0 || result.Translation == "" || len(result.Data) == 0 {
		return errors.New("semantic embedding result is invalid")
	}
	if result.Translation != TranslationNone && result.Translation != TranslationDeclared && result.Translation != TranslationUnsupported {
		return errors.New("semantic translation loss is invalid")
	}
	for _, item := range result.Data {
		if item.Index < 0 || !json.Valid(item.Value) {
			return errors.New("semantic embedding item is invalid")
		}
	}
	if result.Usage != nil {
		return result.Usage.Validate()
	}
	return nil
}
