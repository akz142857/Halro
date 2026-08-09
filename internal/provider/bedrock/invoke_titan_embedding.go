package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

const (
	titanEmbedV2ModelID       = "amazon.titan-embed-text-v2:0"
	titanEmbedV2MaxTokens     = int64(8192)
	titanEmbedV2MaxCharacters = 50_000
)

// titanEmbedV2Request is deliberately model-family specific. Adding another
// InvokeModel family requires a new versioned schema and profile rather than a
// generic JSON escape hatch.
type titanEmbedV2Request struct {
	InputText      string   `json:"inputText"`
	Dimensions     *int64   `json:"dimensions,omitempty"`
	Normalize      bool     `json:"normalize"`
	EmbeddingTypes []string `json:"embeddingTypes"`
}

type titanEmbedV2Response struct {
	Embedding           []float64            `json:"embedding"`
	InputTextTokenCount *int64               `json:"inputTextTokenCount"`
	EmbeddingsByType    map[string][]float64 `json:"embeddingsByType"`
}

func (a *Adapter) embedTitanV2(ctx context.Context, call provider.EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	if err := ValidateProfileModel(a.profileID, call.ProviderModel); err != nil {
		return openaiapi.EmbeddingResponse{}, badRequest(err.Error(), nil)
	}
	request, err := translateTitanEmbedV2Request(call.Request)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	var result titanEmbedV2Response
	if _, err := a.postJSON(ctx, call.ProviderModel, "invoke", call.RequestID, request, &result); err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	return translateTitanEmbedV2Response(call.ProviderModel, request, result)
}

// pinnedProfile is a profile that accepts exactly one model. The label is the
// human name used in the refusal, so an operator reads which product the
// profile is for rather than a profile identifier.
type pinnedProfile struct {
	Label string
	Model string
	Owner string
}

// pinnedProfileModels is the single place the pins are written down. Model
// discovery answers from it and ValidateProfileModel enforces it, so the list an
// operator is offered and the list the adapter accepts cannot drift apart.
var pinnedProfileModels = map[domain.ProviderProfileID]pinnedProfile{
	domain.ProfileBedrockInvokeTitanEmbedV2:  {"Titan Text Embeddings V2", titanEmbedV2ModelID, "Amazon"},
	domain.ProfileBedrockInvokeTitanImageV2:  {"Titan Image Generator V2", "amazon.titan-image-generator-v2:0", "Amazon"},
	domain.ProfileBedrockAsyncNovaReel:       {"Nova Reel", "amazon.nova-reel-v1:0", "Amazon"},
	domain.ProfileBedrockAgentRerankCohere35: {"Cohere Rerank", "cohere.rerank-v3-5:0", "Cohere"},
}

func ValidateProfileModel(profileID domain.ProviderProfileID, model string) error {
	pinned, ok := pinnedProfileModels[profileID]
	if !ok || strings.TrimSpace(model) == pinned.Model {
		return nil
	}
	return errors.New(pinned.Label + " profile requires model " + pinned.Model)
}

func translateTitanEmbedV2Request(request openaiapi.EmbeddingRequest) (titanEmbedV2Request, error) {
	if request.EncodingFormat != "" && request.EncodingFormat != "float" {
		return titanEmbedV2Request{}, badRequest("Titan Text Embeddings V2 supports float encoding only", nil)
	}
	if request.User != "" {
		return titanEmbedV2Request{}, badRequest("Titan Text Embeddings V2 profile does not declare user", nil)
	}
	var input string
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return titanEmbedV2Request{}, badRequest("Titan Text Embeddings V2 requires one text input", err)
	}
	if !utf8.ValidString(input) || strings.TrimSpace(input) == "" {
		return titanEmbedV2Request{}, badRequest("Titan Text Embeddings V2 input must be non-empty UTF-8 text", nil)
	}
	if utf8.RuneCountInString(input) > titanEmbedV2MaxCharacters {
		return titanEmbedV2Request{}, badRequest("Titan Text Embeddings V2 input exceeds 50000 characters", nil)
	}
	if request.Dimensions != nil && !validTitanEmbedV2Dimensions(*request.Dimensions) {
		return titanEmbedV2Request{}, badRequest("Titan Text Embeddings V2 dimensions must be 256, 512, or 1024", nil)
	}
	return titanEmbedV2Request{
		InputText: input, Dimensions: request.Dimensions, Normalize: true,
		EmbeddingTypes: []string{"float"},
	}, nil
}

func translateTitanEmbedV2Response(model string, request titanEmbedV2Request, response titanEmbedV2Response) (openaiapi.EmbeddingResponse, error) {
	expectedDimensions := int64(1024)
	if request.Dimensions != nil {
		expectedDimensions = *request.Dimensions
	}
	floatEmbedding, ok := response.EmbeddingsByType["float"]
	if response.InputTextTokenCount == nil || *response.InputTextTokenCount <= 0 || *response.InputTextTokenCount > titanEmbedV2MaxTokens {
		return openaiapi.EmbeddingResponse{}, malformed("Titan Text Embeddings V2 response has invalid usage", nil)
	}
	if !ok || len(response.EmbeddingsByType) != 1 || int64(len(response.Embedding)) != expectedDimensions || !slices.Equal(response.Embedding, floatEmbedding) {
		return openaiapi.EmbeddingResponse{}, malformed("Titan Text Embeddings V2 response has an invalid float embedding", nil)
	}
	encoded, err := json.Marshal(response.Embedding)
	if err != nil {
		return openaiapi.EmbeddingResponse{}, malformed("encode Titan Text Embeddings V2 response", err)
	}
	usage := &openaiapi.Usage{
		PromptTokens: *response.InputTextTokenCount,
		TotalTokens:  *response.InputTextTokenCount,
	}
	return openaiapi.EmbeddingResponse{
		Object: "list", Model: model, Usage: usage,
		Data: []openaiapi.EmbeddingData{{Object: "embedding", Embedding: encoded, Index: 0}},
	}, nil
}

func validTitanEmbedV2Dimensions(value int64) bool {
	return value == 256 || value == 512 || value == 1024
}
