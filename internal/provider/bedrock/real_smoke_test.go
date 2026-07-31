package bedrock

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
)

// TestRealProviderSmoke is opt-in because it contacts AWS and can incur cost.
//
//	HEIMDALL_REAL_PROVIDER_SMOKE=1
//	HEIMDALL_SMOKE_PROFILE=bedrock
//	HEIMDALL_SMOKE_BASE_URL=https://bedrock-runtime.us-east-1.amazonaws.com
//	HEIMDALL_SMOKE_CREDENTIAL_JSON='{"access_key_id":"...","secret_access_key":"...","region":"us-east-1"}'
//	HEIMDALL_SMOKE_MODEL=anthropic.claude-...
func TestRealProviderSmoke(t *testing.T) {
	if os.Getenv("HEIMDALL_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HEIMDALL_SMOKE_PROFILE") != "bedrock" {
		t.Skip("set HEIMDALL_REAL_PROVIDER_SMOKE=1 and HEIMDALL_SMOKE_PROFILE=bedrock")
	}
	endpoint, err := url.Parse(os.Getenv("HEIMDALL_SMOKE_BASE_URL"))
	if err != nil || endpoint.Host == "" {
		t.Fatal("HEIMDALL_SMOKE_BASE_URL must be a valid URL")
	}
	model := os.Getenv("HEIMDALL_SMOKE_MODEL")
	if model == "" {
		t.Fatal("HEIMDALL_SMOKE_MODEL is required")
	}
	adapter, err := New(Options{
		Endpoint:       endpoint,
		CredentialJSON: []byte(os.Getenv("HEIMDALL_SMOKE_CREDENTIAL_JSON")),
		Client:         &http.Client{Timeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
