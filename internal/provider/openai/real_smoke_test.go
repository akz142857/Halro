package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/semantic"
)

// TestRealProviderSmoke is intentionally inert unless the operator explicitly
// opts in. It never logs credentials, request content, or provider responses.
//
// Required:
//
//	HEIMDALL_REAL_PROVIDER_SMOKE=1
//	HEIMDALL_SMOKE_PROFILE=openai|azure_openai|deepseek|openai_compatible
//	HEIMDALL_SMOKE_BASE_URL=...
//	HEIMDALL_SMOKE_API_KEY=...
//	HEIMDALL_SMOKE_MODEL=...
//
// Azure also requires HEIMDALL_SMOKE_API_VERSION. Embeddings run only when
// HEIMDALL_SMOKE_EMBEDDING_MODEL is set.
func TestRealProviderSmoke(t *testing.T) {
	if os.Getenv("HEIMDALL_REAL_PROVIDER_SMOKE") != "1" {
		t.Skip("real provider smoke is opt-in")
	}
	profile := os.Getenv("HEIMDALL_SMOKE_PROFILE")
	baseURL := os.Getenv("HEIMDALL_SMOKE_BASE_URL")
	apiKey := os.Getenv("HEIMDALL_SMOKE_API_KEY")
	model := os.Getenv("HEIMDALL_SMOKE_MODEL")
	if profile == "" || baseURL == "" || apiKey == "" || model == "" {
		t.Fatal("real smoke requires profile, base URL, API key, and model")
	}
	switch profile {
	case "openai", "azure_openai", "deepseek", "openai_compatible":
	default:
		t.Fatal("unsupported smoke profile")
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		t.Fatal("smoke base URL must be an absolute HTTPS URL")
	}
	capabilities := provider.Capabilities{
		Chat: true, Streaming: true, Embeddings: true, StreamUsage: true,
	}
	if profile == "deepseek" {
		capabilities.Embeddings = false
	}
	if profile == "openai_compatible" {
		capabilities.StreamUsage = false
	}
	client := &http.Client{Timeout: 45 * time.Second}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte(apiKey), Client: client,
		ProviderType: profile, APIVersion: os.Getenv("HEIMDALL_SMOKE_API_VERSION"),
		Azure: profile == "azure_openai", Capabilities: capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	request := openaiapi.ChatCompletionRequest{
		Model: model,
		Messages: []openaiapi.Message{{
			Role: "user", Content: openaiapi.TextContent("Reply with OK."),
		}},
		MaxTokens: int64Pointer(8),
	}
	response, err := adapter.Chat(ctx, provider.ChatCall{
		RequestID: "smoke_nonstream", ProviderModel: model, Request: request,
	})
	if err != nil {
		t.Fatalf("non-stream chat failed: %s", smokeErrorClass(err))
	}
	if response.ID == "" || len(response.Choices) == 0 {
		t.Fatal("non-stream chat returned an incomplete envelope")
	}

	streamRequest := request
	streamRequest.Stream = true
	chunks := 0
	_, err = adapter.ChatStream(ctx, provider.ChatCall{
		RequestID: "smoke_stream", ProviderModel: model, Request: streamRequest,
	}, func(chunk semantic.Event) error {
		if len(chunk.Choices) > 0 {
			chunks++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat failed: %s", smokeErrorClass(err))
	}
	if chunks == 0 {
		t.Fatal("stream chat returned no semantic chunks")
	}

	embeddingModel := os.Getenv("HEIMDALL_SMOKE_EMBEDDING_MODEL")
	if embeddingModel == "" {
		return
	}
	if !capabilities.Embeddings {
		t.Fatal("embedding model was configured for a profile without embeddings")
	}
	embedding, err := adapter.Embed(ctx, provider.EmbeddingCall{
		RequestID: "smoke_embedding", ProviderModel: embeddingModel,
		Request: openaiapi.EmbeddingRequest{
			Model: embeddingModel, Input: json.RawMessage(`["smoke"]`),
		},
	})
	if err != nil {
		t.Fatalf("embedding failed: %s", smokeErrorClass(err))
	}
	if len(embedding.Data) == 0 {
		t.Fatal("embedding returned no vectors")
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func smokeErrorClass(err error) string {
	var classified *provider.Error
	if errorsAsProvider(err, &classified) {
		return string(classified.Class)
	}
	return "unknown"
}

// Kept local so failure messages cannot accidentally format a wrapped error
// containing a provider response body.
func errorsAsProvider(err error, target **provider.Error) bool {
	for err != nil {
		if classified, ok := err.(*provider.Error); ok {
			*target = classified
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
