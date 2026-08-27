package bedrockmantle

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	anthropicprovider "github.com/akz142857/Halro/internal/provider/anthropic"
	openaiprovider "github.com/akz142857/Halro/internal/provider/openai"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// TestRealProviderSmoke contacts a real Amazon Bedrock Mantle account and costs
// money. It is inert unless every variable below is set deliberately.
//
//	HALRO_REAL_PROVIDER_SMOKE=1
//	HALRO_SMOKE_PROFILE=bedrock_mantle
//	HALRO_SMOKE_BASE_URL=https://bedrock-mantle.<region>.api.aws
//	HALRO_SMOKE_API_KEY=<Bedrock API key>
//	HALRO_SMOKE_MODEL=<exact upstream model id>
//	HALRO_SMOKE_MANTLE_PROFILE=chat|openai-chat|responses|openai-responses|messages
//
// The chat and responses values name the default /v1 route; the openai-
// prefixed ones name the second /openai/v1 route. A model reaches exactly one
// of the two, so pairing HALRO_SMOKE_BEDROCK_MODEL with the wrong value here
// is refused upstream rather than silently served.
//
//	HALRO_SMOKE_BEDROCK_PROJECT_ID=<proj_...>   (optional; empty means default)
//
// One run proves one cell of the matrix in docs/verification/provider-real-matrix.md:
// commit × region × profile × exact model × authentication × project mode. It
// says nothing about the other two wire profiles, another model, or another
// region, and the runner records it that way.
func TestRealProviderSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "bedrock_mantle" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=bedrock_mantle")
	}
	endpoint, err := url.Parse(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if err != nil {
		t.Fatal("HALRO_SMOKE_BASE_URL must be a valid URL")
	}
	// The same endpoint rule the Admin API applies, so a smoke cannot pass
	// against a host the product would refuse to store.
	if err := ValidateEndpoint(endpoint); err != nil {
		t.Fatalf("HALRO_SMOKE_BASE_URL is not an accepted Mantle endpoint: %v", err)
	}
	apiKey := os.Getenv("HALRO_SMOKE_API_KEY")
	model := os.Getenv("HALRO_SMOKE_MODEL")
	if apiKey == "" || model == "" {
		t.Fatal("HALRO_SMOKE_API_KEY and HALRO_SMOKE_MODEL are required")
	}
	projectID := domain.NormalizeBedrockProjectID(os.Getenv("HALRO_SMOKE_BEDROCK_PROJECT_ID"))
	if err := domain.ValidateBedrockProjectID(projectID); err != nil {
		t.Fatalf("HALRO_SMOKE_BEDROCK_PROJECT_ID: %v", err)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	switch wire := os.Getenv("HALRO_SMOKE_MANTLE_PROFILE"); wire {
	case "chat", "openai-chat":
		chatProfile := domain.ProfileBedrockMantleChat
		if wire == "openai-chat" {
			chatProfile = domain.ProfileBedrockMantleOpenAIChat
		}
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte(apiKey), "api-key", "x-api-key")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := openaiprovider.NewWithOptions(openaiprovider.Options{
			Endpoint: endpoint, Authorizer: authorizer, Client: client,
			ProviderType:        string(domain.ProviderBedrock),
			CredentialScheme:    domain.CredentialBedrockAPIKey,
			BedrockProjectID:    projectID,
			Capabilities:        domainCapabilities(chatProfile),
			OperationPathPrefix: smokeMantlePrefix(wire),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer adapter.Close()
		smokeChatBoth(ctx, t, adapter, model)
	case "responses", "openai-responses":
		responsesProfile := domain.ProfileBedrockMantleResponses
		if wire == "openai-responses" {
			responsesProfile = domain.ProfileBedrockMantleOpenAIResponses
		}
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "Authorization", "Bearer ", []byte(apiKey), "api-key", "x-api-key")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := NewResponses(ResponsesOptions{
			Endpoint: endpoint, Authorizer: authorizer, Client: client,
			BedrockProjectID:    projectID,
			Capabilities:        domainCapabilities(responsesProfile),
			OperationPathPrefix: smokeMantlePrefix(wire),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer adapter.Close()
		smokeChatBoth(ctx, t, adapter, model)
	case "messages":
		authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "x-api-key", "", []byte(apiKey), "Authorization", "api-key")
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := anthropicprovider.New(anthropicprovider.Options{
			Endpoint: endpoint, Authorizer: authorizer, Client: client,
			ProviderType:     string(domain.ProviderBedrock),
			CredentialScheme: domain.CredentialBedrockAPIKey,
			MessagesPath:     "anthropic/v1/messages",
			BedrockProjectID: projectID,
			Capabilities:     domainCapabilities(domain.ProfileBedrockMantleAnthropicMessages),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer adapter.Close()
		smokeChatBoth(ctx, t, adapter, model)
	default:
		t.Fatalf("HALRO_SMOKE_MANTLE_PROFILE must be chat, openai-chat, responses, openai-responses, or messages (got %q)", wire)
	}
}

// smokeChatBoth exercises the two shapes every Mantle profile must serve. It
// asserts structure only: no response text is printed, because the runner
// captures this process's output into an evidence file.
func smokeChatBoth(ctx context.Context, t *testing.T, adapter provider.Adapter, model string) {
	t.Helper()
	chat, ok := adapter.(interface {
		Chat(context.Context, provider.ChatCall) (openaiapi.ChatCompletionResponse, error)
	})
	if !ok {
		t.Fatal("adapter cannot serve chat")
	}
	request := openaiapi.ChatCompletionRequest{
		Messages:            []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("Reply with OK.")}},
		MaxCompletionTokens: smokePointer(int64(8)),
	}
	response, err := chat.Chat(ctx, provider.ChatCall{RequestID: "mantle-real-smoke", ProviderModel: model, Request: request})
	if err != nil {
		t.Fatalf("non-stream chat: %v", err)
	}
	if len(response.Choices) == 0 || response.Choices[0].Message == nil {
		t.Fatal("non-stream chat returned no assistant choice")
	}
	if response.Usage == nil || response.Usage.TotalTokens == 0 {
		t.Fatal("non-stream chat reported no usage, so this run cannot be accounted")
	}

	streamer, ok := adapter.(interface {
		ChatStream(context.Context, provider.ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error)
	})
	if !ok {
		t.Fatal("adapter cannot serve streaming chat")
	}
	events := 0
	usage, err := streamer.ChatStream(ctx, provider.ChatCall{RequestID: "mantle-real-smoke-stream", ProviderModel: model, Request: request}, func(semantic.Event) error {
		events++
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	if events == 0 {
		t.Fatal("stream chat produced no events")
	}
	if usage == nil || usage.TotalTokens == 0 {
		t.Fatal("stream chat reported no usage, so this run cannot be accounted")
	}
}

// domainCapabilities gives the adapter exactly the profile's ceiling, so a
// smoke cannot accidentally exercise a capability the product refuses to
// declare.
func domainCapabilities(profileID domain.ProviderProfileID) provider.Capabilities {
	declared := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, profileID)
	return provider.Capabilities{
		Chat: declared.Chat, Streaming: declared.Streaming, Embeddings: declared.Embeddings,
		Tools: declared.Tools, Vision: declared.Vision,
		JSONObject: declared.JSONObject, StructuredOutputs: declared.StructuredOutputs,
		DeveloperRole: declared.DeveloperRole, Reasoning: declared.Reasoning,
		StreamUsage: declared.StreamUsage,
		Moderations: declared.Moderations, Images: declared.Images,
		Transcriptions: declared.Transcriptions, Speech: declared.Speech,
		Files: declared.Files, Batches: declared.Batches, Rerank: declared.Rerank,
		AsyncGenerate:    declared.AsyncGenerate,
		MaxContextTokens: declared.MaxContextTokens, MaxOutputTokens: declared.MaxOutputTokens,
	}
}

func smokePointer[T any](value T) *T { return &value }

// smokeMantlePrefix maps the selector to the route the profile addresses. The
// empty string is the default /v1 join the adapters already do.
func smokeMantlePrefix(wire string) string {
	if strings.HasPrefix(wire, "openai-") {
		return "openai/v1"
	}
	return ""
}
