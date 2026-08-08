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

func ValidateProfileModel(profileID domain.ProviderProfileID, model string) error {
	if profileID == domain.ProfileBedrockInvokeTitanEmbedV2 && strings.TrimSpace(model) != titanEmbedV2ModelID {
		return errors.New("Titan Text Embeddings V2 profile requires model amazon.titan-embed-text-v2:0")
	}
	if profileID == domain.ProfileBedrockInvokeTitanImageV2 && strings.TrimSpace(model) != "amazon.titan-image-generator-v2:0" {
		return errors.New("Titan Image Generator V2 profile requires model amazon.titan-image-generator-v2:0")
	}
	if profileID == domain.ProfileBedrockAsyncNovaReel && strings.TrimSpace(model) != "amazon.nova-reel-v1:0" {
		return errors.New("Nova Reel profile requires model amazon.nova-reel-v1:0")
	}
	if profileID == domain.ProfileBedrockAgentRerankCohere35 && strings.TrimSpace(model) != "cohere.rerank-v3-5:0" {
		return errors.New("Cohere Rerank profile requires model cohere.rerank-v3-5:0")
	}
	return nil
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
