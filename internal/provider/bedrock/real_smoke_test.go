package bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

// TestRealProviderSmoke is opt-in because it contacts AWS and can incur cost.
//
//	HALRO_REAL_PROVIDER_SMOKE=1
//	HALRO_SMOKE_PROFILE=bedrock
//	HALRO_SMOKE_BASE_URL=https://bedrock-runtime.us-east-1.amazonaws.com
//	HALRO_SMOKE_CREDENTIAL_JSON='{"access_key_id":"...","secret_access_key":"...","region":"us-east-1"}'
//	HALRO_SMOKE_MODEL=anthropic.claude-...
//	HALRO_SMOKE_OPERATION=chat|embeddings
func TestRealProviderSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "bedrock" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=bedrock")
	}
	endpoint, err := url.Parse(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if err != nil || endpoint.Host == "" {
		t.Fatal("HALRO_SMOKE_BASE_URL must be a valid URL")
	}
	model := os.Getenv("HALRO_SMOKE_MODEL")
	if model == "" {
		t.Fatal("HALRO_SMOKE_MODEL is required")
	}
	profileID := domain.ProfileBedrockConverseText
	if os.Getenv("HALRO_SMOKE_OPERATION") == "embeddings" {
		profileID = domain.ProfileBedrockInvokeTitanEmbedV2
	}
	adapter, err := New(Options{
		Endpoint:       endpoint,
		CredentialJSON: []byte(os.Getenv("HALRO_SMOKE_CREDENTIAL_JSON")),
		Client:         &http.Client{Timeout: 30 * time.Second},
		ProfileID:      profileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if profileID == domain.ProfileBedrockInvokeTitanEmbedV2 {
		response, embedErr := adapter.Embed(ctx, provider.EmbeddingCall{
			RequestID: "bedrock-real-smoke", ProviderModel: model,
			Request: openaiapi.EmbeddingRequest{Input: json.RawMessage(`"Halro Bedrock smoke test"`)},
		})
		if embedErr != nil {
			t.Fatal(embedErr)
		}
		if len(response.Data) != 1 || len(response.Data[0].Embedding) == 0 || response.Usage == nil {
			t.Fatalf("Bedrock returned no embedding or usage: %#v", response)
		}
		return
	}
	response, err := adapter.Chat(ctx, provider.ChatCall{
		RequestID: "bedrock-real-smoke", ProviderModel: model,
		Request: openaiapi.ChatCompletionRequest{
			Messages:            []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply with OK.")}},
			MaxCompletionTokens: pointer(int64(8)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) == 0 || response.Choices[0].Message == nil {
		t.Fatalf("Bedrock returned no assistant choice: %#v", response)
	}
}

func pointer[T any](value T) *T { return &value }
