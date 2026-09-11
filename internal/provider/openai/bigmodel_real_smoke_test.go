package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// TestBigModelRealSmoke is deliberately absent from ordinary test traffic. It
// enumerates first, then makes bounded billable calls only when an operator has
// explicitly supplied a dedicated key and model through the environment.
// Results and credentials are never printed.
func TestBigModelRealSmoke(t *testing.T) {
	if os.Getenv("HALRO_BIGMODEL_SMOKE") != "1" {
		t.Skip("set HALRO_BIGMODEL_SMOKE=1 with BASE_URL, API_KEY and MODEL to run the opt-in real-provider smoke")
	}
	base := os.Getenv("HALRO_BIGMODEL_BASE_URL")
	key := os.Getenv("HALRO_BIGMODEL_API_KEY")
	model := os.Getenv("HALRO_BIGMODEL_MODEL")
	if base == "" || key == "" || model == "" {
		t.Fatal("HALRO_BIGMODEL_BASE_URL, HALRO_BIGMODEL_API_KEY and HALRO_BIGMODEL_MODEL are required")
	}
	endpoint, err := url.Parse(base)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		t.Fatal("HALRO_BIGMODEL_BASE_URL must be an absolute HTTPS URL")
	}
	authorizer, err := provider.NewStaticHeaderAuthorizer(
		domain.CredentialBigModelAPIKey, "Authorization", "Bearer ", []byte(key), "api-key", "x-api-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "api/paas/v4"
	profileID := domain.ProfileBigModelGlobalChat
	if endpoint.Hostname() == "open.bigmodel.cn" {
		profileID = domain.ProfileBigModelCNChatEmbeddings
	}
	capabilities := provider.Capabilities{Chat: true, Streaming: true, StreamUsage: true}
	embeddingModel := os.Getenv("HALRO_BIGMODEL_EMBEDDING_MODEL")
	capabilities.Embeddings = embeddingModel != ""
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: &http.Client{Timeout: 45 * time.Second},
		ProviderType: string(domain.ProviderBigModel), CredentialScheme: domain.CredentialBigModelAPIKey,
		ProfileID:    profileID,
		Capabilities: capabilities, OperationPathPrefix: prefix, CatalogPathPrefix: &prefix, DisableTargetDescribe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	targets, err := adapter.ListInvocationTargets(ctx, domain.TargetQuery{})
	if err != nil {
		t.Fatal("model enumeration did not succeed")
	}
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		ids = append(ids, target.TargetID)
	}
	if !slices.Contains(ids, model) {
		t.Fatal("the configured chat model was not returned by this account's model list")
	}

	limit := int64(32)
	call := provider.ChatCall{RequestID: "bigmodel-real-smoke", ProviderModel: model, Request: openaiapi.ChatCompletionRequest{
		Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply with OK.")}}, MaxCompletionTokens: &limit,
	}}
	response, err := adapter.Chat(ctx, call)
	if err != nil || len(response.Choices) == 0 || response.Choices[0].Message == nil || response.Usage == nil {
		t.Fatal("bounded unary chat did not return a complete response with usage")
	}
	terminated := false
	usage, err := adapter.ChatStream(ctx, call, func(event semantic.Event) error {
		for _, output := range event.Outputs {
			terminated = terminated || output.Termination != ""
		}
		return nil
	})
	if err != nil || !terminated || usage == nil {
		t.Fatal("bounded stream did not terminate with usage")
	}

	if embeddingModel != "" {
		if !slices.Contains(ids, embeddingModel) {
			t.Fatal("the configured embedding model was not returned by this account's model list")
		}
		embedding, err := adapter.Embed(ctx, provider.EmbeddingCall{RequestID: "bigmodel-real-smoke-embedding", ProviderModel: embeddingModel,
			Request: openaiapi.EmbeddingRequest{Input: json.RawMessage(`"halro"`)}})
		if err != nil || len(embedding.Data) == 0 || embedding.Usage == nil {
			t.Fatal("embedding did not return a vector with usage")
		}
	}
}
