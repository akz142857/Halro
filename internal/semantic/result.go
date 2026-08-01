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

type Usage struct {
	InputTokens     int64       `json:"input_tokens"`
	OutputTokens    int64       `json:"output_tokens"`
	ReasoningTokens int64       `json:"reasoning_tokens,omitempty"`
	AudioTokens     int64       `json:"audio_tokens,omitempty"`
	TotalTokens     int64       `json:"total_tokens"`
	Source          UsageSource `json:"source,omitempty"`
}

func (usage Usage) Validate() error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 || usage.AudioTokens < 0 || usage.TotalTokens < usage.InputTokens+usage.OutputTokens {
		return errors.New("semantic usage is invalid")
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
