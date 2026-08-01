package gemini

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

// TestRealProviderSmoke is opt-in because it contacts Gemini and can incur
// cost. It logs only stable error classes, never credentials or response bodies.
//
//	HEIMDALL_REAL_PROVIDER_SMOKE=1
//	HEIMDALL_SMOKE_PROFILE=gemini
//	HEIMDALL_SMOKE_BASE_URL=https://generativelanguage.googleapis.com
//	HEIMDALL_SMOKE_API_KEY=...
//	HEIMDALL_SMOKE_MODEL=gemini-...
//
// Embeddings run only when HEIMDALL_SMOKE_EMBEDDING_MODEL is set.
func TestRealProviderSmoke(t *testing.T) {
	if os.Getenv("HEIMDALL_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HEIMDALL_SMOKE_PROFILE") != "gemini" {
		t.Skip("set HEIMDALL_REAL_PROVIDER_SMOKE=1 and HEIMDALL_SMOKE_PROFILE=gemini")
	}
	endpoint, err := url.Parse(os.Getenv("HEIMDALL_SMOKE_BASE_URL"))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		t.Fatal("HEIMDALL_SMOKE_BASE_URL must be an absolute HTTPS URL")
	}
	apiKey := os.Getenv("HEIMDALL_SMOKE_API_KEY")
	model := os.Getenv("HEIMDALL_SMOKE_MODEL")
	if apiKey == "" || model == "" {
		t.Fatal("HEIMDALL_SMOKE_API_KEY and HEIMDALL_SMOKE_MODEL are required")
	}
	adapter, err := New(Options{
		Endpoint: endpoint,
		APIKey:   []byte(apiKey),
		Client:   &http.Client{Timeout: 45 * time.Second},
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
		MaxTokens: geminiInt64Pointer(8),
	}
	response, err := adapter.Chat(ctx, provider.ChatCall{
		RequestID: "gemini_smoke_nonstream", ProviderModel: model, Request: request,
	})
	if err != nil {
		t.Fatalf("non-stream chat failed: %s", geminiSmokeErrorClass(err))
	}
	if response.ID == "" || len(response.Choices) == 0 {
		t.Fatal("non-stream chat returned an incomplete envelope")
	}
	request.Stream = true
	chunks := 0
	_, err = adapter.ChatStream(ctx, provider.ChatCall{
		RequestID: "gemini_smoke_stream", ProviderModel: model, Request: request,
	}, func(event semantic.Event) error {
		if len(event.Outputs) > 0 {
			chunks++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat failed: %s", geminiSmokeErrorClass(err))
	}
	if chunks == 0 {
		t.Fatal("stream chat returned no semantic chunks")
	}
	embeddingModel := os.Getenv("HEIMDALL_SMOKE_EMBEDDING_MODEL")
	if embeddingModel == "" {
		return
	}
	embedding, err := adapter.Embed(ctx, provider.EmbeddingCall{
		RequestID: "gemini_smoke_embedding", ProviderModel: embeddingModel,
		Request: openaiapi.EmbeddingRequest{
			Model: embeddingModel, Input: json.RawMessage(`["smoke"]`),
		},
	})
	if err != nil {
		t.Fatalf("embedding failed: %s", geminiSmokeErrorClass(err))
	}
	if len(embedding.Data) == 0 {
		t.Fatal("embedding returned no vectors")
	}
}

func geminiInt64Pointer(value int64) *int64 { return &value }

func geminiSmokeErrorClass(err error) string {
	for err != nil {
		if classified, ok := err.(*provider.Error); ok {
			return string(classified.Class)
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = unwrapper.Unwrap()
	}
	return "unknown"
}
